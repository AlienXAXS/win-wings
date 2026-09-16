@section('title')
    Win-Wings
@endsection

@section('content-header')
    <h1>Win-Wings<small>Windows profiles for win-wings nodes</small></h1>
    <ol class="breadcrumb">
        <li><a href="{{ route('admin.index') }}">Admin</a></li>
        <li><a href="{{ route('admin.extensions') }}">Extensions</a></li>
        <li class="active">Win-Wings</li>
    </ol>
@endsection

@section('content')

{{-- No backslash appears anywhere in this file on purpose. Blueprint strips
     escape sequences out of a view before it is written to the panel, which
     turns an escaped apostrophe in JavaScript into a syntax error and a newline
     escape into the letter n. So: apostrophes are typographic, PHP_EOL stands in
     for the newline escape, JavaScript builds the characters it needs with
     String.fromCharCode, and the one thing that is nothing but backslashes -- the
     PowerShell templates -- is fetched over a route rather than inlined. --}}

<style>
.ww-wrap .box { background: #29343e; border: 1px solid #3d4d5c; box-shadow: none; }
.ww-wrap .box .box-header.with-border { background: #222d38; border-bottom: 1px solid #3d4d5c; color: #cad1d8; }
.ww-wrap .box .box-title { color: #cad1d8; }
.ww-wrap .box-body { color: #cad1d8; }
.ww-wrap .box-footer { background: #222d38; border-top: 1px solid #3d4d5c; }
.ww-wrap .form-control { background: #1f2933; border: 1px solid #3d4d5c; color: #cad1d8; }
.ww-wrap .form-control:focus { border-color: #3c8dbc; background: #253040; color: #fff; box-shadow: none; }
.ww-wrap select.form-control option, .ww-wrap select.form-control optgroup { background: #1f2933; color: #cad1d8; }
.ww-wrap label, .ww-wrap .control-label { color: #b8c7ce; }
.ww-wrap .text-muted { color: #6d8492 !important; }
.ww-wrap hr { border-color: #3d4d5c; }
.ww-wrap .callout { background: #1f2933; border-left-color: #3c8dbc; color: #cad1d8; margin-bottom: 14px; }
.ww-wrap code { background: #1a2129; color: #7fb8dd; border: 0; }

/* checkbox.css zeroes the native input opacity and draws the tick with
   `input:checked + label`, which never matches an input nested in its label. */
.ww-wrap .ww-check input[type=checkbox] { opacity: 1 !important; position: static !important; margin-right: 6px; }
.ww-wrap .ww-check { margin-bottom: 14px; }
.ww-wrap .ww-check label { font-weight: 600; margin-bottom: 2px; }
.ww-wrap .ww-check p { margin: 0 0 0 22px; }

.ww-banner { display: none; padding: 9px 12px; border-radius: 3px; margin-bottom: 14px; }
.ww-banner.ww-good { display: block; background: #1e3a2a; border: 1px solid #2f6a45; color: #b6e5c8; }
.ww-banner.ww-bad { display: block; background: #3a2020; border: 1px solid #6d3232; color: #f0bcbc; }

.ww-diag { list-style: none; padding: 0; margin: 0; }
.ww-diag li { padding: 7px 0; border-top: 1px solid #2f3d4a; display: flex; align-items: flex-start; }
.ww-diag li:first-child { border-top: 0; }
.ww-diag .ww-dot { flex: 0 0 auto; width: 10px; height: 10px; border-radius: 50%; margin: 5px 10px 0 0; background: #6d8492; }
.ww-diag .ww-dot.ww-on { background: #2f9e5e; }
.ww-diag .ww-dot.ww-off { background: #c0392b; }
.ww-diag .ww-dot.ww-warn { background: #d29a2b; }
.ww-diag .ww-txt { flex: 1 1 auto; }
.ww-diag .ww-txt small { display: block; color: #6d8492; }

.ww-table { width: 100%; }
.ww-table th { color: #8fa3b1; font-size: 11px; text-transform: uppercase; letter-spacing: 1px; font-weight: 600; padding: 0 8px 7px; border-bottom: 1px solid #3d4d5c; }
.ww-table td { padding: 7px 8px; border-top: 1px solid #2f3d4a; vertical-align: middle; color: #cad1d8; }
.ww-table tbody tr:first-child td { border-top: 0; }
.ww-mono { font-family: monospace; color: #7fb8dd; }

.ww-nest-title { color: #8fa3b1; font-size: 12px; text-transform: uppercase; letter-spacing: 1px; margin: 18px 0 8px; }
.ww-egg { border: 1px solid #3d4d5c; border-radius: 3px; margin-bottom: 8px; background: #222d38; }
.ww-egg-head { display: flex; align-items: center; padding: 9px 12px; cursor: pointer; }
.ww-egg-head:hover { background: #26313d; }
.ww-egg-name { flex: 1 1 auto; color: #cad1d8; font-weight: 600; }
.ww-egg-meta { color: #6d8492; font-size: 12px; margin-left: 10px; text-align: right; }
.ww-egg-body { display: none; padding: 12px; border-top: 1px solid #3d4d5c; }
.ww-egg-body.ww-open { display: block; }
.ww-tag { display: inline-block; font-size: 11px; padding: 1px 7px; border-radius: 10px; margin-left: 6px; border: 1px solid #3d4d5c; color: #8fa3b1; }
.ww-tag.ww-tag-on { background: #1e3a2a; border-color: #2f6a45; color: #8fd6a8; }
.ww-tag.ww-tag-off { background: #3a2020; border-color: #6d3232; color: #e69a9a; }
.ww-tag.ww-tag-none { background: #2b2b2b; border-color: #4a4a4a; color: #9a9a9a; }

.ww-editor textarea { font-family: monospace; font-size: 12px; }
.ww-editor .ww-field { margin-bottom: 12px; }
.ww-editor .ww-hint { color: #6d8492; font-size: 12px; margin: 4px 0 0; }
.ww-editor .ww-inherit { color: #6d8492; font-size: 12px; font-family: monospace; word-break: break-all; }
.ww-advanced { border: 1px solid #3d4d5c; border-radius: 3px; padding: 8px 12px; margin-bottom: 14px; }
.ww-advanced > summary { cursor: pointer; color: #9fb6c6; }
.ww-advanced[open] > summary { margin-bottom: 4px; padding-bottom: 6px; border-bottom: 1px solid #3d4d5c; }
.ww-cols { display: flex; flex-wrap: wrap; margin: 0 -8px; }
.ww-col { flex: 1 1 240px; padding: 0 8px; }
.ww-payload { background: #1a2129; border: 1px solid #3d4d5c; border-radius: 3px; padding: 9px 12px; font-family: monospace; font-size: 12px; color: #9fb6c6; white-space: pre; overflow-x: auto; }
.ww-btnbar { margin-top: 6px; }
.ww-btnbar .btn { margin-right: 6px; }
.ww-spin { color: #6d8492; font-style: italic; }
</style>

<div class="ww-wrap">

@if(!$ready)
    <div class="row"><div class="col-xs-12">
        <div class="box box-danger"><div class="box-body">
            Win-Wings tables are missing. Re-run the extension migrations, then reload this page.
        </div></div>
    </div></div>
@else

<div id="ww-banner" class="ww-banner"></div>

{{-- ------------------------------------------------------------------ status --}}
<div class="row">
    <div class="col-xs-12">
        <div class="box">
            <div class="box-header with-border"><h3 class="box-title">Status</h3></div>
            <div class="box-body">
                <div class="callout">
                    win-wings runs Windows game servers as ordinary processes inside Job Objects. The Panel is not
                    forked; everything the daemon needs beyond a stock egg &mdash; a PowerShell install script, a
                    Windows startup command, a stop it can actually perform, and which runtime the host must provide
                    &mdash; is served from here over <code>/api/remote/windows/</code>, authenticated with the node
                    token the daemon already holds.
                </div>

                <ul class="ww-diag">
                    <li>
                        <span class="ww-dot {{ $diagnostics['ping'] ? 'ww-on' : 'ww-off' }}"></span>
                        <span class="ww-txt">
                            <strong>Windows API {{ $diagnostics['ping'] ? 'registered' : 'NOT registered' }}</strong>
                            @if($diagnostics['ping'])
                                <small><code>GET /api/remote/windows/ping</code> is being served, so a node running with
                                <code>runtime.require_windows_profile: true</code> will start.</small>
                            @else
                                <small>
                                    The line the installer adds to <code>routes/api-remote.php</code> is gone &mdash;
                                    usually because a panel upgrade replaced that file. Every win-wings node refuses to
                                    boot until it is back. Re-run <code>blueprint -install axwinwings</code>.
                                </small>
                            @endif
                        </span>
                    </li>
                    <li>
                        <span class="ww-dot {{ $diagnostics['install_override'] ? 'ww-on' : ($settings['override_install_endpoint'] ? 'ww-off' : 'ww-warn') }}"></span>
                        <span class="ww-txt">
                            <strong>Install endpoint {{ $diagnostics['install_override'] ? 'overridden' : 'not overridden' }}</strong>
                            <small>
                                The daemon fetches its install script from the Panel standard route
                                <code>/api/remote/servers/{uuid}/install</code>, not from the Windows one, so that route
                                has to be replaced for a Windows node to receive PowerShell instead of bash. Linux nodes
                                are handed exactly what the core controller returns.
                            </small>
                        </span>
                    </li>
                    <li>
                        <span class="ww-dot {{ $diagnostics['gate_patched'] ? 'ww-on' : 'ww-warn' }}"></span>
                        <span class="ww-txt">
                            <strong>Egg gate {{ $diagnostics['gate_patched'] ? 'installed' : 'not installed' }}</strong>
                            <small>
                                Optional. Without it, creating a server with an unprofiled egg on a Windows node still
                                fails &mdash; but only after the record exists, with the user watching a server that
                                says installing and never will.
                            </small>
                        </span>
                    </li>
                    <li>
                        <span class="ww-dot {{ $diagnostics['windows_nodes'] > 0 ? 'ww-on' : 'ww-warn' }}"></span>
                        <span class="ww-txt">
                            <strong>{{ $diagnostics['windows_nodes'] }} Windows node{{ $diagnostics['windows_nodes'] === 1 ? '' : 's' }},
                            {{ $diagnostics['profiles_enabled'] }} enabled profile{{ $diagnostics['profiles_enabled'] === 1 ? '' : 's' }}</strong>
                            <small>
                                @if($diagnostics['profiles'] > $diagnostics['profiles_enabled'])
                                    {{ $diagnostics['profiles'] - $diagnostics['profiles_enabled'] }} saved but disabled, which reads to a node as no profile at all.
                                @else
                                    A node is flagged automatically the first time it calls the Windows API.
                                @endif
                            </small>
                        </span>
                    </li>
                </ul>
            </div>
        </div>
    </div>
</div>

{{-- ---------------------------------------------------------------- behaviour --}}
<div class="row">
    <div class="col-md-6">
        <div class="box">
            <div class="box-header with-border"><h3 class="box-title">Behaviour</h3></div>
            <div class="box-body">
                <div class="ww-check">
                    <label><input type="checkbox" id="ww-gate" {{ $settings['gate_enabled'] ? 'checked' : '' }}> Block unprofiled eggs at creation</label>
                    <p class="text-muted">
                        Refuse to create a server on a Windows node when its egg has no enabled profile. The daemon
                        refuses it either way; this moves the failure to somewhere a person can read it.
                    </p>
                </div>
                <div class="ww-field" style="margin-left: 22px;">
                    <input type="text" class="form-control" id="ww-gate-message" value="{{ $settings['gate_message'] }}" maxlength="191">
                    <p class="ww-hint text-muted">Shown when the gate fires. The egg name is appended automatically.</p>
                </div>
                <hr>
                <div class="ww-check">
                    <label><input type="checkbox" id="ww-autoflag" {{ $settings['auto_flag_nodes'] ? 'checked' : '' }}> Detect Windows nodes automatically</label>
                    <p class="text-muted">
                        Nothing but win-wings calls <code>/api/remote/windows/</code>, so the first such call from a node
                        is proof of what it runs. A node whose flag you have turned off by hand is never turned back on.
                    </p>
                </div>
                <div class="ww-check">
                    <label><input type="checkbox" id="ww-override" {{ $settings['override_install_endpoint'] ? 'checked' : '' }}> Serve PowerShell install scripts to Windows nodes</label>
                    <p class="text-muted">
                        Turn this off only to compare a node against an unmodified Panel. With it off, a Windows node
                        receives each egg Linux script and runs it through PowerShell, which fails at the first
                        <code>apt-get</code>.
                    </p>
                </div>
            </div>
            <div class="box-footer"><button type="button" class="btn btn-primary" id="ww-save-settings">Save</button></div>
        </div>
    </div>

    <div class="col-md-6">
        <div class="box">
            <div class="box-header with-border"><h3 class="box-title">Runtime names</h3></div>
            <div class="box-body">
                <p class="text-muted" style="margin-top:0;">
                    A profile runtime names an entry in the node <code>runtime.runtimes</code> map in
                    <code>config.yml</code>. The daemon resolves it to a directory, puts that directory
                    <code>bin</code> first on the server PATH and exports <code>RUNTIME_PATH</code> &mdash; which is how
                    several Java versions coexist on one host without an egg getting whichever was installed last.
                </p>
                <p class="text-muted">
                    A name the node does not recognise is not an error there: the server simply inherits the host PATH.
                    Correct for an egg that needs no runtime, silently wrong for one that does &mdash; which is why
                    these are a list and the profile editor offers a dropdown. One name per line.
                </p>
                <textarea class="form-control" id="ww-runtimes" rows="7">{{ implode(PHP_EOL, $runtimes) }}</textarea>
            </div>
            <div class="box-footer"><button type="button" class="btn btn-primary" id="ww-save-runtimes">Save</button></div>
        </div>
    </div>
</div>

{{-- --------------------------------------------------------------------- nodes --}}
<div class="row">
    <div class="col-xs-12">
        <div class="box">
            <div class="box-header with-border"><h3 class="box-title">Nodes</h3></div>
            <div class="box-body">
                @if($nodes->isEmpty())
                    <p class="text-muted" style="margin:0;">No nodes exist yet.</p>
                @else
                <table class="ww-table">
                    <thead>
                        <tr>
                            <th style="width: 28%;">Node</th>
                            <th style="width: 27%;">FQDN</th>
                            <th style="width: 30%;">Windows API</th>
                            <th style="width: 15%; text-align: right;">Treat as Windows</th>
                        </tr>
                    </thead>
                    <tbody>
                    @foreach($nodes as $node)
                        <tr>
                            <td>{{ $node->name }}</td>
                            <td class="ww-mono">{{ $node->fqdn }}</td>
                            <td class="text-muted">
                                @if($node->last_seen_human)
                                    last call {{ $node->last_seen_human }} ({{ $node->last_seen_endpoint }})
                                @elseif($node->known)
                                    flagged by hand; has never called
                                @else
                                    has never called
                                @endif
                            </td>
                            <td style="text-align: right;">
                                <span class="ww-check" style="margin:0; display:inline-block;">
                                    <input type="checkbox" class="ww-node-toggle" data-node="{{ $node->id }}" {{ $node->is_windows ? 'checked' : '' }}>
                                </span>
                            </td>
                        </tr>
                    @endforeach
                    </tbody>
                </table>
                @endif
            </div>
        </div>
    </div>
</div>

{{-- ------------------------------------------------------------ servers at risk --}}
<div class="row">
    <div class="col-xs-12">
        <div class="box">
            <div class="box-header with-border">
                <h3 class="box-title">Servers with no profile</h3>
                <div class="box-tools pull-right">
                    <button type="button" class="btn btn-xs btn-default" id="ww-exposure-load">Check</button>
                </div>
            </div>
            <div class="box-body">
                <p class="text-muted" style="margin-top:0;">
                    Servers already sitting on a Windows node whose egg has no enabled profile. Each one is refused by
                    its node every time it is started, and the reason appears only in that node log.
                </p>
                <div id="ww-exposure"></div>
            </div>
        </div>
    </div>
</div>

{{-- ------------------------------------------------------------------ profiles --}}
<div class="row">
    <div class="col-xs-12">
        <div class="box">
            <div class="box-header with-border"><h3 class="box-title">Egg profiles</h3></div>
            <div class="box-body">
                <p class="text-muted" style="margin-top:0;">
                    One profile per egg. Every field left empty falls back to the egg own value, so a profile can be as
                    small as &ldquo;this egg needs java-21&rdquo;. The exception is stopping: a POSIX signal inherited
                    from a Linux egg means nothing here, so an egg that stops that way needs either a stop command or a
                    Ctrl+C interrupt configured below, or its servers are killed rather than asked to shut down.
                </p>

                @foreach($nests as $nest)
                    <div class="ww-nest-title">{{ $nest->name }}</div>
                    @forelse($nest->eggs as $egg)
                        @php($p = $profiles->get($egg->id))
                        <div class="ww-egg" id="ww-egg-{{ $egg->id }}">
                            <div class="ww-egg-head" data-egg="{{ $egg->id }}">
                                <span class="ww-egg-name">{{ $egg->name }}</span>
                                <span class="ww-egg-meta" id="ww-tags-{{ $egg->id }}">
                                    @if(!$p)
                                        <span class="ww-tag ww-tag-none">no profile</span>
                                    @else
                                        @if($p->runtime)<span class="ww-tag">{{ $p->runtime }}</span>@endif
                                        @if($p->pseudo_console)<span class="ww-tag">ConPTY</span>@endif
                                        @if($p->install_override)<span class="ww-tag">PowerShell install</span>@endif
                                        @if(($p->console_source_type ?? '') === 'file')<span class="ww-tag">log file</span>@endif
                                        @if(($p->console_command_type ?? '') === 'telnet')<span class="ww-tag">TCP console</span>@endif
                                        @if($p->prestart_override ?? false)<span class="ww-tag">pre-start script</span>@endif
                                        @if($p->prestop_override ?? false)<span class="ww-tag">pre-stop script</span>@endif
                                        <span class="ww-tag {{ $p->enabled ? 'ww-tag-on' : 'ww-tag-off' }}">{{ $p->enabled ? 'enabled' : 'disabled' }}</span>
                                    @endif
                                </span>
                            </div>
                            <div class="ww-egg-body" id="ww-body-{{ $egg->id }}"></div>
                        </div>
                    @empty
                        <p class="text-muted">This nest has no eggs.</p>
                    @endforelse
                @endforeach
            </div>
        </div>
    </div>
</div>

@endif
</div>

@if($ready)
<script>
/*
 * Vanilla, and bound immediately rather than on DOMContentLoaded: the panel
 * admin layout loads jQuery after the content block, so nothing here can assume
 * $ exists.
 */
(function () {
    var ROOT = @json($root);
    var API = @json($api);
    var CSRF = @json(csrf_token());
    var RUNTIMES = @json($runtimes);

    // Built rather than written as an escape, which Blueprint would eat.
    var NL = String.fromCharCode(10);

    var banner = document.getElementById('ww-banner');
    var templates = null;
    var loaded = {};

    function say(message, good) {
        banner.textContent = message;
        banner.className = 'ww-banner ' + (good ? 'ww-good' : 'ww-bad');
        window.scrollTo({ top: 0, behavior: 'smooth' });
    }

    function post(fields) {
        var body = new URLSearchParams();

        Object.keys(fields).forEach(function (key) {
            var value = fields[key];

            if (value === true) { value = '1'; }
            else if (value === false) { value = '0'; }
            else if (value === null || value === undefined) { value = ''; }

            body.append(key, value);
        });

        return fetch(ROOT, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/x-www-form-urlencoded',
                'X-CSRF-TOKEN': CSRF,
                'Accept': 'application/json'
            },
            body: body.toString(),
            credentials: 'same-origin'
        }).then(function (response) {
            return response.json().catch(function () {
                return {
                    success: false,
                    message: 'The panel answered ' + response.status + ' with something that is not JSON. Check the panel log.'
                };
            });
        });
    }

    function get(path) {
        return fetch(API + path, {
            headers: { 'Accept': 'application/json' },
            credentials: 'same-origin'
        }).then(function (response) {
            if (!response.ok) {
                throw new Error('Request failed with ' + response.status);
            }

            return response.json();
        });
    }

    function escapeHtml(value) {
        var holder = document.createElement('div');
        holder.textContent = (value === null || value === undefined) ? '' : String(value);

        return holder.innerHTML;
    }

    // ------------------------------------------------------------- behaviour

    document.getElementById('ww-save-settings').addEventListener('click', function () {
        var button = this;
        button.disabled = true;

        post({
            action: 'save_settings',
            gate_enabled: document.getElementById('ww-gate').checked,
            auto_flag_nodes: document.getElementById('ww-autoflag').checked,
            override_install_endpoint: document.getElementById('ww-override').checked,
            gate_message: document.getElementById('ww-gate-message').value
        }).then(function (result) {
            button.disabled = false;
            say(result.message, result.success);
        });
    });

    document.getElementById('ww-save-runtimes').addEventListener('click', function () {
        var button = this;
        button.disabled = true;

        post({
            action: 'save_runtimes',
            runtimes: document.getElementById('ww-runtimes').value
        }).then(function (result) {
            button.disabled = false;
            say(result.message, result.success);

            if (!result.success || !result.runtimes) {
                return;
            }

            RUNTIMES = result.runtimes;
            document.getElementById('ww-runtimes').value = RUNTIMES.join(NL);

            // Any open editor is showing a dropdown built from the old list.
            Object.keys(loaded).forEach(function (eggId) {
                var body = document.getElementById('ww-body-' + eggId);

                if (body && body.classList.contains('ww-open')) {
                    delete loaded[eggId];
                    openEgg(parseInt(eggId, 10), true);
                }
            });
        });
    });

    // ----------------------------------------------------------------- nodes

    Array.prototype.forEach.call(document.querySelectorAll('.ww-node-toggle'), function (input) {
        input.addEventListener('change', function () {
            var checkbox = this;
            checkbox.disabled = true;

            post({
                action: 'set_node',
                node_id: checkbox.getAttribute('data-node'),
                is_windows: checkbox.checked
            }).then(function (result) {
                checkbox.disabled = false;
                say(result.message, result.success);

                if (!result.success) {
                    checkbox.checked = !checkbox.checked;
                }
            });
        });
    });

    // -------------------------------------------------------------- exposure

    document.getElementById('ww-exposure-load').addEventListener('click', function () {
        var target = document.getElementById('ww-exposure');
        target.innerHTML = '<p class="ww-spin">Checking...</p>';

        get('/exposure').then(function (data) {
            if (!data.servers.length) {
                target.innerHTML = '<p class="text-muted" style="margin:0;">Nothing. Every server on a Windows node has a profile for its egg.</p>';
                return;
            }

            var rows = data.servers.map(function (server) {
                return '<tr><td>' + escapeHtml(server.name) + '</td>'
                    + '<td class="ww-mono">' + escapeHtml(server.uuid_short) + '</td>'
                    + '<td>' + escapeHtml(server.node) + '</td>'
                    + '<td>' + escapeHtml(server.egg) + '</td>'
                    + '<td style="text-align:right;"><button type="button" class="btn btn-xs btn-default" data-jump="'
                    + server.egg_id + '">Write a profile</button></td></tr>';
            }).join('');

            target.innerHTML = '<table class="ww-table"><thead><tr>'
                + '<th>Server</th><th>UUID</th><th>Node</th><th>Egg</th><th></th>'
                + '</tr></thead><tbody>' + rows + '</tbody></table>';

            Array.prototype.forEach.call(target.querySelectorAll('[data-jump]'), function (button) {
                button.addEventListener('click', function () {
                    openEgg(parseInt(this.getAttribute('data-jump'), 10), true);
                });
            });
        }).catch(function (error) {
            target.innerHTML = '<p class="text-muted" style="margin:0;">' + escapeHtml(error.message) + '</p>';
        });
    });

    // -------------------------------------------------------------- profiles

    Array.prototype.forEach.call(document.querySelectorAll('.ww-egg-head'), function (head) {
        head.addEventListener('click', function () {
            openEgg(parseInt(this.getAttribute('data-egg'), 10), false);
        });
    });

    function openEgg(eggId, keepOpen) {
        var body = document.getElementById('ww-body-' + eggId);

        if (!body) {
            return;
        }

        if (body.classList.contains('ww-open') && !keepOpen) {
            body.classList.remove('ww-open');
            return;
        }

        body.classList.add('ww-open');

        if (loaded[eggId]) {
            body.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
            return;
        }

        body.innerHTML = '<p class="ww-spin">Loading...</p>';

        ensureTemplates().then(function () {
            return get('/egg/' + eggId);
        }).then(function (data) {
            loaded[eggId] = true;
            renderEditor(body, eggId, data);
            body.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        }).catch(function (error) {
            body.innerHTML = '<p class="text-muted">' + escapeHtml(error.message) + '</p>';
        });
    }

    function ensureTemplates() {
        if (templates) {
            return Promise.resolve(templates);
        }

        return get('/templates').then(function (data) {
            templates = data;
            return templates;
        });
    }

    /*
     * Values are assigned to DOM properties rather than interpolated into the
     * markup: an install script is arbitrary text and an egg startup line
     * routinely contains quotes and braces.
     */
    function renderEditor(body, eggId, data) {
        var profile = data.profile || {
            enabled: true,
            runtime: '',
            startup: '',
            working_dir: '',
            stop_type: null,
            stop_value: '',
            pseudo_console: false,
            install_override: false,
            install_script: '',
            notes: '',
            console_source_type: '',
            console_source_path: '',
            console_source_encoding: '',
            console_command_type: '',
            console_command_host: '',
            console_command_port: '',
            console_command_password: '',
            console_connect_timeout: null,
            prestart_override: false,
            prestart_script: '',
            prestop_override: false,
            prestop_script: ''
        };

        // The advanced section starts folded, except on a profile that already
        // uses it: hiding somebody own configuration behind a disclosure they
        // have to remember to open is how a setting gets edited by accident.
        var advancedInUse = !!(profile.console_source_type
            || profile.console_command_type
            || profile.prestart_override
            || (profile.prestart_script || '').trim() !== ''
            || profile.prestop_override
            || (profile.prestop_script || '').trim() !== '');

        var known = RUNTIMES.slice();

        // A profile written before a name was taken off the list still has to be
        // editable without silently becoming something else.
        if (profile.runtime && known.indexOf(profile.runtime) === -1) {
            known.push(profile.runtime);
        }

        var runtimeOptions = ['<option value="">(none, the egg needs no runtime on PATH)</option>'];
        known.forEach(function (name) {
            runtimeOptions.push('<option value="' + escapeHtml(name) + '">' + escapeHtml(name) + '</option>');
        });

        var templateOptions = ['<option value="">Insert a starter script...</option>'];
        Object.keys(templates).forEach(function (key) {
            templateOptions.push('<option value="' + escapeHtml(key) + '">' + escapeHtml(templates[key].label) + '</option>');
        });

        body.innerHTML = ''
            + '<div class="ww-editor">'
            + '  <div class="ww-cols">'
            + '    <div class="ww-col">'
            + '      <div class="ww-field">'
            + '        <label>Runtime</label>'
            + '        <select class="form-control" data-role="runtime">' + runtimeOptions.join('') + '</select>'
            + '        <p class="ww-hint">Sent as <code>runtime</code>, and to the install script as'
            + '        <code>INSTALL_RUNTIME</code>. Docker image on the egg:'
            + '        <span class="ww-inherit">' + escapeHtml(data.egg.container) + '</span></p>'
            + '      </div>'
            + '    </div>'
            + '    <div class="ww-col">'
            + '      <div class="ww-field">'
            + '        <label>Stop</label>'
            + '        <div style="display:flex; gap:6px;">'
            + '          <select class="form-control" data-role="stop_type" style="flex:0 0 170px;">'
            + '            <option value="">Inherit from the egg</option>'
            + '            <option value="command">Command to stdin</option>'
            + '            <option value="signal">Signal</option>'
            + '          </select>'
            + '          <input type="text" class="form-control" data-role="stop_value" maxlength="191">'

            // A select rather than a text box, because the daemon can deliver
            // exactly one signal. Free text here is how a profile ends up looking
            // configured while every stop is really a kill.
            + '          <select class="form-control" data-role="stop_signal">'
            + '            <option value="ctrl_c">Ctrl+C (interrupt)</option>'
            + '          </select>'
            + '        </div>'
            + '        <p class="ww-hint" data-role="stop_warning" style="display:none; color:#d29a2b;"></p>'
            + '        <p class="ww-hint">Stop on the egg: <span class="ww-inherit">'
            + escapeHtml(data.egg.stop || '(none)') + '</span><br>'
            + '        Windows has no signals, but a console interrupt (Ctrl+C) can be delivered to a server that was'
            + '        given a pseudo console, and most console servers shut down cleanly on one. A stop command on'
            + '        stdin is still preferred wherever the server has one, because it needs no pseudo console.</p>'
            + '      </div>'
            + '    </div>'
            + '  </div>'

            + '  <div class="ww-field">'
            + '    <label>Windows startup command</label>'
            + '    <textarea class="form-control" rows="2" data-role="startup" spellcheck="false"></textarea>'
            + '    <p class="ww-hint">Executed directly rather than through a shell, so <code>&amp;&amp;</code>,'
            + '    <code>|</code> and <code>&gt;</code> are not interpreted. Both <code>@{{VAR}}</code> and'
            + '    <code>${VAR}</code> are substituted. Empty uses the egg own line:<br>'
            + '    <span class="ww-inherit">' + escapeHtml(data.egg.startup) + '</span></p>'
            + '  </div>'

            + '  <div class="ww-field">'
            + '    <label>Working directory <span class="text-muted">(optional)</span></label>'
            + '    <input type="text" class="form-control" data-role="working_dir" spellcheck="false" placeholder="the server data directory">'
            + '    <p class="ww-hint">Where the server process is started, relative to its data directory, and'
            + '    relative to which the startup command above is resolved. Empty is the data directory itself,'
            + '    which is what nearly every egg wants.<br>'
            + '    Set it for a game that builds its own paths by climbing out of wherever it was started &mdash;'
            + '    a log root at <code>..\Logs</code> is the usual shape. From the data directory that lands in'
            + '    the server root, which the server account cannot write and must not be able to. Naming the'
            + '    folder the game was installed into puts it back inside the sandbox. With a working directory'
            + '    of <code>ServerFile</code>, the startup command is <code>MyServer.exe</code>, not'
            + '    <code>ServerFile\MyServer.exe</code>.<br>'
            + '    Applies to the server process only: a steamcmd update and the log tail below both stay at the'
            + '    data directory, since they are what install and read the content this names.</p>'
            + '  </div>'

            + '  <div class="ww-check">'
            + '    <label><input type="checkbox" data-role="pseudo_console"> Allocate a ConPTY instead of pipes</label>'
            + '    <p class="text-muted">Only for a process that inspects its own stdout and behaves differently when it is not a console, steamcmd being the usual one. It costs a stream of VT escapes in place of clean lines.</p>'
            + '  </div>'

            + '  <hr>'

            /*
             * Advanced settings, folded away by default and opened when the
             * profile already uses them.
             *
             * Everything here exists for the same small family of eggs: the ones
             * whose Linux startup line is a shell pipeline rather than a command,
             * because the game writes nothing to stdout and reads nothing from
             * stdin. Nearly every egg wants none of it, and an editor that put
             * these fields in front of everybody would suggest otherwise.
             */
            + '  <details class="ww-advanced"' + (advancedInUse ? ' open' : '') + '>'
            + '    <summary><strong>Advanced settings</strong> &mdash; console redirection and a pre-start script</summary>'
            + '    <p class="ww-hint" style="margin-top:8px;">Only for a game that does not use its own stdin and stdout &mdash; one that writes its log to a file and takes commands on a port it opens itself. On Linux those eggs are started through a shell pipeline (<code>tail -F</code> into stdout, a telnet client on stdin); here the node does both jobs itself, so the game process stays the process it supervises and the exit code stays the game own. Leave all of it alone otherwise.</p>'

            + '    <div class="ww-cols">'
            + '      <div class="ww-col">'
            + '        <div class="ww-field">'
            + '          <label>Console output</label>'
            + '          <select class="form-control" data-role="console_source_type">'
            + '            <option value="">The process own output (normal)</option>'
            + '            <option value="file">Also follow a log file</option>'
            + '          </select>'
            + '          <div data-role="source_fields" style="display:none; margin-top:6px;">'
            + '            <input type="text" class="form-control" data-role="console_source_path" maxlength="512" placeholder="Logs/server/server.log">'
            + '            <select class="form-control" data-role="console_source_encoding" style="margin-top:6px;">'
            + '              <option value="">utf-8</option>'
            + '              <option value="utf-16le">utf-16le</option>'
            + '              <option value="utf-16be">utf-16be</option>'
            + '            </select>'
            + '            <p class="ww-hint">Relative to the server directory; an absolute path is refused. Both <code>@{{VAR}}</code> and <code>${VAR}</code> are substituted. The node follows the <em>name</em>, so a log the game deletes and recreates on boot is followed into the new file, and whatever was there from the last run is not replayed. Pick utf-16 only if the console fills with gaps between every character, which is what a .NET server writing its log on the defaults produces.</p>'
            + '          </div>'
            + '        </div>'
            + '      </div>'
            + '      <div class="ww-col">'
            + '        <div class="ww-field">'
            + '          <label>Console commands</label>'
            + '          <select class="form-control" data-role="console_command_type">'
            + '            <option value="">The process stdin (normal)</option>'
            + '            <option value="telnet">A TCP console the server listens on</option>'
            + '          </select>'
            + '          <div data-role="command_fields" style="display:none; margin-top:6px;">'
            + '            <input type="text" class="form-control" data-role="console_command_port" maxlength="64" placeholder="@{{TELNET_PORT}}">'
            + '            <input type="text" class="form-control" data-role="console_command_password" maxlength="191" style="margin-top:6px;" placeholder="password, if the console asks for one">'
            + '            <div style="display:flex; gap:6px; margin-top:6px;">'
            + '              <input type="text" class="form-control" data-role="console_command_host" maxlength="191" placeholder="127.0.0.1">'
            + '              <input type="number" class="form-control" data-role="console_connect_timeout" min="0" max="3600" placeholder="300" style="flex:0 0 120px;">'
            + '            </div>'
            + '            <p class="ww-hint">The port is usually an egg variable, since every server on the node has a different one. Everything the console sends back appears in the server console, its banner included &mdash; which is how you tell a connected channel from a silent one. The stop command goes here too, so this egg stop must be a command rather than a signal. The number beside the host is how long the node waits for the port to open after the server starts: a world-loading time, not a network timeout, so leave it blank unless five minutes is not enough.</p>'
            + '          </div>'
            + '        </div>'
            + '      </div>'
            + '    </div>'

            + '    <p class="ww-hint" data-role="console_warning" style="display:none; color:#d29a2b;"></p>'

            + '    <div class="ww-check">'
            + '      <label><input type="checkbox" data-role="prestart_override"> Run a PowerShell script before every boot</label>'
            + '      <p class="text-muted">Runs to completion before the startup command, in the server directory, as the server own account, with its output on the console &mdash; after any steamcmd update, so it can patch files the update has just replaced. For preparing a server, not for running one: writing a config file out of the egg variables is what it is for. A non-zero exit is logged and the server starts anyway, because a server with a stale config is more use than one that will not boot.</p>'
            + '    </div>'

            + '    <div class="ww-field" data-role="prestart_field" style="display:none;">'
            + '      <label>Pre-start script</label>'
            + '      <textarea class="form-control" rows="12" data-role="prestart_script" spellcheck="false"></textarea>'
            + '      <p class="ww-hint">The server directory is the working directory and <code>$env:SERVER_DIR</code>. The egg variables are in the environment, same as during an install.</p>'
            + '    </div>'

            + '    <div class="ww-check">'
            + '      <label><input type="checkbox" data-role="prestop_override"> Run a PowerShell script when the server is stopped</label>'
            + '      <p class="text-muted">Runs to completion when a stop is requested, before the stop method above is tried, in the server directory, as the server own account, with its output on the console. For the servers whose clean shutdown is neither a line on stdin nor a console interrupt &mdash; an RCON command, a web endpoint, a save that has to be asked for first. If the server exits during or after the script the stop is done; otherwise the stop method above follows as if the script had not run. A script that overruns the stop timeout is killed and the stop carries on. Not run when a server is killed, nor when a stop arrives before the server has finished starting.</p>'
            + '    </div>'

            + '    <div class="ww-field" data-role="prestop_field" style="display:none;">'
            + '      <label>Pre-stop script</label>'
            + '      <textarea class="form-control" rows="12" data-role="prestop_script" spellcheck="false"></textarea>'
            + '      <p class="ww-hint">Same environment as the pre-start script, plus <code>$env:SERVER_PID</code>, the process id of the server being stopped &mdash; wait on it after asking the server to exit, so the node knows the script worked.</p>'
            + '    </div>'
            + '  </details>'

            + '  <hr>'

            + '  <div class="ww-check">'
            + '    <label><input type="checkbox" data-role="install_override"> Serve a PowerShell install script for this egg</label>'
            + '    <p class="text-muted">Off means Windows nodes are handed the egg own script, which for almost every egg is bash and will fail. The egg is never modified, so it keeps working on Linux nodes.</p>'
            + '  </div>'

            + '  <div class="ww-field">'
            + '    <div style="display:flex; align-items:center; gap:8px; margin-bottom:6px;">'
            + '      <label style="margin:0; flex:1 1 auto;">Install script</label>'
            + '      <select class="form-control" data-role="template" style="width:auto;">' + templateOptions.join('') + '</select>'
            + '      <button type="button" class="btn btn-xs btn-default" data-role="show_egg_script">Load the egg own script</button>'
            + '    </div>'
            + '    <textarea class="form-control" rows="18" data-role="install_script" spellcheck="false"></textarea>'
            + '    <p class="ww-hint">Runs as the server own unprivileged account with <code>$env:SERVER_DIR</code> as the working directory. No apt, no curl, no tar. A non-zero exit fails the installation.</p>'
            + '  </div>'

            + '  <div class="ww-field">'
            + '    <label>Notes</label>'
            + '    <input type="text" class="form-control" data-role="notes" maxlength="255">'
            + '  </div>'

            + '  <div class="ww-check">'
            + '    <label><input type="checkbox" data-role="enabled"> Enabled</label>'
            + '    <p class="text-muted">A disabled profile is indistinguishable from no profile: nodes are answered 404 and refuse the server.</p>'
            + '  </div>'

            + '  <div class="ww-btnbar">'
            + '    <button type="button" class="btn btn-primary" data-role="save">Save profile</button>'
            + '    <button type="button" class="btn btn-default" data-role="preview">Show what the node receives</button>'
            + '    <button type="button" class="btn btn-danger pull-right" data-role="delete">Delete profile</button>'
            + '  </div>'
            + '  <div style="clear:both;"></div>'
            + '  <div class="ww-payload" data-role="payload" style="display:none; margin-top:10px;"></div>'
            + '</div>';

        var field = function (role) {
            return body.querySelector('[data-role="' + role + '"]');
        };

        field('runtime').value = profile.runtime || '';
        field('startup').value = profile.startup || '';
        field('startup').placeholder = data.egg.startup || '';
        field('working_dir').value = profile.working_dir || '';
        field('stop_type').value = profile.stop_type || '';
        field('stop_value').value = profile.stop_type === 'signal' ? '' : (profile.stop_value || '');

        // Anything stored before the interrupt was the only accepted signal value
        // lands on the one option there is, which is also what saving will store.
        field('stop_signal').value = 'ctrl_c';
        field('pseudo_console').checked = !!profile.pseudo_console;
        field('install_override').checked = !!profile.install_override;
        field('install_script').value = profile.install_script || '';
        field('notes').value = profile.notes || '';
        field('notes').placeholder = 'Why this profile looks the way it does';
        field('enabled').checked = !!profile.enabled;

        field('console_source_type').value = profile.console_source_type === 'file' ? 'file' : '';
        field('console_source_path').value = profile.console_source_path || '';
        field('console_source_encoding').value = profile.console_source_encoding || '';
        field('console_command_type').value = profile.console_command_type === 'telnet' ? 'telnet' : '';
        field('console_command_host').value = profile.console_command_host || '';
        field('console_command_port').value = profile.console_command_port || '';
        field('console_command_password').value = profile.console_command_password || '';
        field('console_connect_timeout').value = profile.console_connect_timeout || '';
        field('prestart_override').checked = !!profile.prestart_override;
        field('prestart_script').value = profile.prestart_script || '';
        field('prestop_override').checked = !!profile.prestop_override;
        field('prestop_script').value = profile.prestop_script || '';

        /*
         * Show only the fields the chosen console arrangement actually uses, and
         * warn about the one combination that saves happily and then fails on the
         * node: a signal stop on a server whose console is a socket. There is
         * nothing to raise an interrupt on, so the stop becomes a kill and the
         * world is not saved.
         */
        function syncConsole() {
            var source = field('console_source_type').value;
            var commands = field('console_command_type').value;

            field('source_fields').style.display = source === 'file' ? '' : 'none';
            field('command_fields').style.display = commands === 'telnet' ? '' : 'none';
            field('prestart_field').style.display = field('prestart_override').checked ? '' : 'none';
            field('prestop_field').style.display = field('prestop_override').checked ? '' : 'none';

            var warning = field('console_warning');
            var message = '';

            if (commands === 'telnet' && field('stop_type').value !== 'command') {
                message = 'This server takes its commands over a socket, so its stop has to be a command sent there. '
                    + 'A signal stop has no console to interrupt and the node will kill the server instead, losing whatever it had not saved.';
            }

            warning.style.display = message ? '' : 'none';
            warning.textContent = message;
        }

        field('console_source_type').addEventListener('change', syncConsole);
        field('console_command_type').addEventListener('change', syncConsole);
        field('prestart_override').addEventListener('change', syncConsole);
        field('prestop_override').addEventListener('change', syncConsole);
        field('stop_type').addEventListener('change', syncConsole);
        syncConsole();

        // Which of the two stop controls is the live one.
        function stopValue() {
            return field('stop_type').value === 'signal'
                ? field('stop_signal').value
                : field('stop_value').value;
        }

        /*
         * The stop value is a text box for a command and a fixed list for a
         * signal, so both controls exist and one is shown at a time.
         *
         * The pseudo console warning is guidance only -- the save is refused
         * server side either way. It is here so the refusal is not the first
         * time anyone hears about the requirement.
         */
        function syncStop() {
            var type = field('stop_type').value;
            var text = field('stop_value');
            var list = field('stop_signal');
            var warning = field('stop_warning');

            text.style.display = (type === 'signal') ? 'none' : '';
            list.style.display = (type === 'signal') ? '' : 'none';

            text.disabled = (type !== 'command');
            text.placeholder = type === 'command' ? 'stop' : 'inherited from the egg';

            if (type === 'signal' && !field('pseudo_console').checked) {
                warning.style.display = '';
                warning.textContent = 'Ctrl+C needs the pseudo console turned on, further down. Without a console there is nothing to deliver the interrupt through and the stop becomes a kill.';
            } else {
                warning.style.display = 'none';
                warning.textContent = '';
            }
        }

        field('stop_type').addEventListener('change', syncStop);
        field('pseudo_console').addEventListener('change', syncStop);
        syncStop();

        field('template').addEventListener('change', function () {
            var key = this.value;
            this.value = '';

            if (!key || !templates[key]) {
                return;
            }

            var box = field('install_script');

            if (box.value.trim() !== '' && !window.confirm('Replace the script in the editor with the ' + templates[key].label + ' starter?')) {
                return;
            }

            box.value = templates[key].script;
            field('install_override').checked = true;
        });

        field('show_egg_script').addEventListener('click', function () {
            var box = field('install_script');
            var script = data.egg.script || '';

            if (script.trim() === '') {
                say('This egg has no install script of its own.', false);
                return;
            }

            if (box.value.trim() !== '' && !window.confirm('Replace the script in the editor with the egg own bash script, to port from?')) {
                return;
            }

            box.value = script;
        });

        field('preview').addEventListener('click', function () {
            var out = field('payload');
            out.style.display = 'block';
            out.textContent = describe(body, data);
        });

        field('save').addEventListener('click', function () {
            var button = this;
            button.disabled = true;

            post({
                action: 'save_profile',
                egg_id: eggId,
                runtime: field('runtime').value,
                startup: field('startup').value,
                working_dir: field('working_dir').value,
                stop_type: field('stop_type').value,
                stop_value: stopValue(),
                pseudo_console: field('pseudo_console').checked,
                install_override: field('install_override').checked,
                install_script: field('install_script').value,
                notes: field('notes').value,
                enabled: field('enabled').checked,
                console_source_type: field('console_source_type').value,
                console_source_path: field('console_source_path').value,
                console_source_encoding: field('console_source_encoding').value,
                console_command_type: field('console_command_type').value,
                console_command_host: field('console_command_host').value,
                console_command_port: field('console_command_port').value,
                console_command_password: field('console_command_password').value,
                console_connect_timeout: field('console_connect_timeout').value,
                prestart_override: field('prestart_override').checked,
                prestart_script: field('prestart_script').value,
                prestop_override: field('prestop_override').checked,
                prestop_script: field('prestop_script').value
            }).then(function (result) {
                button.disabled = false;
                say(result.message, result.success);

                if (result.success) {
                    updateTags(eggId, result.summary);
                }
            });
        });

        field('delete').addEventListener('click', function () {
            if (!window.confirm('Delete this profile? Windows nodes will then refuse every server using this egg.')) {
                return;
            }

            var button = this;
            button.disabled = true;

            post({ action: 'delete_profile', egg_id: eggId }).then(function (result) {
                button.disabled = false;
                say(result.message, result.success);

                if (!result.success) {
                    return;
                }

                updateTags(eggId, null);
                delete loaded[eggId];

                var container = document.getElementById('ww-body-' + eggId);
                container.innerHTML = '';
                container.classList.remove('ww-open');
            });
        });
    }

    /*
     * The JSON a node would be handed, built from the form as it stands rather
     * than from what was saved: the point is to see the effect of a change
     * before committing to it.
     */
    function describe(body, data) {
        var field = function (role) {
            return body.querySelector('[data-role="' + role + '"]');
        };

        var payload = {
            runtime: field('runtime').value,
            startup: field('startup').value,
            working_dir: field('working_dir').value,
            pseudo_console: field('pseudo_console').checked
        };

        if (field('stop_type').value) {
            payload.stop = {
                type: field('stop_type').value,
                value: field('stop_type').value === 'signal' ? field('stop_signal').value : field('stop_value').value
            };
        }

        // Each half is sent only when it is set. The node reads a missing half as
        // the ordinary arrangement, so the preview has to show it missing too.
        var consoleCfg = {};

        if (field('console_source_type').value === 'file') {
            consoleCfg.source = {
                type: 'file',
                path: field('console_source_path').value,
                encoding: field('console_source_encoding').value || 'utf-8'
            };
        }

        if (field('console_command_type').value === 'telnet') {
            consoleCfg.commands = {
                type: 'telnet',
                host: field('console_command_host').value,
                port: field('console_command_port').value,
                password: field('console_command_password').value ? '(set)' : '',
                connect_timeout_seconds: parseInt(field('console_connect_timeout').value, 10) || 0
            };
        }

        if (consoleCfg.source || consoleCfg.commands) {
            payload.console = consoleCfg;
        }

        if (field('prestart_override').checked && field('prestart_script').value.trim() !== '') {
            payload.pre_start_script = '(the ' + field('prestart_script').value.length + ' character PowerShell script above)';
        }

        if (field('prestop_override').checked && field('prestop_script').value.trim() !== '') {
            payload.pre_stop_script = '(the ' + field('prestop_script').value.length + ' character PowerShell script above)';
        }

        var overriding = field('install_override').checked && field('install_script').value.trim() !== '';

        var lines = [];

        if (!field('enabled').checked) {
            lines.push('This profile is disabled. Nodes are answered 404 and refuse the server.');
            lines.push('');
        }

        lines.push('GET /api/remote/windows/servers/{uuid}/profile');
        lines.push(JSON.stringify(payload, null, 2));
        lines.push('');
        lines.push('GET /api/remote/servers/{uuid}/install');
        lines.push(JSON.stringify({
            container_image: field('runtime').value || data.egg.container,
            entrypoint: 'powershell',
            script: overriding
                ? '(the ' + field('install_script').value.length + ' character PowerShell script above)'
                : '(the egg own script, unchanged, which is almost certainly bash)'
        }, null, 2));

        if (payload.startup === '') {
            lines.push('');
            lines.push('An empty startup means the node uses the Panel value: ' + (data.egg.startup || '(none)'));
        }

        if (payload.working_dir !== '') {
            lines.push('');
            lines.push('The server process starts in ' + payload.working_dir + ', relative to its data directory, and the startup command is resolved against it. A steamcmd update and the log tail still run from the data directory.');
        }

        if (field('stop_type').value === 'signal' && !field('pseudo_console').checked) {
            lines.push('');
            lines.push('This will not save: a Ctrl+C stop needs the pseudo console, or there is no console to deliver the interrupt through.');
        }

        if (field('stop_type').value === '') {
            lines.push('');
            lines.push('With no stop configured the node falls back to the egg. A POSIX signal there is not deliverable, and the server is killed.');
        }

        if (payload.console && payload.console.commands && field('stop_type').value !== 'command') {
            lines.push('');
            lines.push('This server takes commands over a socket but its stop is not a command, so there is nothing to send there and the node will kill it instead.');
        }

        return lines.join(NL);
    }

    function updateTags(eggId, summary) {
        var target = document.getElementById('ww-tags-' + eggId);

        if (!target) {
            return;
        }

        if (!summary) {
            target.innerHTML = '<span class="ww-tag ww-tag-none">no profile</span>';
            return;
        }

        var tags = [];

        if (summary.runtime) {
            tags.push('<span class="ww-tag">' + escapeHtml(summary.runtime) + '</span>');
        }

        if (summary.pseudo_console) {
            tags.push('<span class="ww-tag">ConPTY</span>');
        }

        if (summary.install_override) {
            tags.push('<span class="ww-tag">PowerShell install</span>');
        }

        if (summary.log_source === 'file') {
            tags.push('<span class="ww-tag">log file</span>');
        }

        if (summary.command_channel === 'telnet') {
            tags.push('<span class="ww-tag">TCP console</span>');
        }

        if (summary.prestart_override) {
            tags.push('<span class="ww-tag">pre-start script</span>');
        }

        if (summary.prestop_override) {
            tags.push('<span class="ww-tag">pre-stop script</span>');
        }

        tags.push('<span class="ww-tag ' + (summary.enabled ? 'ww-tag-on' : 'ww-tag-off') + '">'
            + (summary.enabled ? 'enabled' : 'disabled') + '</span>');

        target.innerHTML = tags.join('');
    }
})();
</script>
@endif

@endsection
