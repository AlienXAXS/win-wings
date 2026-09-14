<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * The advanced half of a profile: a console that is not the process's stdio,
     * and a script run before every boot.
     *
     * Both exist for the eggs whose Linux startup line is a shell pipeline rather
     * than a command — the ones that tail a log file into the container's stdout
     * and put a telnet client on its stdin, because the game writes nothing to
     * one and reads nothing from the other. None of that survives the port, so
     * the node does the same two jobs itself and these columns are how it is told
     * to.
     *
     * Every column is nullable and every default is off. A profile written before
     * this migration keeps behaving exactly as it did.
     */
    public function up(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            // Where console output comes from. NULL or '' means the process's own
            // stdout, which is right for all but a handful of eggs.
            $table->string('console_source_type', 16)->nullable();

            // Relative to the server's data directory, and refused by the daemon
            // if it resolves outside it. Long because a log path is often built
            // out of egg variables, which are substituted node-side.
            $table->string('console_source_path', 512)->nullable();

            // utf-8, utf-16le or utf-16be. NULL means utf-8.
            $table->string('console_source_encoding', 32)->nullable();

            // Where console input goes. NULL or '' means the process's stdin.
            $table->string('console_command_type', 16)->nullable();

            // Empty means 127.0.0.1. A console like this authenticates weakly or
            // not at all and should never be reachable off the host.
            $table->string('console_command_host', 191)->nullable();

            // A string, not an integer: it is nearly always written as an egg
            // variable, since every server on a node has a different one. The
            // daemon substitutes and parses it.
            $table->string('console_command_port', 64)->nullable();

            // Sent as the first line after connecting. Also usually a variable.
            $table->string('console_command_password', 191)->nullable();

            // How long the node waits for the port to open after the server
            // starts. This is a world-loading time, not a network timeout. NULL
            // uses the node's default.
            $table->unsignedInteger('console_connect_timeout')->nullable();

            // PowerShell run to completion before every boot, in the server's
            // directory and under its own account. Kept beside the install script
            // and switched separately, because a profile frequently wants one and
            // not the other.
            $table->boolean('prestart_override')->default(false);
            $table->longText('prestart_script')->nullable();
        });
    }

    public function down(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            $table->dropColumn([
                'console_source_type',
                'console_source_path',
                'console_source_encoding',
                'console_command_type',
                'console_command_host',
                'console_command_port',
                'console_command_password',
                'console_connect_timeout',
                'prestart_override',
                'prestart_script',
            ]);
        });
    }
};
