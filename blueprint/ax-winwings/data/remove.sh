#!/bin/bash
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PANEL_DIR="${PTERODACTYL_DIRECTORY:-$(pwd)}"

ROUTES="$PANEL_DIR/routes/api-remote.php"
CREATION="$PANEL_DIR/app/Services/Servers/ServerCreationService.php"

# install.sh appends its routes between two markers and adds exactly one line to
# ServerCreationService.php, so each file is restored to its original bytes by a
# single deletion. The blank line before the routes block goes too, otherwise
# repeated install/remove cycles would grow the file a line at a time.

if [ -f "$ROUTES" ] && grep -q "axwinwings" "$ROUTES"; then
	echo "Removing Windows API routes from routes/api-remote.php ..."
	sed -i -e "/axwinwings-start/,/axwinwings-end/d" "$ROUTES"

	# Collapse the trailing blank lines the deletion leaves behind.
	perl -0pi -e 's/\n+\z/\n/' "$ROUTES" 2>/dev/null || true

	echo "Removing Windows API routes from routes/api-remote.php ... Done"
fi

if [ -f "$CREATION" ] && grep -q "axwinwings" "$CREATION"; then
	echo "Removing egg gate from ServerCreationService.php ..."

	# Drop the call and the blank line install.sh put after it.
	sed -i -e "/Extensions\\\\axwinwings\\\\EggGate::check/,+1d" "$CREATION"

	echo "Removing egg gate from ServerCreationService.php ... Done"
fi

# A stale compiled route file would keep serving the routes that were just
# deleted, and every one of them points at a class that is about to be gone.
if ls "$PANEL_DIR"/bootstrap/cache/routes-*.php >/dev/null 2>&1; then
	(cd "$PANEL_DIR" && php artisan route:cache) || echo "  WARNING: route:cache failed. Run it by hand."
else
	(cd "$PANEL_DIR" && php artisan route:clear) >/dev/null 2>&1 || true
fi

# Only drops the section when this was the last AlienX plugin; otherwise the
# Win-Wings entry hides itself via Route::has().
php "$SCRIPT_DIR/alienx_sidebar_patch.php" remove "$PANEL_DIR" || true
