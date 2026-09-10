<?php

declare(strict_types=1);

/**
 * Adds an "ALIENX'S PLUGINS" section to the Pterodactyl admin sidebar.
 *
 * Pterodactyl's sidebar is plain Blade (resources/views/layouts/admin.blade.php)
 * and Blueprint gives extensions no hook into it, so the only way in is to patch
 * the file — the same thing MultiAuth does for its "OAuth Settings" entry.
 *
 * The block is written between markers and is IDENTICAL no matter which of the
 * plugins below installs it: every entry is wrapped in `@if (Route::has(...))`,
 * so a plugin that is not installed simply renders nothing, and the heading
 * itself only renders when at least one of them is present. That means several
 * plugins can ship this same script, install and uninstall in any order, and
 * never produce a duplicate or an orphaned heading.
 *
 * Usage: php alienx_sidebar_patch.php [install|remove|verify] [panel-dir]
 *
 * Not patched: Blueprint's own "Extensions" sidebar entry stays highlighted when
 * you are on a plugin page, so two items light up at once. Narrowing that means
 * editing resources/views/blueprint/admin/admin.blade.php, which MultiAuth also
 * rewrites — left alone deliberately so the two extensions cannot corrupt each
 * other's uninstall.
 */

const MARKER_START = '{{-- alienx-plugins-start --}}';
const MARKER_END   = '{{-- alienx-plugins-end --}}';
const HEADING      = "ALIENX'S PLUGINS";

/**
 * Blueprint identifier => sidebar label + Font Awesome 4 icon. FA4 is already
 * loaded by the admin layout, so entries cost no extra requests.
 *
 * Add a plugin here and re-run any one of their installs to pick it up.
 */
const PLUGINS = [
    'axfreeservers'      => ['label' => 'Free Servers',         'icon' => 'fa-gift'],
    'axbadactordetector' => ['label' => 'Bad Actor Detector',   'icon' => 'fa-user-secret'],
    'axportmanager'      => ['label' => 'Port Manager',         'icon' => 'fa-plug'],
    'axwinwings'         => ['label' => 'Win-Wings',            'icon' => 'fa-windows'],
    'agnkofi'            => ['label' => 'Ko-Fi',                'icon' => 'fa-coffee'],
    'egginstaller'       => ['label' => 'Egg Installer',        'icon' => 'fa-download'],
    'factoriomodmanager' => ['label' => 'Factorio Mods',        'icon' => 'fa-cogs'],
];

$mode    = $argv[1] ?? 'install';
$baseDir = rtrim((string) ($argv[2] ?? getcwd()), "\\/");
$layout  = $baseDir . '/resources/views/layouts/admin.blade.php';

try {
    switch ($mode) {
        case 'install':
            install($layout);
            verify($layout, true);
            fwrite(STDOUT, "[AlienX] Admin sidebar section installed.\n");
            break;

        case 'remove':
            // Only tear the block out once the last of these plugins is going;
            // otherwise the departing entry hides itself via Route::has().
            if (othersStillInstalled($baseDir)) {
                fwrite(STDOUT, "[AlienX] Other AlienX plugins remain — sidebar section kept.\n");
                break;
            }
            remove($layout);
            verify($layout, false);
            fwrite(STDOUT, "[AlienX] Admin sidebar section removed.\n");
            break;

        case 'verify':
            verify($layout, true);
            fwrite(STDOUT, "[AlienX] Admin sidebar section present.\n");
            break;

        default:
            fwrite(STDERR, "Usage: alienx_sidebar_patch.php [install|remove|verify] [panel-dir]\n");
            exit(1);
    }
} catch (RuntimeException $e) {
    fwrite(STDERR, "[AlienX] {$e->getMessage()}\n");
    exit(1);
}

function install(string $path): void
{
    $content = read($path);
    $eol     = str_contains($content, "\r\n") ? "\r\n" : "\n";

    // Already patched (possibly by a sibling plugin, possibly an older version
    // of this list) — rewrite in place so the block stays where it was.
    if (str_contains($content, MARKER_START) && str_contains($content, MARKER_END)) {
        $indent = existingIndent($content);

        // Callback, not a replacement string: a future label containing $ or \
        // would otherwise be mangled by backreference expansion.
        $patched = preg_replace_callback(
            '/[ \t]*' . preg_quote(MARKER_START, '/') . '.*?' . preg_quote(MARKER_END, '/') . '/s',
            static fn (): string => block($indent, $eol),
            $content,
            1,
            $count
        );

        if (!is_string($patched) || $count < 1) {
            throw new RuntimeException("Could not rewrite the existing sidebar block in {$path}");
        }

        write($path, $patched);
        return;
    }

    // Fresh install: sit above SERVICE MANAGEMENT, so the order reads
    // BASIC ADMINISTRATION / MANAGEMENT / ALIENX'S PLUGINS / SERVICE MANAGEMENT.
    // MultiAuth anchors on the MANAGEMENT header instead, so the two never
    // fight over the same insertion point.
    $anchor = '/^(?<indent>[ \t]*)<li class="header">SERVICE MANAGEMENT<\/li>/m';

    if (preg_match($anchor, $content, $m, PREG_OFFSET_CAPTURE) !== 1) {
        throw new RuntimeException(
            'Could not find the SERVICE MANAGEMENT sidebar header. '
            . 'Check resources/views/layouts/admin.blade.php by hand.'
        );
    }

    $indent = $m['indent'][0];
    $offset = $m[0][1];

    write($path, substr_replace($content, block($indent, $eol) . $eol, $offset, 0));
}

function remove(string $path): void
{
    $content = read($path);

    if (!str_contains($content, MARKER_START)) {
        return;
    }

    $patched = preg_replace(
        '/[ \t]*' . preg_quote(MARKER_START, '/') . '.*?' . preg_quote(MARKER_END, '/') . '\R?/s',
        '',
        $content
    );

    if (!is_string($patched)) {
        throw new RuntimeException("Could not remove the sidebar block from {$path}");
    }

    write($path, $patched);
}

function verify(string $path, bool $shouldExist): void
{
    $content = read($path);
    $present = str_contains($content, MARKER_START) && str_contains($content, MARKER_END);

    if ($present !== $shouldExist) {
        throw new RuntimeException(sprintf(
            'Sidebar block should be %s in %s but is not.',
            $shouldExist ? 'present' : 'absent',
            $path
        ));
    }

    if ($shouldExist && !str_contains($content, HEADING)) {
        throw new RuntimeException("Sidebar block in {$path} is missing its heading.");
    }
}

/**
 * Is any OTHER plugin from the list still installed? Blueprint runs remove.sh
 * before it deletes the extension, so the one being uninstalled is skipped by
 * identifier rather than by looking at the filesystem.
 */
function othersStillInstalled(string $baseDir): bool
{
    $leaving = getenv('EXTENSION_IDENTIFIER') ?: '';

    foreach (array_keys(PLUGINS) as $identifier) {
        if ($identifier === $leaving) {
            continue;
        }
        if (is_dir($baseDir . '/.blueprint/extensions/' . $identifier)) {
            return true;
        }
    }

    return false;
}

function existingIndent(string $content): string
{
    if (preg_match('/^(?<indent>[ \t]*)' . preg_quote(MARKER_START, '/') . '/m', $content, $m) !== 1) {
        return '                        ';
    }

    return $m['indent'];
}

/** The Blade block itself, indented to match the surrounding sidebar markup. */
function block(string $indent, string $eol): string
{
    $anyInstalled = implode(' || ', array_map(
        static fn (string $id): string => "Route::has('admin.extensions.{$id}.index')",
        array_keys(PLUGINS)
    ));

    $lines = [
        MARKER_START,
        '@if (' . $anyInstalled . ')',
        '<li class="header">' . HEADING . '</li>',
        '@endif',
    ];

    foreach (PLUGINS as $identifier => $plugin) {
        $route  = "admin.extensions.{$identifier}.index";
        $active = "{{ ! starts_with(Route::currentRouteName(), 'admin.extensions.{$identifier}') ?: 'active' }}";

        array_push(
            $lines,
            "@if (Route::has('{$route}'))",
            '<li class="' . $active . '">',
            "    <a href=\"{{ route('{$route}') }}\">",
            '        <i class="fa ' . $plugin['icon'] . '"></i> <span>' . $plugin['label'] . '</span>',
            '    </a>',
            '</li>',
            '@endif'
        );
    }

    $lines[] = MARKER_END;

    return implode($eol, array_map(static fn (string $l): string => $indent . $l, $lines));
}

function read(string $path): string
{
    if (!is_file($path)) {
        throw new RuntimeException("Target file not found: {$path}");
    }

    $content = file_get_contents($path);

    if ($content === false) {
        throw new RuntimeException("Cannot read: {$path}");
    }

    return $content;
}

function write(string $path, string $content): void
{
    if (file_put_contents($path, $content) === false) {
        throw new RuntimeException("Cannot write: {$path}");
    }
}
