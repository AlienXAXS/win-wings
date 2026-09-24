<?php

namespace Pterodactyl\BlueprintFramework\Extensions\{identifier};

use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Pterodactyl\Http\Controllers\Controller;
use Pterodactyl\Models\Permission;
use Pterodactyl\Models\Server;

/**
 * What the server's own Startup page needs to stop lying on a Windows node.
 *
 * The Panel renders the egg's startup line and Docker image there, and on a
 * win-wings node neither is what runs: the profile's command replaces the line
 * whenever it sets one, and there is no container at all. This answers with the
 * command the daemon will actually execute, substituted the way the Panel
 * substitutes its own, so the component can put it next to the one that is
 * wrong.
 */
class ClientController extends Controller
{
    /**
     * GET /api/client/extensions/axwinwings/servers/{uuid}/startup
     *
     * `windows: false` is the answer for every server the component has nothing
     * to say about, including ones on a panel whose tables are missing, so the
     * page renders exactly as it would without the extension.
     */
    public function startup(Request $request, string $uuid): JsonResponse
    {
        /** @var Server|null $server */
        $server = Server::query()
            ->where('uuidShort', $uuid)
            ->orWhere('uuid', $uuid)
            ->first();

        // Same answer for "no such server" and "not yours", as the Panel's own
        // client API gives, so the route cannot be used to probe for servers.
        // Its own body rather than abort(404), so that it can be told apart
        // from the route not being registered at all.
        if (!$server || !$request->user()->can(Permission::ACTION_STARTUP_READ, $server)) {
            return new JsonResponse(['error' => 'axwinwings: server not found'], 404);
        }

        if (!WinWings::isWindowsNode((int) $server->node_id)) {
            return new JsonResponse(['windows' => false]);
        }

        $profile = WinWings::profileForEgg((int) $server->egg_id);
        $template = $profile ? trim((string) ($profile->startup ?? '')) : '';

        return new JsonResponse([
            'windows' => true,

            // False means the daemon will refuse to start this server at all.
            // Worth its own state: the fix is an admin's, not the user's, and a
            // page that just shows a command would suggest otherwise.
            'profiled' => $profile !== null,

            // With no override the daemon runs the Panel's own line, and the
            // Panel's box is right. Only then is it worth saying so rather than
            // showing a second copy of the same command.
            'overridden' => $template !== '',
            'invocation' => $template !== '' ? self::substitute($server, $template) : null,
        ]);
    }

    /**
     * The Panel's StartupCommandService, extended to the ${VAR} form the daemon
     * also accepts.
     *
     * Hidden variables stay hidden: the core page prints "[hidden]" for anything
     * that is not user-viewable, and this must not become the way round that.
     * Anything the Panel does not know about -- RUNTIME_PATH and the rest of what
     * the daemon adds to the environment -- is left as written rather than
     * blanked, because an empty gap in a command line reads as a bug.
     */
    private static function substitute(Server $server, string $template): string
    {
        $values = [
            'SERVER_MEMORY' => (string) $server->memory,
            'SERVER_IP' => (string) optional($server->allocation)->ip,
            'SERVER_PORT' => (string) optional($server->allocation)->port,
        ];

        foreach ($server->variables as $variable) {
            $values[$variable->env_variable] = $variable->user_viewable
                ? (string) ($variable->server_value ?? $variable->default_value)
                : '[hidden]';
        }

        // The three forms the daemon's os.Expand ends up accepting once it has
        // rewritten {{VAR}} to ${VAR}.
        return preg_replace_callback(
            '/\{\{([A-Za-z0-9_]+)\}\}|\$\{([A-Za-z0-9_]+)\}|\$([A-Za-z_][A-Za-z0-9_]*)/',
            function (array $m) use ($values) {
                $name = ($m[1] ?? '') !== '' ? $m[1] : (($m[2] ?? '') !== '' ? $m[2] : $m[3]);

                return array_key_exists($name, $values) ? $values[$name] : $m[0];
            },
            $template
        );
    }
}
