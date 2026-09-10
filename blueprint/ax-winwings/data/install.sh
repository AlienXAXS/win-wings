#!/bin/bash
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PANEL_DIR="${PTERODACTYL_DIRECTORY:-$(pwd)}"

ROUTES="$PANEL_DIR/routes/api-remote.php"
CREATION="$PANEL_DIR/app/Services/Servers/ServerCreationService.php"

# ---------------------------------------------------------------------------
# 1. The remote API routes.
#
# Blueprint can only give an extension routes under /extensions/<id>, and the
# daemon builds its URLs as <panel>/api/remote/... with the node token it
# already holds. Serving those paths therefore means adding them to the Panel's
# own api-remote.php, which is also what puts them behind the existing `daemon`
# authentication middleware -- the alternative being a second per-node
# credential to provision, rotate and eventually misconfigure.
#
# One appended line, loading a file that ships with the extension. Everything
# that can change lives in that file, so an update never has to re-patch this.
# ---------------------------------------------------------------------------

if [ ! -f "$ROUTES" ]; then
	echo ""
	echo "  ERROR: $ROUTES does not exist."
	echo "  This does not look like a Pterodactyl panel directory. Win-Wings was NOT installed."
	echo ""
	exit 1
fi

if grep -q "axwinwings" "$ROUTES"; then
	echo "Windows API routes already registered in routes/api-remote.php ... Skipping"
else
	echo "Registering Windows API routes in routes/api-remote.php ..."

	# Appended, so it is registered after the core routes. That ordering is load
	# bearing: the file re-registers /servers/{uuid}/install, and Laravel resolves
	# a duplicate method+URI to the route registered last.
	cat >> "$ROUTES" <<'PHP'

// axwinwings-start -- Windows profile API for win-wings nodes.
// Added by the Ax Win-Wings extension and removed again by its removal script,
// which deletes everything between these two markers. See that extension's README.
//
// Guarded because a missing file here would fatal every panel request, and an
// extension directory can be deleted without the removal script ever running.
if (is_file($axwinwings = base_path('app/BlueprintFramework/Extensions/axwinwings/remote-routes.php'))) { Route::group([], $axwinwings); }
// axwinwings-end
PHP

	echo "Registering Windows API routes in routes/api-remote.php ... Done"
fi

# ---------------------------------------------------------------------------
# 2. The egg gate.
#
# Optional. The daemon refuses an unprofiled egg by itself, so a panel that
# fails this patch is still correct -- it just tells the user later and worse.
# ---------------------------------------------------------------------------

if [ ! -f "$CREATION" ]; then
	echo "  WARNING: ServerCreationService.php not found; the egg-selection gate was not installed."
	echo "           Creating a server with an unprofiled egg on a Windows node will fail at the daemon instead."
elif grep -q "axwinwings" "$CREATION"; then
	echo "Egg gate already present in ServerCreationService.php ... Skipping"
elif ! grep -q '$eggVariableData = $this->validatorService' "$CREATION"; then
	echo ""
	echo "  WARNING: could not find the hook point in ServerCreationService.php."
	echo "           Another extension has probably modified it, so the egg-selection gate"
	echo "           was NOT installed. The remote API above is installed and working; a"
	echo "           server created with an unprofiled egg will be refused by the node"
	echo "           rather than by the panel."
	echo ""
else
	echo "Adding egg gate to ServerCreationService.php ..."

	# Runs before anything is written -- the point is to fail while the user is
	# still looking at the form, not after a record and an allocation exist.
	sed -i -e "s|\$eggVariableData = \$this->validatorService|\\\\Pterodactyl\\\\BlueprintFramework\\\\Extensions\\\\axwinwings\\\\EggGate::check(\$data);\n\n        \$eggVariableData = \$this->validatorService|" "$CREATION"

	echo "Adding egg gate to ServerCreationService.php ... Done"
fi

# ---------------------------------------------------------------------------
# 3. Route cache.
#
# A panel that has run `artisan optimize` is serving routes from a compiled
# file, and would ignore the line added above until something rebuilt it.
# Rebuild it only if it was already there, so this cannot quietly turn caching
# on for a panel that had chosen not to use it.
# ---------------------------------------------------------------------------

if ls "$PANEL_DIR"/bootstrap/cache/routes-*.php >/dev/null 2>&1; then
	echo "Rebuilding the route cache ..."
	(cd "$PANEL_DIR" && php artisan route:cache) || echo "  WARNING: route:cache failed. Run it by hand, or the Windows API will 404."
else
	(cd "$PANEL_DIR" && php artisan route:clear) >/dev/null 2>&1 || true
fi

# ---------------------------------------------------------------------------
# 4. Sidebar link. Cosmetic, and never fatal.
# ---------------------------------------------------------------------------

if ! php "$SCRIPT_DIR/alienx_sidebar_patch.php" install "$PANEL_DIR"; then
	echo "[AlienX] Sidebar patch skipped — Win-Wings is still reachable at /admin/extensions/axwinwings"
fi

echo ""
echo "  Ax Win-Wings installed."
echo ""
echo "  Next:"
echo "    1. /admin/extensions/axwinwings — check the overview says the API is registered."
echo "    2. Start a win-wings node with runtime.require_windows_profile: true."
echo "       Its first call flags it as a Windows node automatically."
echo "    3. Write a profile for each egg you intend to run on it."
echo ""
