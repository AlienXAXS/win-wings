<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * A script run when the server is asked to stop, before the stop itself.
     *
     * The stop field covers a line on stdin and a console interrupt, which is
     * what nearly every server wants. Some servers shut down cleanly only when
     * asked another way — an RCON command, a call to a web endpoint, a save that
     * has to be requested first — and their Linux eggs did that in a shell trap
     * wrapped around the game. There is no shell around the game here, so the
     * node runs this script instead, in the server's directory and under its
     * own account, with the egg's variables and SERVER_PID in the environment.
     * The server exiting during or after it is the stop succeeding; otherwise
     * the stop field is tried as if the script had not run.
     *
     * Switched separately from the pre-start script, as that is from the install
     * script, and for the same reason: a profile frequently wants one and not
     * the other. Off by default, so a profile written before this migration
     * behaves exactly as it did.
     */
    public function up(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            $table->boolean('prestop_override')->default(false);
            $table->longText('prestop_script')->nullable();
        });
    }

    public function down(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            $table->dropColumn(['prestop_override', 'prestop_script']);
        });
    }
};
