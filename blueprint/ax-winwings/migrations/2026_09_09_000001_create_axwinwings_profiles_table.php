<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * One row per egg. This is the Windows half of an egg: everything a stock
     * Pterodactyl egg carries that means nothing on a host with no containers,
     * plus the things it does not carry at all.
     *
     * Keyed on egg_id rather than on (egg_id, node_id): a profile describes how
     * the software is run on Windows, which does not vary between two Windows
     * nodes. If it ever needs to, that is a second table, not a wider key here.
     */
    public function up(): void
    {
        Schema::create('axwinwings_profiles', function (Blueprint $table) {
            $table->id();
            $table->unsignedInteger('egg_id')->unique();

            // Off means the daemon is told there is no profile at all (404), which
            // on a production node refuses the server rather than running it with
            // Linux defaults that cannot work. Lets an operator park a broken
            // profile without deleting the work that went into it.
            $table->boolean('enabled')->default(true);

            // Names an entry in the node's `runtime.runtimes` map, which the daemon
            // resolves to a directory and prepends to the server's PATH. Empty means
            // "no runtime needed" and falls back to the egg's container image.
            $table->string('runtime', 191)->default('');

            // Windows startup command. Empty means the daemon uses the egg's own
            // startup line unchanged, which is right for the handful of eggs whose
            // command happens to be portable.
            $table->text('startup')->nullable();

            // NULL means "inherit the egg's stop configuration". That is only ever
            // correct when the egg already stops with a command; a signal-based egg
            // left NULL will not shut down cleanly, which is why the admin UI warns
            // about it.
            $table->string('stop_type', 32)->nullable();
            $table->string('stop_value', 191)->nullable();

            // ConPTY instead of pipes. Costs a VT escape stream in the console, so
            // it is off unless the process actually inspects its stdout.
            $table->boolean('pseudo_console')->default(false);

            // The PowerShell install script. Held here rather than overwriting the
            // egg's own script so the same egg can keep working on Linux nodes.
            $table->boolean('install_override')->default(false);
            $table->longText('install_script')->nullable();

            $table->text('notes')->nullable();
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('axwinwings_profiles');
    }
};
