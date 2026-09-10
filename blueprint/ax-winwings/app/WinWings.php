<?php

namespace Pterodactyl\BlueprintFramework\Extensions\{identifier};

use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Schema;

/**
 * Shared state for the Windows profile extension.
 *
 * Everything here is read on the hot path — the daemon asks for a profile every
 * time a server boots — so lookups are memoised for the life of the request and
 * the table-existence check is done once rather than per call. A panel that has
 * the extension's files but not its migrations must degrade to "no profiles"
 * rather than throwing, because the remote API is on the same middleware group
 * as the routes that keep existing servers alive.
 */
class WinWings
{
    public const TABLE_PROFILES = 'axwinwings_profiles';
    public const TABLE_NODES = 'axwinwings_nodes';
    public const TABLE_SETTINGS = 'axwinwings_settings';

    private static ?bool $ready = null;
    private static ?array $settings = null;
    private static array $profiles = [];
    private static array $windowsNodes = [];

    /**
     * Whether the extension's tables exist. Cached: Schema::hasTable is a real
     * query against information_schema and this is called on every daemon
     * request.
     */
    public static function ready(): bool
    {
        if (self::$ready === null) {
            self::$ready = Schema::hasTable(self::TABLE_PROFILES)
                && Schema::hasTable(self::TABLE_NODES)
                && Schema::hasTable(self::TABLE_SETTINGS);
        }

        return self::$ready;
    }

    /**
     * Forget everything memoised. Only useful inside the admin controller, where
     * a write and a read happen in the same request.
     */
    public static function flush(): void
    {
        self::$ready = null;
        self::$settings = null;
        self::$profiles = [];
        self::$windowsNodes = [];
    }

    // ---------------------------------------------------------------- settings

    public static function setting(string $key, ?string $default = null): ?string
    {
        if (!self::ready()) {
            return $default;
        }

        if (self::$settings === null) {
            self::$settings = DB::table(self::TABLE_SETTINGS)->pluck('value', 'key')->all();
        }

        return array_key_exists($key, self::$settings) ? self::$settings[$key] : $default;
    }

    public static function flag(string $key, bool $default = false): bool
    {
        $value = self::setting($key, $default ? '1' : '0');

        return $value === '1' || $value === 'true';
    }

    public static function putSetting(string $key, ?string $value): void
    {
        if (!self::ready()) {
            return;
        }

        DB::table(self::TABLE_SETTINGS)->updateOrInsert(
            ['key' => $key],
            ['value' => $value, 'updated_at' => now(), 'created_at' => now()]
        );

        self::$settings = null;
    }

    /**
     * The runtime names offered in the profile editor.
     *
     * These have to match the node's config.yml, and a typo there produces a
     * server that silently inherits the wrong Java rather than an error — which
     * is exactly why the editor offers a list instead of a text box.
     */
    public static function runtimes(): array
    {
        $raw = json_decode((string) self::setting('runtimes', '[]'), true);

        if (!is_array($raw)) {
            return [];
        }

        $out = [];
        foreach ($raw as $name) {
            $name = trim((string) $name);
            if ($name !== '' && !in_array($name, $out, true)) {
                $out[] = $name;
            }
        }

        sort($out, SORT_NATURAL | SORT_FLAG_CASE);

        return $out;
    }

    // ------------------------------------------------------------------- nodes

    public static function isWindowsNode(int $nodeId): bool
    {
        if (!self::ready() || $nodeId <= 0) {
            return false;
        }

        if (!array_key_exists($nodeId, self::$windowsNodes)) {
            self::$windowsNodes[$nodeId] = (bool) DB::table(self::TABLE_NODES)
                ->where('node_id', $nodeId)
                ->where('is_windows', true)
                ->exists();
        }

        return self::$windowsNodes[$nodeId];
    }

    /**
     * Record that a node just used the Windows API.
     *
     * Only win-wings calls those routes, so the call itself is the evidence that
     * this is a Windows node — that is what makes `auto_flag_nodes` safe. An
     * operator who has deliberately turned a node's flag off is not overridden:
     * the row is only created, never flipped back on.
     */
    public static function touchNode(int $nodeId, string $endpoint): void
    {
        if (!self::ready() || $nodeId <= 0) {
            return;
        }

        $existing = DB::table(self::TABLE_NODES)->where('node_id', $nodeId)->first();

        if ($existing) {
            DB::table(self::TABLE_NODES)->where('node_id', $nodeId)->update([
                'last_seen_at' => now(),
                'last_seen_endpoint' => $endpoint,
                'updated_at' => now(),
            ]);

            return;
        }

        if (!self::flag('auto_flag_nodes', true)) {
            return;
        }

        DB::table(self::TABLE_NODES)->insert([
            'node_id' => $nodeId,
            'is_windows' => true,
            'detected_at' => now(),
            'last_seen_at' => now(),
            'last_seen_endpoint' => $endpoint,
            'created_at' => now(),
            'updated_at' => now(),
        ]);

        unset(self::$windowsNodes[$nodeId]);
    }

    // ---------------------------------------------------------------- profiles

    /**
     * The enabled profile for an egg, or null.
     *
     * A disabled profile is deliberately indistinguishable from a missing one:
     * "off" has to mean the daemon refuses the server, not that it runs with a
     * half-configured profile.
     */
    public static function profileForEgg(int $eggId): ?object
    {
        if (!self::ready() || $eggId <= 0) {
            return null;
        }

        if (!array_key_exists($eggId, self::$profiles)) {
            self::$profiles[$eggId] = DB::table(self::TABLE_PROFILES)
                ->where('egg_id', $eggId)
                ->where('enabled', true)
                ->first();
        }

        return self::$profiles[$eggId];
    }

    /**
     * The profile row for an egg regardless of its enabled flag. For the admin
     * UI, which has to be able to see and edit a parked profile.
     */
    public static function rawProfileForEgg(int $eggId): ?object
    {
        if (!self::ready() || $eggId <= 0) {
            return null;
        }

        return DB::table(self::TABLE_PROFILES)->where('egg_id', $eggId)->first();
    }

    /**
     * The JSON body the daemon expects from
     * GET /api/remote/windows/servers/{uuid}/profile.
     *
     * `stop` is omitted rather than sent empty when the profile does not set one:
     * the daemon reads a missing stop as "use the egg's own configuration", and
     * an empty object would be read as a command stop with no command.
     */
    public static function profilePayload(object $profile): array
    {
        $payload = [
            'runtime' => (string) $profile->runtime,
            'startup' => (string) ($profile->startup ?? ''),
            'pseudo_console' => (bool) $profile->pseudo_console,
        ];

        if (!empty($profile->stop_type)) {
            $payload['stop'] = [
                'type' => (string) $profile->stop_type,
                'value' => (string) ($profile->stop_value ?? ''),
            ];
        }

        return $payload;
    }
}
