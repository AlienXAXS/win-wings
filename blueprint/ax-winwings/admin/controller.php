<?php

namespace Pterodactyl\Http\Controllers\Admin\Extensions\{identifier};

use Carbon\Carbon;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route as RouteFacade;
use Illuminate\Support\Facades\Schema;
use Illuminate\View\Factory as ViewFactory;
use Illuminate\View\View;
use Pterodactyl\BlueprintFramework\Libraries\ExtensionLibrary\Admin\BlueprintAdminLibrary as BlueprintExtensionLibrary;
use Pterodactyl\BlueprintFramework\Extensions\{identifier}\WinWings;
use Pterodactyl\Http\Controllers\Controller;
use Pterodactyl\Models\Nest;
use Pterodactyl\Models\Node;

class {identifier}ExtensionController extends Controller
{
    public function __construct(
        private ViewFactory $view,
        private BlueprintExtensionLibrary $blueprint,
    ) {}

    public function index(): View
    {
        $ready = Schema::hasTable(WinWings::TABLE_PROFILES)
            && Schema::hasTable(WinWings::TABLE_NODES)
            && Schema::hasTable(WinWings::TABLE_SETTINGS);

        return $this->view->make('admin.extensions.{identifier}.index', [
            'blueprint' => $this->blueprint,
            'root' => '/admin/extensions/{identifier}',
            'api' => '/extensions/{identifier}',
            'ready' => $ready,
            'diagnostics' => $this->diagnostics($ready),
            'settings' => $ready ? [
                'gate_enabled' => WinWings::flag('gate_enabled', true),
                'gate_message' => (string) WinWings::setting('gate_message', 'This egg is not available on Windows nodes.'),
                'auto_flag_nodes' => WinWings::flag('auto_flag_nodes', true),
                'override_install_endpoint' => WinWings::flag('override_install_endpoint', true),
            ] : null,
            'runtimes' => $ready ? WinWings::runtimes() : [],
            'nodes' => $ready ? $this->nodes() : collect([]),
            'nests' => $ready ? $this->nests() : collect([]),
            'profiles' => $ready
                ? DB::table(WinWings::TABLE_PROFILES)->get()->keyBy('egg_id')
                : collect([]),
        ]);
    }

    public function post(Request $request): JsonResponse
    {
        if (!Schema::hasTable(WinWings::TABLE_PROFILES)) {
            return $this->fail('Win-Wings tables are missing. Re-run the extension migrations.');
        }

        return match ($request->input('action')) {
            'save_settings' => $this->saveSettings($request),
            'save_runtimes' => $this->saveRuntimes($request),
            'set_node' => $this->setNode($request),
            'forget_node' => $this->forgetNode($request),
            'save_profile' => $this->saveProfile($request),
            'toggle_profile' => $this->toggleProfile($request),
            'delete_profile' => $this->deleteProfile($request),
            default => $this->fail('Unknown action.'),
        };
    }

    // -------------------------------------------------------------- diagnostics

    /**
     * What is actually wired up, as opposed to what is installed.
     *
     * Three separate things have to be true for a Windows node to work, and two
     * of them are patches to Panel files that a panel upgrade will silently
     * revert. Nothing else reports that; a node just starts refusing servers.
     */
    private function diagnostics(bool $ready): array
    {
        $routes = RouteFacade::getRoutes();

        $ping = false;
        $installOverride = false;

        foreach ($routes->getRoutes() as $route) {
            if ($route->uri() === 'api/remote/windows/ping') {
                $ping = true;
            }

            if ($route->uri() === 'api/remote/servers/{uuid}/install' && in_array('GET', $route->methods(), true)) {
                // The core route and ours share a URI; whichever was registered
                // last is the one in the collection, so the action name is the
                // only honest answer to "which one runs".
                $installOverride = str_contains((string) $route->getActionName(), 'axwinwings');
            }
        }

        $creation = base_path('app/Services/Servers/ServerCreationService.php');
        $gatePatched = is_readable($creation)
            && str_contains((string) file_get_contents($creation), 'axwinwings');

        return [
            'ready' => $ready,
            'ping' => $ping,
            'install_override' => $installOverride,
            'gate_patched' => $gatePatched,
            'profiles' => $ready ? DB::table(WinWings::TABLE_PROFILES)->count() : 0,
            'profiles_enabled' => $ready ? DB::table(WinWings::TABLE_PROFILES)->where('enabled', true)->count() : 0,
            'windows_nodes' => $ready ? DB::table(WinWings::TABLE_NODES)->where('is_windows', true)->count() : 0,
        ];
    }

    private function nodes()
    {
        $flags = DB::table(WinWings::TABLE_NODES)->get()->keyBy('node_id');

        return Node::query()
            ->orderBy('name')
            ->get(['id', 'name', 'fqdn', 'scheme'])
            ->map(function (Node $node) use ($flags) {
                $flag = $flags->get($node->id);

                return (object) [
                    'id' => $node->id,
                    'name' => $node->name,
                    'fqdn' => $node->fqdn,
                    'is_windows' => $flag ? (bool) $flag->is_windows : false,
                    'known' => (bool) $flag,
                    'detected_at' => $flag->detected_at ?? null,
                    'last_seen_at' => $flag->last_seen_at ?? null,
                    'last_seen_endpoint' => $flag->last_seen_endpoint ?? null,

                    // Formatted here rather than in the view: Blueprint strips
                    // backslashes out of a Blade file, so a fully qualified
                    // Carbon call in the template would not survive install.
                    'last_seen_human' => isset($flag->last_seen_at) && $flag->last_seen_at
                        ? Carbon::parse($flag->last_seen_at)->diffForHumans()
                        : null,
                ];
            });
    }

    private function nests()
    {
        return Nest::query()
            ->with(['eggs' => fn ($q) => $q->orderBy('name')])
            ->orderBy('name')
            ->get();
    }

    // ----------------------------------------------------------------- settings

    private function saveSettings(Request $request): JsonResponse
    {
        $message = trim((string) $request->input('gate_message', ''));

        WinWings::putSetting('gate_enabled', $request->boolean('gate_enabled') ? '1' : '0');
        WinWings::putSetting('auto_flag_nodes', $request->boolean('auto_flag_nodes') ? '1' : '0');
        WinWings::putSetting('override_install_endpoint', $request->boolean('override_install_endpoint') ? '1' : '0');
        WinWings::putSetting('gate_message', $message !== '' ? $message : 'This egg is not available on Windows nodes.');

        return $this->ok('Settings saved.');
    }

    /**
     * The runtime dropdown's contents.
     *
     * Free text, because the names are a convention between this panel and each
     * node's config.yml and nothing here can see that file. Validated only for
     * shape: a name with a space or a backslash in it is a pasted directory
     * path, which is the mistake worth catching.
     */
    private function saveRuntimes(Request $request): JsonResponse
    {
        $raw = (string) $request->input('runtimes', '');
        $names = preg_split('/[\r\n,]+/', $raw) ?: [];

        $out = [];
        foreach ($names as $name) {
            $name = trim($name);

            if ($name === '') {
                continue;
            }

            if (!preg_match('/^[A-Za-z0-9][A-Za-z0-9._-]*$/', $name)) {
                return $this->fail('"' . $name . '" is not a runtime name. Use the key from the node\'s runtime.runtimes map, such as java-21 — not the directory it points at.');
            }

            if (!in_array($name, $out, true)) {
                $out[] = $name;
            }
        }

        WinWings::putSetting('runtimes', json_encode($out));

        return $this->ok(count($out) . ' runtime name' . (count($out) === 1 ? '' : 's') . ' saved.', ['runtimes' => $out]);
    }

    // -------------------------------------------------------------------- nodes

    private function setNode(Request $request): JsonResponse
    {
        $nodeId = (int) $request->input('node_id');

        if (!Node::query()->whereKey($nodeId)->exists()) {
            return $this->fail('That node no longer exists.');
        }

        $windows = $request->boolean('is_windows');

        DB::table(WinWings::TABLE_NODES)->updateOrInsert(
            ['node_id' => $nodeId],
            ['is_windows' => $windows, 'updated_at' => now(), 'created_at' => now()]
        );

        WinWings::flush();

        return $this->ok($windows
            ? 'Node marked as a Windows node.'
            : 'Node no longer treated as a Windows node. Its servers will be served the standard Linux install script again.');
    }

    /**
     * Delete the row rather than clearing the flag, so a node that is genuinely
     * Windows can be re-detected on its next call instead of being stuck off.
     */
    private function forgetNode(Request $request): JsonResponse
    {
        DB::table(WinWings::TABLE_NODES)->where('node_id', (int) $request->input('node_id'))->delete();

        WinWings::flush();

        return $this->ok('Node forgotten. It will be re-detected the next time win-wings calls the Windows API.');
    }

    // ----------------------------------------------------------------- profiles

    private function saveProfile(Request $request): JsonResponse
    {
        $data = $request->validate([
            'egg_id' => 'required|integer|exists:eggs,id',
            'runtime' => 'nullable|string|max:191',
            'startup' => 'nullable|string|max:65535',
            'stop_type' => 'nullable|string|in:command,signal',
            'stop_value' => 'nullable|string|max:191',
            'pseudo_console' => 'nullable',
            'install_override' => 'nullable',
            'install_script' => 'nullable|string',
            'notes' => 'nullable|string|max:65535',
            'enabled' => 'nullable',
        ]);

        $stopType = $data['stop_type'] ?? null;
        $stopValue = trim((string) ($data['stop_value'] ?? ''));
        $pseudoConsole = $request->boolean('pseudo_console');

        // A command stop with no command is the one combination the daemon
        // cannot do anything sensible with: it would write an empty line to
        // stdin and then wait out the whole timeout before killing the process.
        if ($stopType === 'command' && $stopValue === '') {
            return $this->fail('A command stop needs the text to send — "stop", "end", "quit" and so on.');
        }

        if ($stopType === 'signal') {
            // Everything the daemon can actually deliver as a signal is one
            // interrupt. A value it does not recognise is not a milder failure
            // than an empty one: it falls through to CTRL_BREAK, which a
            // pipe-backed process never receives, so the server is killed. That
            // is the same silent outcome as saving nothing, and it looks
            // configured, so it is refused rather than stored.
            $canonical = self::canonicalInterrupt($stopValue);

            if ($canonical === null) {
                return $this->fail('The only signal Windows can deliver to a running server is a console interrupt. Choose Ctrl+C, or use a stop command instead — any other value reaches the daemon as unrecognised and the server is killed rather than asked to stop.');
            }

            // Refused rather than silently switched on: the pseudo console
            // changes how the server console behaves for everyone watching it
            // (VT escapes instead of clean lines), which is not a side effect to
            // apply on someone's behalf while they were editing the stop field.
            if (!$pseudoConsole) {
                return $this->fail('A Ctrl+C stop needs the pseudo console turned on for this egg. The interrupt is delivered by writing a byte to the server console input, and a server started on plain pipes has no console to receive it — the stop would silently degrade to a kill. Enable the pseudo console, or use a stop command, which needs no console.');
            }

            // Stored canonically. The daemon accepts several spellings, so what
            // is served stays the same either way, but a table holding one form
            // is a table you can query.
            $stopValue = $canonical;
        }

        $installOverride = $request->boolean('install_override');
        $script = (string) ($data['install_script'] ?? '');

        if ($installOverride && trim($script) === '') {
            return $this->fail('The install script is empty, so there is nothing to override the egg\'s script with.');
        }

        $row = [
            'runtime' => trim((string) ($data['runtime'] ?? '')),
            'startup' => $data['startup'] ?? null,
            'stop_type' => $stopType,
            'stop_value' => $stopType ? $stopValue : null,
            'pseudo_console' => $pseudoConsole,
            'install_override' => $installOverride,
            'install_script' => $script !== '' ? $script : null,
            'notes' => $data['notes'] ?? null,
            'enabled' => $request->boolean('enabled'),
            'updated_at' => now(),
        ];

        $existing = DB::table(WinWings::TABLE_PROFILES)->where('egg_id', $data['egg_id'])->first();

        if ($existing) {
            DB::table(WinWings::TABLE_PROFILES)->where('egg_id', $data['egg_id'])->update($row);
        } else {
            DB::table(WinWings::TABLE_PROFILES)->insert($row + [
                'egg_id' => $data['egg_id'],
                'created_at' => now(),
            ]);
        }

        WinWings::flush();

        $profile = WinWings::rawProfileForEgg((int) $data['egg_id']);

        return $this->ok('Profile saved.', [
            'egg_id' => (int) $data['egg_id'],
            'summary' => $this->summary($profile),
            'payload' => $profile && $profile->enabled ? WinWings::profilePayload($profile) : null,
        ]);
    }

    private function toggleProfile(Request $request): JsonResponse
    {
        $eggId = (int) $request->input('egg_id');
        $enabled = $request->boolean('enabled');

        $affected = DB::table(WinWings::TABLE_PROFILES)
            ->where('egg_id', $eggId)
            ->update(['enabled' => $enabled, 'updated_at' => now()]);

        if (!$affected) {
            return $this->fail('That egg has no profile to toggle.');
        }

        WinWings::flush();

        return $this->ok($enabled
            ? 'Profile enabled.'
            : 'Profile disabled. Windows nodes will now refuse servers using this egg.', [
                'egg_id' => $eggId,
                'summary' => $this->summary(WinWings::rawProfileForEgg($eggId)),
            ]);
    }

    private function deleteProfile(Request $request): JsonResponse
    {
        $eggId = (int) $request->input('egg_id');

        DB::table(WinWings::TABLE_PROFILES)->where('egg_id', $eggId)->delete();

        WinWings::flush();

        return $this->ok('Profile deleted.', ['egg_id' => $eggId, 'summary' => null]);
    }

    /**
     * The spellings of a console interrupt the daemon accepts, and the one this
     * extension stores.
     *
     * Kept in step with isCtrlC() in the daemon's environment/windows/power.go,
     * which lowercases and trims before comparing. SIGINT is in the list because
     * an egg that already carries it means exactly this.
     */
    private const INTERRUPT_SPELLINGS = ['ctrl_c', 'ctrl+c', 'ctrlc', '^c', 'sigint'];

    private static function canonicalInterrupt(string $value): ?string
    {
        return in_array(strtolower(trim($value)), self::INTERRUPT_SPELLINGS, true) ? 'ctrl_c' : null;
    }

    /**
     * The condensed form the egg list shows. Deliberately excludes the install
     * script: the list renders every egg on the panel and the scripts are the
     * only thing here big enough to matter.
     */
    private function summary(?object $profile): ?array
    {
        if (!$profile) {
            return null;
        }

        return [
            'enabled' => (bool) $profile->enabled,
            'runtime' => (string) $profile->runtime,
            'has_startup' => trim((string) ($profile->startup ?? '')) !== '',
            'stop_type' => $profile->stop_type,
            'stop_value' => (string) ($profile->stop_value ?? ''),
            'pseudo_console' => (bool) $profile->pseudo_console,
            'install_override' => (bool) $profile->install_override,
        ];
    }

    // ------------------------------------------------------------------ replies

    private function ok(string $message, array $extra = []): JsonResponse
    {
        return new JsonResponse(['success' => true, 'message' => $message] + $extra);
    }

    private function fail(string $message): JsonResponse
    {
        return new JsonResponse(['success' => false, 'message' => $message], 422);
    }
}
