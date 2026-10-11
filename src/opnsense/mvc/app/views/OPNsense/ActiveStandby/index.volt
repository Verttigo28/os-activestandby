<script>
    $(document).ready(function () {
        mapDataToFormUI({'frm_general': '/api/activestandby/settings/get'}).done(function () {
            $('.selectpicker').selectpicker('refresh');
        });

        function esc(s) { return $('<div>').text(s === undefined || s === null ? '' : s).html(); }

        function showResult(sel, r) {
            let cls = r.error ? 'alert-danger' : (r.warning ? 'alert-warning' : 'alert-success');
            let html = esc(r.error || r.message || r.status || '');
            if (r.warning) { html += '<br>' + esc(r.warning); }
            if (r.diffs && r.diffs.length) {
                html += '<ul>' + r.diffs.map(d => '<li>' + esc(d) + '</li>').join('') + '</ul>';
            }
            if (r.spoofmac) { html += '<br><small>' + esc(r.spoofmac) + '</small>'; }
            $(sel).attr('class', 'alert ' + cls).html(html).show();
        }

        function syncText(y) {
            if (!y) { return '-'; }
            let t = y.state === 'in_sync' ? '{{ lang._('in sync') }}' : (y.state === 'behind' ? y.behind + ' {{ lang._('revision(s) behind') }}' : '{{ lang._('FAILED') }}');
            t += ' (' + (y.direction === 'sending' ? '{{ lang._('sending to standby') }}' : '{{ lang._('receiving from active') }}') + ', rev ' + y.rev + ')';
            if (y.error) { t += ' 2014 ' + y.error; }
            return t;
        }

        function setTile(id, dot, text, sub) {
            $('#' + id + 'Dot').attr('class', 'as-dot ' + dot);
            $('#' + id).text(text);
            $('#' + id + 'Sub').text(sub || '');
        }

        function renderTiles(s) {
            const paired = s.role === 'master' || s.role === 'standby';
            const standby = paired && s.state === 'standby';
            $('#asLoading').hide();
            $('#bannerKit').toggle(!!s.kit);
            if (s.kit) { $('#bannerKitTxt').text('{{ lang._('Waiting for the second firewall at') }} ' + s.kit.peer + (s.kit.error ? ' (' + s.kit.error + ')' : '')); }
            $('#hero').toggle(true);
            $('#standbyUi').toggle(standby);
            $('#fullUi').toggle(!standby);
            if (!paired) {
                setTile('tRole', 'idle', '{{ lang._('Not paired') }}', '{{ lang._('Use the setup wizard to create a pair') }}');
                setTile('tPeer', '', '-', '');
                setTile('tSync', '', '-', '');
                setTile('tProt', '', '{{ lang._('Off') }}', '');
                return;
            }
            const role = s.role === 'master' ? '{{ lang._('primary') }}' : '{{ lang._('secondary') }}';
            const peerRole = s.role === 'master' ? '{{ lang._('secondary') }}' : '{{ lang._('primary') }}';
            const me = s.hostname || '{{ lang._('this firewall') }}';
            if (s.state === 'active') {
                setTile('tRole', 'ok', me, '{{ lang._('ACTIVE') }} · {{ lang._('handles all traffic') }} · ' + role);
            } else {
                setTile('tRole', 'idle', me, '{{ lang._('STANDBY') }} · {{ lang._('waiting as backup') }} · ' + role);
            }
            const other = s.peer_hostname || s.peer_addr || '{{ lang._('other firewall') }}';
            if (s.peer_alive) {
                setTile('tPeer', 'ok', other, (s.peer_state === 'active' ? '{{ lang._('ACTIVE') }}' : '{{ lang._('STANDBY') }}') + ' · ' + peerRole + ' · ' + (s.peer_addr || ''));
            } else {
                setTile('tPeer', 'bad', other, '{{ lang._('NOT RESPONDING') }} · ' + peerRole + ' · ' + (s.peer_addr || ''));
            }
            const y = s.sync || {};
            if (y.state === 'in_sync') { setTile('tSync', 'ok', '{{ lang._('In sync') }}', '{{ lang._('revision') }} ' + y.rev); }
            else if (y.state === 'behind') { setTile('tSync', 'warn', y.behind + ' {{ lang._('behind') }}', y.error || ''); }
            else { setTile('tSync', 'bad', '{{ lang._('FAILED') }}', y.error || ''); }
            if (s.maintenance) { setTile('tProt', 'warn', '{{ lang._('Paused') }}', '{{ lang._('maintenance mode') }}'); }
            else if (!s.auto_failover) { setTile('tProt', 'warn', '{{ lang._('Manual only') }}', '{{ lang._('automatic failover is off') }}'); }
            else if (s.health || s.drift || s.auto_blocked) { setTile('tProt', 'warn', '{{ lang._('Attention') }}', s.health || s.drift || s.auto_blocked); }
            else { setTile('tProt', 'ok', '{{ lang._('Armed') }}', (s.preempt ? '{{ lang._('automatic failover + preemption') }}' : '{{ lang._('automatic failover') }}') + (s.fencing ? ' · {{ lang._('fencing') }}' : '')); }
            $('#peerDownBox').toggle(standby && !s.peer_alive);
        }

        function takeover(force) {
            $('#takeResult').attr('class', 'alert alert-info').html('<i class="fa fa-spinner fa-spin"></i> {{ lang._('Taking over...') }}').show();
            $('#btnTakeover, #btnTakeoverForce').prop('disabled', true);
            ajaxCall(force ? '/api/activestandby/pairing/promote' : '/api/activestandby/pairing/failover', force ? {force: '1'} : {}, function (r) {
                $('#btnTakeover, #btnTakeoverForce').prop('disabled', false);
                showResult('#takeResult', r);
                refreshStatus();
                if (r.error && r.error.indexOf('would lose') >= 0) {
                    stdDialogConfirm('{{ lang._('Unreplicated changes') }}', r.error + ' {{ lang._('Force the failover anyway and lose those changes?') }}', '{{ lang._('Force') }}', '{{ lang._('Cancel') }}', function () {
                        ajaxCall('/api/activestandby/pairing/failover', {force: '1'}, function (r2) { showResult('#takeResult', r2); refreshStatus(); });
                    });
                }
            });
        }
        $('#btnTakeover').click(function () {
            stdDialogConfirm('{{ lang._('Make this firewall active') }}', '{{ lang._('The other firewall becomes the standby and this one takes over all traffic. Continue?') }}', '{{ lang._('Make active') }}', '{{ lang._('Cancel') }}', function () { takeover(false); });
        });
        $('#btnTakeoverForce').click(function () {
            stdDialogConfirm('{{ lang._('Force active') }}', '{{ lang._('Take over WITHOUT asking the other firewall. If it is still running, both will fight over the same addresses. Continue?') }}', '{{ lang._('Force') }}', '{{ lang._('Cancel') }}', function () { takeover(true); });
        });
        $('#bannerKitStop').click(function (e) { e.preventDefault(); ajaxCall('/api/activestandby/pairing/kitcancel', {}, function () { refreshStatus(); }); });
        $('#toggleSbEvents').click(function (e) { e.preventDefault(); $('#sbEvents').toggle(); });

        let seenWho = null, misses = 0;
        // Reload at most once every 10 s, whatever happens (never a reload loop).
        function reloadOnce() {
            try {
                const last = parseInt(sessionStorage.getItem('asReload') || '0', 10);
                if (Date.now() - last < 10000) { return; }
                sessionStorage.setItem('asReload', String(Date.now()));
            } catch (e) { /* storage blocked: reload anyway */ }
            location.reload();
        }
        // Status poll that survives a failover: the shared address is unreachable for a moment, then answered by the other
        // firewall (maybe without our login). Recovery after an outage or a non-JSON answer reloads the page.
        function statusGet(cb) {
            $.ajax({url: '/api/activestandby/pairing/status', dataType: 'json', timeout: 4000, cache: false})
                .done(function (s) {
                    if (misses >= 2) { reloadOnce(); return; }
                    misses = 0;
                    cb(s);
                })
                .fail(function (xhr) {
                    if (xhr.status === 401 || xhr.status === 403 || xhr.status === 200) { reloadOnce(); return; } // login page instead of JSON
                    misses++;
                });
        }
        function refreshStatus() {
            statusGet(function (s) {
                // The shared address is now answered by the other firewall (failover) or this one changed role: reload,
                // so the page, its menus and its buttons belong to the node that is serving them.
                const who = (s.hostname || '') + '|' + (s.state || '') + '|' + (s.role || '');
                if (seenWho !== null && who !== seenWho && s.daemon === 'up') { reloadOnce(); return; }
                seenWho = who;
                let rows = [
                    ['{{ lang._('This firewall') }}', (s.hostname || '-') + ' (' + (s.state || '-') + ', ' + (s.role === 'master' ? '{{ lang._('primary') }}' : (s.role === 'standby' ? '{{ lang._('secondary') }}' : s.role)) + ')'],
                    ['{{ lang._('Other firewall') }}', s.peer_addr ? (s.peer_hostname || '-') + ' (' + (s.peer_state || '-') + ')' : '-'],
                    ['{{ lang._('Management probe') }}', s.mgmt_probe ? (s.mgmt_probe === 'ok' ? '{{ lang._('this firewall reaches the probe targets') }}' : '{{ lang._('this firewall CANNOT reach the probe targets') }}') + (s.peer_mgmt_probe ? ' / ' + '{{ lang._('other firewall') }}: ' + s.peer_mgmt_probe : '') : '-'],
                    ['{{ lang._('Daemon') }}', s.daemon || (s.error ? 'error' : '')],
                    ['{{ lang._('Role') }}', s.role],
                    ['{{ lang._('State') }}', s.state ? (s.state + (s.peer_state ? ' (peer: ' + s.peer_state + ')' : '')) : '-'],
                    ['{{ lang._('Peer') }}', s.peer_addr ? s.peer_addr + (s.peer_reachable === undefined ? '' : (s.peer_reachable ? ' (reachable)' : ' (unreachable)')) : '-'],
                    ['{{ lang._('OPNsense version') }}', (s.os_version || '-') + (s.peer_os_version ? ' (peer: ' + s.peer_os_version + ')' : '')],
                    ['{{ lang._('Config sync') }}', syncText(s.sync)],
                    ['{{ lang._('Paired at') }}', s.paired_at || '-'],
                    ['{{ lang._('HA link address') }}', s.ha_address ? s.ha_address + ':' + s.listen_port : '-'],
                    ['{{ lang._('Pairing window') }}', s.pairing_open ? ('{{ lang._('open until') }} ' + s.pairing_expires) : '{{ lang._('closed') }}'],
                    ['{{ lang._('Certificate fingerprint') }}', s.local_fp ? '<code>' + esc(s.local_fp.substring(0, 32)) + '…</code>' : '-']
                ];
                if (s.error) { rows.push(['{{ lang._('Error') }}', esc(s.error)]); }
                $('#statusTable tbody').html(rows.map(r => '<tr><td>' + r[0] + '</td><td>' + (r[0] === '{{ lang._('Certificate fingerprint') }}' ? r[1] : esc(r[1])) + '</td></tr>').join(''));
                renderTiles(s);
                $('#bannerLost').toggle(!!s.state_lost);
                $('#bannerMaint').toggle(!!s.maintenance);
                const problems = [];
                if (s.health) { problems.push('{{ lang._('Health problem on the active node') }}: ' + s.health); }
                if (s.drift) { problems.push('{{ lang._('Configuration drift') }}: ' + s.drift); }
                if (s.auto_blocked) { problems.push(s.auto_blocked); }
                $('#bannerProblem').html(problems.map(p => '<div>' + esc(p) + '</div>').join('')).toggle(problems.length > 0);
                $('#bannerVersion').toggle(!!s.version_mismatch);
                $('#btnBreak').toggle(s.role === 'master' || s.role === 'standby');
                $('#haActions').toggle(s.role === 'master' || s.role === 'standby');
                $('#pairBox').toggle(s.role === 'standalone');
            });
        }
        function refreshEvents() {
            ajaxGet('/api/activestandby/pairing/events', {}, function (r) {
                const col = {error: 'danger', warning: 'warning', change: 'info', info: ''};
                var evh = (r.events || []).map(e => '<tr class="' + (col[e.kind] || '') + '"><td>' + esc(e.time.replace('T', ' ').replace('Z', '')) + '</td><td>' + esc(e.msg) + '</td></tr>').join(''); $('#evTable tbody, #evTable2 tbody').html(evh);
            });
        }
        refreshEvents();
        setInterval(refreshEvents, 10000);
        refreshStatus();
        setInterval(refreshStatus, 3000);

        $('#saveAct').click(function () {
            saveFormToEndpoint('/api/activestandby/settings/set', 'frm_general', function () {
                ajaxCall('/api/activestandby/service/reconfigure', {}, function () { refreshStatus(); });
            });
        });

        $('#btnPrepare').click(function () {
            ajaxCall('/api/activestandby/pairing/prepare', {}, function (r) {
                if (r.error) { showResult('#prepResult', r); return; }
                $('#prepResult').attr('class', 'alert alert-info').html(
                    '{{ lang._('On the MASTER, enter peer address') }} <b>' + esc(r.address) + '</b> {{ lang._('and token') }} <b style="font-size:150%">' + esc(r.token) + '</b>' +
                    '<br><small>{{ lang._('Valid until') }} ' + esc(r.expires) + '</small>').show();
                refreshStatus();
            });
        });

        $('#btnPair').click(function () {
            $('#pairResult').attr('class', 'alert alert-info').text('{{ lang._('Pairing...') }}').show();
            ajaxCall('/api/activestandby/pairing/pair', {peer: $('#pairPeer').val(), token: $('#pairToken').val()}, function (r) {
                showResult('#pairResult', r);
                refreshStatus();
            });
        });

        function doFailover(force) {
            $('#actResult').attr('class', 'alert alert-info').text('{{ lang._('Working...') }}').show();
            ajaxCall('/api/activestandby/pairing/failover', force ? {force: '1'} : {}, function (r) {
                showResult('#actResult', r);
                refreshStatus();
                if (r.error && r.error.indexOf('would lose') >= 0) {
                    stdDialogConfirm('{{ lang._('Unreplicated changes') }}', r.error + ' {{ lang._('Force the failover anyway and lose those changes?') }}', '{{ lang._('Force') }}', '{{ lang._('Cancel') }}', function () { doFailover(true); });
                }
            });
        }
        $('#btnFailover').click(function () {
            stdDialogConfirm('{{ lang._('Failover') }}', '{{ lang._('Switch the active role to the other node now?') }}', '{{ lang._('Yes') }}', '{{ lang._('Cancel') }}', function () { doFailover(false); });
        });

        $('#btnBreak').click(function () {
            stdDialogConfirm('{{ lang._('Break pair') }}', '{{ lang._('Both nodes become standalone again. Continue?') }}', '{{ lang._('Yes') }}', '{{ lang._('Cancel') }}', function () {
                ajaxCall('/api/activestandby/pairing/break', {}, function (r) { showResult('#breakResult', r); refreshStatus(); });
            });
        });
    });
</script>

<style>
    .as-tiles { display: flex; flex-wrap: wrap; gap: 12px; margin: 0 0 18px 0; }
    .as-tile { flex: 1 1 200px; border: 1px solid rgba(128,128,128,.35); border-radius: 6px; padding: 12px 16px; min-width: 190px; }
    .as-tile .as-k { font-size: 11px; text-transform: uppercase; letter-spacing: .06em; opacity: .65; }
    .as-tile .as-v { font-size: 22px; font-weight: 600; line-height: 1.3; }
    .as-tile .as-s { font-size: 12px; opacity: .75; min-height: 16px; }
    .as-dot { display: inline-block; width: 11px; height: 11px; border-radius: 50%; margin-right: 8px; background: #999; }
    .as-dot.ok { background: #3c9d4e; } .as-dot.warn { background: #e0a100; } .as-dot.bad { background: #d9534f; } .as-dot.idle { background: #5b8fd6; }
    #standbyUi .as-big { padding: 34px 10px 10px 10px; text-align: center; }
    #standbyUi .btn-lg { padding: 16px 34px; font-size: 20px; margin: 14px 0; }
</style>

<div id="asLoading" class="content-box" style="padding:30px"><i class="fa fa-spinner fa-spin"></i> {{ lang._('Loading...') }}</div>

<div id="hero" style="display:none">
    <div class="as-tiles">
        <div class="as-tile"><div class="as-k">{{ lang._('This firewall') }}</div><div class="as-v"><span class="as-dot" id="tRoleDot"></span><span id="tRole">-</span></div><div class="as-s" id="tRoleSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Other firewall') }}</div><div class="as-v"><span class="as-dot" id="tPeerDot"></span><span id="tPeer">-</span></div><div class="as-s" id="tPeerSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Configuration sync') }}</div><div class="as-v"><span class="as-dot" id="tSyncDot"></span><span id="tSync">-</span></div><div class="as-s" id="tSyncSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Failover protection') }}</div><div class="as-v"><span class="as-dot" id="tProtDot"></span><span id="tProt">-</span></div><div class="as-s" id="tProtSub"></div></div>
    </div>
    <div id="bannerKit" class="alert alert-info" style="display:none"><i class="fa fa-spinner fa-spin"></i> <span id="bannerKitTxt"></span> <a href="#" id="bannerKitStop" style="margin-left:10px">{{ lang._('Stop waiting') }}</a></div>
    <div id="bannerMaint" class="alert alert-info" style="display:none">{{ lang._('MAINTENANCE MODE is on: automatic failover, preemption and health switching are paused on both nodes. Switch it off under Operations when you are done.') }}</div>
    <div id="bannerProblem" class="alert alert-danger" style="display:none"></div>
    <div id="bannerVersion" class="alert alert-warning" style="display:none">{{ lang._('The two nodes run different OPNsense or plugin versions. This is expected while updating one node. Finish updating both; configuration sync is refused across core versions.') }}</div>
    <div id="bannerLost" class="alert alert-danger" style="display:none">{{ lang._('The saved HA state of this node was unreadable. It runs as a silent standby without a peer. Break the pair and pair again.') }}</div>
</div>

<div id="standbyUi" class="content-box" style="display:none">
    <div class="col-md-12 as-big">
        <h2>{{ lang._('This firewall is the standby') }}</h2>
        <p class="text-muted" style="font-size:15px">{{ lang._('The other firewall is active and handles all traffic. Its configuration is copied here automatically; make changes on the active firewall only.') }}</p>
        <button class="btn btn-primary btn-lg" id="btnTakeover"><i class="fa fa-exchange"></i> {{ lang._('Make this firewall the active one') }}</button>
        <p class="text-muted">{{ lang._('The active firewall hands over cleanly. Traffic is interrupted for a few seconds.') }}</p>
        <div id="takeResult" class="alert" style="display:none; max-width:700px; margin:10px auto"></div>
        <div id="peerDownBox" style="display:none; margin-top:20px">
            <p class="text-danger"><b>{{ lang._('The active firewall does not answer.') }}</b> {{ lang._('If it is really down you can take over without its help.') }}</p>
            <button class="btn btn-danger" id="btnTakeoverForce">{{ lang._('Emergency: force this firewall active') }}</button>
        </div>
        <p style="margin-top:30px"><a href="#" id="toggleSbEvents">{{ lang._('Show recent events') }}</a></p>
        <div id="sbEvents" style="display:none; text-align:left; max-width:900px; margin:0 auto">
            <table class="table table-condensed table-striped" id="evTable2"><thead><tr><th>{{ lang._('Time (UTC)') }}</th><th>{{ lang._('Event') }}</th></tr></thead><tbody></tbody></table>
        </div>
    </div>
</div>

<div id="fullUi" style="display:none">
<ul class="nav nav-tabs" data-tabs="tabs" id="maintabs">
    <li><a data-toggle="tab" href="#wizard">{{ lang._('Setup wizard') }}</a></li>
    <li class="active"><a data-toggle="tab" href="#status">{{ lang._('Status and Pairing') }}</a></li>
    <li><a data-toggle="tab" href="#operations">{{ lang._('Operations') }}</a></li>
    <li><a data-toggle="tab" href="#settings">{{ lang._('Settings') }}</a></li>
</ul>
<div class="tab-content content-box">
    <div id="status" class="tab-pane fade in active">
        <div class="col-md-12">
            <h2>{{ lang._('Details') }}</h2>
            <table class="table table-condensed" id="statusTable" style="max-width:700px"><tbody></tbody></table>
            <div id="haActions" style="display:none; margin-bottom:10px">
                <button class="btn btn-warning" id="btnFailover">{{ lang._('Failover now') }}</button>
            </div>
            <div id="actResult" class="alert" style="display:none"></div>
            <div id="breakResult" class="alert" style="display:none"></div>
            <h3>{{ lang._('Recent events') }}</h3>
            <table class="table table-condensed table-striped" id="evTable" style="max-width:900px"><thead><tr><th>{{ lang._('Time (UTC)') }}</th><th>{{ lang._('Event') }}</th></tr></thead><tbody></tbody></table>
            <button class="btn btn-danger" id="btnBreak" style="display:none">{{ lang._('Break pair') }}</button>
        </div>
        <div class="col-md-12" id="pairBox">
            <hr/>
            <h2>{{ lang._('Pairing') }}</h2>
            <p>{{ lang._('Pair two identical nodes over the HA link. Do this first on the standby, then on the master. The master is the node that keeps its configuration.') }}</p>
            <div class="row">
                <div class="col-md-5">
                    <h3>1. {{ lang._('On the standby') }}</h3>
                    <button class="btn btn-primary" id="btnPrepare">{{ lang._('Open pairing window') }}</button>
                    <div id="prepResult" class="alert" style="display:none; margin-top:10px"></div>
                </div>
                <div class="col-md-5">
                    <h3>2. {{ lang._('On the master') }}</h3>
                    <div class="form-group">
                        <label>{{ lang._('Standby HA link address') }}</label>
                        <input class="form-control" id="pairPeer" placeholder="10.99.0.2"/>
                    </div>
                    <div class="form-group">
                        <label>{{ lang._('Pairing token') }}</label>
                        <input class="form-control" id="pairToken" placeholder="XXXXX-XXXXX-XXXXX-XXXXX"/>
                    </div>
                    <button class="btn btn-primary" id="btnPair">{{ lang._('Pair') }}</button>
                    <div id="pairResult" class="alert" style="display:none; margin-top:10px"></div>
                </div>
            </div>
        </div>
    </div>
    {{ partial("OPNsense/ActiveStandby/wizard") }}
    {{ partial("OPNsense/ActiveStandby/operations") }}
    <div id="settings" class="tab-pane fade in">
        {{ partial("layout_partials/base_form", ['fields': generalForm, 'id': 'frm_general']) }}
        <div class="col-md-12">
            <hr/>
            <button class="btn btn-primary" id="saveAct" type="button"><b>{{ lang._('Save') }}</b></button>
        </div>
    </div>
</div>
</div>
