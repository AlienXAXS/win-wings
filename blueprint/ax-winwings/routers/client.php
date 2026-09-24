<?php

use Illuminate\Support\Facades\Route;
use Pterodactyl\BlueprintFramework\Extensions\{identifier}\ClientController;

/*
 * Client API, under /api/client/extensions/axwinwings/.
 *
 * Read by the component on the server's Startup page. The route is not behind
 * the Panel's server-access middleware -- Blueprint gives extension routes no
 * {server} binding -- so the controller does that check itself.
 */

Route::get('/servers/{uuid}/startup', [ClientController::class, 'startup']);
