<?php

/*
 * Windows API routes for win-wings.
 *
 * Loaded from the end of the Panel's routes/api-remote.php by a single line that
 * data/install.sh appends, so these inherit that file's `daemon` middleware,
 * its /api/remote prefix and its node authentication. Nothing else is patched,
 * and remove.sh deletes exactly that line.
 *
 * Being loaded last is not incidental. The final route here re-registers the
 * Panel's own install endpoint, and Laravel resolves a duplicate method+URI to
 * whichever route was registered most recently.
 */

use Illuminate\Support\Facades\Route;
use Pterodactyl\BlueprintFramework\Extensions\axwinwings\RemoteController;

Route::get('/windows/ping', [RemoteController::class, 'ping']);

Route::group(['prefix' => '/windows/servers/{uuid}'], function () {
    Route::get('/profile', [RemoteController::class, 'profile']);
    Route::get('/install', [RemoteController::class, 'install']);
});

/*
 * Serve PowerShell instead of bash to Windows nodes.
 *
 * The daemon reads its install script from this route, not from the Windows one
 * above, so an egg's Linux script would otherwise be handed straight to
 * powershell.exe. Requests from every other node fall through to exactly the
 * response the core controller produces.
 */
Route::get('/servers/{uuid}/install', [RemoteController::class, 'standardInstall']);
