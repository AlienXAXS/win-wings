<?php

use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Route;
use Pterodactyl\Models\Egg;
use Pterodactyl\BlueprintFramework\Extensions\{identifier}\ScriptTemplates;
use Pterodactyl\BlueprintFramework\Extensions\{identifier}\WinWings;

/*
 * Read-only endpoints for the admin page, under /extensions/axwinwings/.
 *
 * The profile editor holds a whole PowerShell script per egg. Rendering every
 * egg's script into one page would be several megabytes on a panel with a real
 * nest list, so the page ships the summary and fetches a profile when a row is
 * opened. Writes all go through the admin controller's POST, which is where the
 * CSRF token and the admin check already are.
 */

Route::middleware(['web', 'auth'])->group(function () {
    /**
     * Everything an egg's editor needs: the stored profile if there is one, and
     * the egg's own values to show alongside it as the fallback -- an empty
     * startup field means "use the egg's", which is only comprehensible if the
     * egg's is on screen next to it.
     */
    Route::get('/egg/{egg}', function (Request $request, int $egg) {
        if (!$request->user()->root_admin) {
            abort(403);
        }

        if (!WinWings::ready()) {
            abort(503, 'Win-Wings tables are missing.');
        }

        /** @var Egg|null $model */
        $model = Egg::query()->find($egg);

        if (!$model) {
            abort(404);
        }

        $profile = WinWings::rawProfileForEgg($egg);

        return response()->json([
            'egg' => [
                'id' => $model->id,
                'name' => $model->name,
                'startup' => (string) $model->startup,
                'container' => $model->copy_script_container,
                'stop' => (string) $model->inherit_config_stop,

                // Where the egg inherits its script from another egg, this is the
                // resolved script -- the same thing the daemon would have been
                // handed -- so what you see is what you are replacing.
                'script' => (string) $model->copy_script_install,
                'script_from' => $model->copy_script_from,
            ],
            'profile' => $profile ? [
                'enabled' => (bool) $profile->enabled,
                'runtime' => (string) $profile->runtime,
                'startup' => (string) ($profile->startup ?? ''),
                'stop_type' => $profile->stop_type,
                'stop_value' => (string) ($profile->stop_value ?? ''),
                'pseudo_console' => (bool) $profile->pseudo_console,
                'install_override' => (bool) $profile->install_override,
                'install_script' => (string) ($profile->install_script ?? ''),
                'notes' => (string) ($profile->notes ?? ''),
            ] : null,

            // Exactly what GET /api/remote/windows/servers/{uuid}/profile would
            // return for this egg. Worth showing: the difference between an empty
            // field and an omitted one is the difference between overriding the
            // egg and inheriting from it, and that is invisible in the form.
            'payload' => $profile && $profile->enabled ? WinWings::profilePayload($profile) : null,
        ]);
    });

    /**
     * The PowerShell starter scripts. Served rather than inlined because
     * Blueprint eats backslash escapes inside a view's script tags.
     */
    Route::get('/templates', function (Request $request) {
        if (!$request->user()->root_admin) {
            abort(403);
        }

        return response()->json(ScriptTemplates::all());
    });

    /**
     * Servers that will fail. A server on a Windows node whose egg has no
     * enabled profile is refused by the daemon every time it starts, and the
     * only place that is visible is the node's log.
     */
    Route::get('/exposure', function (Request $request) {
        if (!$request->user()->root_admin) {
            abort(403);
        }

        if (!WinWings::ready()) {
            abort(503, 'Win-Wings tables are missing.');
        }

        $windowsNodes = DB::table(WinWings::TABLE_NODES)
            ->where('is_windows', true)
            ->pluck('node_id')
            ->all();

        if (empty($windowsNodes)) {
            return response()->json(['servers' => []]);
        }

        $profiled = DB::table(WinWings::TABLE_PROFILES)
            ->where('enabled', true)
            ->pluck('egg_id')
            ->all();

        $rows = DB::table('servers')
            ->join('eggs', 'eggs.id', '=', 'servers.egg_id')
            ->join('nodes', 'nodes.id', '=', 'servers.node_id')
            ->whereIn('servers.node_id', $windowsNodes)
            ->when(!empty($profiled), fn ($q) => $q->whereNotIn('servers.egg_id', $profiled))
            ->orderBy('nodes.name')
            ->orderBy('servers.name')
            ->limit(500)
            ->get([
                'servers.id as id',
                'servers.name as name',
                'servers.uuidShort as uuid_short',
                'servers.egg_id as egg_id',
                'eggs.name as egg',
                'nodes.name as node',
            ]);

        return response()->json(['servers' => $rows]);
    });
});
