<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    /**
     * Which nodes run win-wings.
     *
     * The Panel has no column for this and we are not adding one to `nodes` — a
     * side table can be dropped cleanly when the extension is removed, and a
     * panel upgrade cannot collide with it.
     *
     * Rows are normally written by the daemon itself: only win-wings ever calls
     * /api/remote/windows/*, so the first ping from a node is proof it is a
     * Windows node. `detected_at` records that, `is_windows` is what the gate
     * actually reads, so an operator can still override the detection.
     */
    public function up(): void
    {
        Schema::create('axwinwings_nodes', function (Blueprint $table) {
            $table->id();
            $table->unsignedInteger('node_id')->unique();
            $table->boolean('is_windows')->default(true);

            // Set when the flag came from a daemon call rather than from the admin
            // UI. Shown in the node list so it is obvious which rows were typed in
            // by hand and which the node asserted about itself.
            $table->timestamp('detected_at')->nullable();

            // Last time this node asked for anything on the Windows API. A node that
            // has not called in a long time is either down or no longer running
            // win-wings, and the overview says so rather than leaving it ambiguous.
            $table->timestamp('last_seen_at')->nullable();
            $table->string('last_seen_endpoint', 64)->nullable();

            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('axwinwings_nodes');
    }
};
