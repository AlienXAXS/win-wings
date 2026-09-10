<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * Key/value settings. A wide single-row table would need a migration for
     * every new toggle; this does not.
     */
    public function up(): void
    {
        Schema::create('axwinwings_settings', function (Blueprint $table) {
            $table->id();
            $table->string('key', 191)->unique();
            $table->text('value')->nullable();
            $table->timestamps();
        });

        $defaults = [
            // Refuse to create a server on a Windows node when its egg has no
            // enabled profile. The daemon refuses it anyway, but by then the record
            // exists and the user is staring at a server stuck in "installing".
            'gate_enabled' => '1',

            // Message shown when the gate fires. Operator-editable because the right
            // wording depends on whether users pick their own node.
            'gate_message' => 'This egg is not available on Windows nodes.',

            // Take the first call on /api/remote/windows/* from a node as proof that
            // the node runs win-wings. Nothing else calls those routes.
            'auto_flag_nodes' => '1',

            // Rewrite the standard install endpoint for Windows nodes, so the daemon
            // gets PowerShell and the profile's runtime instead of the egg's bash
            // script and Docker image. Without this an install runs the Linux script
            // through PowerShell and fails on the first apt-get.
            'override_install_endpoint' => '1',

            // The runtime names offered in the profile editor. These are a convention
            // between this plugin and each node's config.yml `runtime.runtimes` map,
            // not something the daemon defines — these are the ones
            // scripts/provision-host.ps1 emits.
            'runtimes' => json_encode(['java-8', 'java-11', 'java-17', 'java-21', 'dotnet-8']),
        ];

        $rows = [];
        foreach ($defaults as $key => $value) {
            $rows[] = ['key' => $key, 'value' => $value, 'created_at' => now(), 'updated_at' => now()];
        }

        DB::table('axwinwings_settings')->insert($rows);
    }

    public function down(): void
    {
        Schema::dropIfExists('axwinwings_settings');
    }
};
