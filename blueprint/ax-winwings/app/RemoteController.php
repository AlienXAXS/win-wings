<?php

namespace Pterodactyl\BlueprintFramework\Extensions\{identifier};

use Illuminate\Database\Eloquent\Builder;
use Illuminate\Support\Facades\DB;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Http\Response;
use Pterodactyl\Http\Controllers\Controller;
use Pterodactyl\Models\Server;

/**
 * The Panel side of the win-wings contract (see win-wings docs/PANEL-API.md).
 *
 * These routes live on the Panel's existing `daemon` middleware group, reached
 * by a one-line addition to routes/api-remote.php that data/install.sh makes.
 * That is deliberate: the node already holds a credential for this API, and
 * inventing a second token would mean another secret to provision and rotate
 * per node, plus a new unauthenticated surface whenever one was misconfigured.
 *
 * Every method here re-checks that the calling node actually owns the server it
 * is asking about. The daemon middleware only proves *which* node is calling.
 */
class RemoteController extends Controller
{
    /**
     * GET /api/remote/windows/ping
     *
     * The daemon calls this once at boot when `runtime.require_windows_profile`
     * is on, and refuses to start on a 404. That is the whole point of it: a
     * node with no plugin behind it would otherwise come up looking healthy and
     * then fail on every individual server.
     *
     * The body is ignored by the daemon; it is here for a human with curl.
     */
    public function ping(Request $request): JsonResponse
    {
        // An installed extension whose migrations have not run can answer every
        // route and serve nothing, which would let a node boot and then refuse
        // every server it owns. Answering the boot check the same way a missing
        // extension does keeps that failure at boot, where the operator is
        // already looking, and the daemon message points at the plugin.
        if (!WinWings::ready()) {
            return new JsonResponse([
                'error' => 'The Win-Wings extension is installed but its tables are missing. Re-run the extension migrations.',
            ], Response::HTTP_NOT_FOUND);
        }

        $node = $request->attributes->get('node');

        if ($node) {
            WinWings::touchNode((int) $node->id, 'ping');
        }

        return new JsonResponse([
            'status' => 'ok',
            'extension' => 'axwinwings',
            'version' => '1.1.0',
            'profiles' => DB::table(WinWings::TABLE_PROFILES)->where('enabled', true)->count(),
            'windows_nodes' => DB::table(WinWings::TABLE_NODES)->where('is_windows', true)->count(),
        ]);
    }

    /**
     * GET /api/remote/windows/servers/{uuid}/profile
     *
     * A 404 here is a real answer, not an error: it tells a production node to
     * refuse the server outright rather than initialise it with Linux defaults
     * that cannot work. So a missing table, a missing profile and a disabled
     * profile all deliberately produce the same 404.
     */
    public function profile(Request $request, string $uuid): JsonResponse
    {
        $server = $this->serverForNode($request, $uuid);

        WinWings::touchNode((int) $server->node_id, 'profile');

        $profile = WinWings::profileForEgg((int) $server->egg_id);

        if (!$profile) {
            return new JsonResponse([
                'error' => 'No Windows profile is configured for this egg.',
                'egg_id' => (int) $server->egg_id,
                'egg' => optional($server->egg)->name,
            ], Response::HTTP_NOT_FOUND);
        }

        return new JsonResponse(WinWings::profilePayload($profile));
    }

    /**
     * GET /api/remote/windows/servers/{uuid}/install
     *
     * The optional endpoint from the contract. The daemon as shipped does not
     * call it — it uses the standard install route — but it is served anyway so
     * that a future daemon which prefers it gets the same answer, and so an
     * operator can see exactly what a node would be handed.
     */
    public function install(Request $request, string $uuid): JsonResponse
    {
        $server = $this->serverForNode($request, $uuid);

        WinWings::touchNode((int) $server->node_id, 'install');

        return new JsonResponse($this->installPayload($server));
    }

    /**
     * GET /api/remote/servers/{uuid}/install — the Panel's own route, replaced.
     *
     * Registered after routes/api-remote.php has already registered the core
     * controller, so this wins the lookup. Anything that is not a Windows node
     * gets exactly what the core controller would have returned, byte for byte,
     * so a mixed panel is unaffected.
     *
     * This override is what makes install work at all: the daemon fetches its
     * script from the standard endpoint and runs it through PowerShell, so
     * without a swap here a Windows node runs the egg's bash script and dies on
     * the first apt-get. It also replaces `container_image` with the profile's
     * runtime, which the daemon exports as INSTALL_RUNTIME and resolves onto the
     * script's PATH.
     */
    public function standardInstall(Request $request, string $uuid): JsonResponse
    {
        $server = $this->serverForNode($request, $uuid);
        $egg = $server->egg;

        if (!WinWings::flag('override_install_endpoint', true) || !WinWings::isWindowsNode((int) $server->node_id)) {
            return new JsonResponse([
                'container_image' => $egg->copy_script_container,
                'entrypoint' => $egg->copy_script_entry,
                'script' => $egg->copy_script_install,
            ]);
        }

        WinWings::touchNode((int) $server->node_id, 'install');

        return new JsonResponse($this->installPayload($server));
    }

    /**
     * What a Windows node should be handed for an install.
     *
     * Falls back to the egg's own fields field by field rather than all at once:
     * a profile that overrides only the runtime is common, and a profile that
     * overrides only the script is what you write while porting an egg.
     */
    private function installPayload(Server $server): array
    {
        $egg = $server->egg;
        $profile = WinWings::profileForEgg((int) $server->egg_id);

        $image = $egg->copy_script_container;
        $script = $egg->copy_script_install;

        if ($profile) {
            if (trim((string) $profile->runtime) !== '') {
                $image = (string) $profile->runtime;
            }

            if ($profile->install_override && trim((string) $profile->install_script) !== '') {
                $script = (string) $profile->install_script;
            }
        }

        return [
            'container_image' => $image,

            // Ignored by the daemon, which always runs PowerShell. Reported
            // honestly anyway so that reading the response tells you the truth
            // about what will execute.
            'entrypoint' => 'powershell',
            'script' => $script,
        ];
    }

    /**
     * Resolve the server and confirm the calling node owns it.
     *
     * Matches the core install controller's lookup, which accepts either the
     * full UUID or the short one.
     */
    private function serverForNode(Request $request, string $uuid): Server
    {
        /** @var Server|null $server */
        $server = Server::query()
            ->with(['egg', 'node'])
            ->where(function (Builder $query) use ($uuid) {
                $query->where('uuidShort', $uuid)->orWhere('uuid', $uuid);
            })
            ->first();

        if (!$server) {
            abort(Response::HTTP_NOT_FOUND, 'No server exists with that UUID.');
        }

        $node = $request->attributes->get('node');

        if (!$node || (int) $server->node_id !== (int) $node->id) {
            abort(Response::HTTP_FORBIDDEN, 'Requesting node does not have permission to access this server.');
        }

        return $server;
    }
}
