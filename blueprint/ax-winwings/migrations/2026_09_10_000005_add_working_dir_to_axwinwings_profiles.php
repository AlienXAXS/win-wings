<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * Where the server process is started, relative to its data directory.
     *
     * The node starts every server in its data directory, which is the only part
     * of the tree the server's own account can write. Some games do not ask where
     * they are — they compute their paths by climbing out of wherever they were
     * started, a log root at "..\Logs" being the common shape. From the data
     * directory that resolves to the server's root, which the account is denied
     * and must stay denied: worker.json lives there, and a server able to write
     * it can rewrite the command its own supervisor executes.
     *
     * Naming the subdirectory the game was installed into moves the whole
     * computation back inside the sandbox, with no ACL loosened anywhere.
     *
     * Nullable, and NULL means the data directory. A profile written before this
     * migration behaves exactly as it did.
     */
    public function up(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            // Relative to the server's data directory, and refused by the node if
            // it resolves outside it. Substituted node-side, so it can be built
            // out of egg variables like every other path here.
            $table->string('working_dir', 512)->nullable();
        });
    }

    public function down(): void
    {
        Schema::table('axwinwings_profiles', function (Blueprint $table) {
            $table->dropColumn(['working_dir']);
        });
    }
};
