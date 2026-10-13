<script>
    $(document).ready(function () {
        let settingsDirty = false;
        // (re)load the settings form; the wizard calls this after it saved, so Save here can never write stale values back
        window.asReloadSettings = function () {
            mapDataToFormUI({'frm_general': '/api/activestandby/settings/get'}).done(function () {
                $('.selectpicker').selectpicker('refresh');
                settingsDirty = false;
            });
        };
        window.asReloadSettings();
        $('#frm_general').on('change input', function () { settingsDirty = true; });

        function esc(s) { return $('<div>').text(s === undefined || s === null ? '' : s).html(); }

        function showResult(sel, r) {
            // no usable answer (the shared address is moving during a failover, or the call failed): never a green box
            if (!r || (!r.error && !r.status && !r.message)) {
                r = {error: '{{ lang._('No answer from the firewall. If a failover is in progress this is normal: the page reloads when the other firewall answers.') }}'};
            }
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
            if (y.error) { t += ' - ' + y.error; }
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
            $('#maintabs').data('paired', paired);
            if (!paired && !window.asWizardShown) { // first visit of an unpaired firewall: start where the work is
                window.asWizardShown = true;
                $('#maintabs a[href="#wizard"]').tab('show');
            }
            // the setup wizard only makes sense on a node that is not paired yet; it must not sit there with destructive buttons
            $('#maintabs a[href="#wizard"]').parent().toggle(!paired);
            if (paired && $('#wizard').hasClass('active')) { $('#maintabs a[href="#status"]').tab('show'); }
            if (s.daemon !== 'up' && s.role === 'standalone') {
                setTile('tRole', 'idle', '{{ lang._('HA is not set up') }}', '{{ lang._('Open the Setup wizard tab to create a pair') }}');
                setTile('tPeer', '', '-', '');
                setTile('tSync', '', '-', '');
                setTile('tProt', '', '{{ lang._('Off') }}', '');
                return;
            }
            if (s.daemon !== 'up') {
                // the daemon is the whole product: when it does not run, say so instead of drawing a healthy picture
                setTile('tRole', 'bad', '{{ lang._('HA daemon not running') }}', '{{ lang._('Enable the service under Settings, or look at /var/log/hasyncd.log') }}');
                setTile('tPeer', '', '-', '');
                setTile('tSync', '', '-', '');
                setTile('tProt', 'bad', '{{ lang._('NOT PROTECTED') }}', '{{ lang._('no failover while the daemon is down') }}');
                return;
            }
            if (!paired) {
                setTile('tRole', 'idle', '{{ lang._('Not paired') }}', '{{ lang._('Open the Setup wizard tab to create a pair') }}');
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
            else if (s.degraded || s.health || s.drift || s.auto_blocked) { setTile('tProt', 'warn', '{{ lang._('Attention') }}', s.degraded || s.health || s.drift || s.auto_blocked); }
            else { setTile('tProt', 'ok', '{{ lang._('Ready') }}', (s.preempt ? '{{ lang._('automatic failover + preemption') }}' : '{{ lang._('automatic failover') }}') + (s.fencing ? ' · {{ lang._('fencing') }}' : '')); }
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
        // maintenance mode can be left on by mistake and pauses automatic failover on BOTH nodes: it must be switchable here too
        $('#btnSbMaintOff').click(function () {
            $(this).prop('disabled', true);
            ajaxCall('/api/activestandby/pairing/maintenance', {on: '0'}, function (r) { $('#btnSbMaintOff').prop('disabled', false); showResult('#takeResult', r); refreshStatus(); });
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
        let inflight = false;
        function statusGet(cb) {
            if (inflight || document.hidden) { return; } // one request at a time, none while the tab is in the background
            inflight = true;
            $.ajax({url: '/api/activestandby/pairing/status', dataType: 'json', timeout: 4000, cache: false})
                .done(function (s) {
                    if (misses >= 2) { reloadOnce(); return; }
                    misses = 0;
                    $('#bannerOffline').hide();
                    cb(s);
                })
                .fail(function (xhr) {
                    if (xhr.status === 401 || xhr.status === 403 || xhr.status === 200) { reloadOnce(); return; } // login page instead of JSON
                    misses++;
                    if (misses >= 2) { $('#bannerOffline').show(); }
                    $('#asLoading').hide();
                })
                .always(function () { inflight = false; });
        }
        function refreshStatus() {
            statusGet(function (s) {
                // The shared address is now answered by the other firewall (failover) or this one changed role: reload,
                // so the page, its menus and its buttons belong to the node that is serving them.
                const who = (s.hostname || '') + '|' + (s.state || '') + '|' + (s.role || '');
                if (seenWho !== null && who !== seenWho && s.daemon === 'up') {
                    const wasPaired = seenWho.split('|')[2] !== 'standalone';
                    // pairing completing (standalone -> paired) is not a reason to throw the wizard away; unsaved settings are not
                    // thrown away either: tell the user instead
                    if (!wasPaired) { seenWho = who; }
                    else if (settingsDirty) { $('#bannerReload').show(); seenWho = who; }
                    else { reloadOnce(); return; }
                }
                seenWho = who;
                $('#sbMaintBox').toggle(!!s.maintenance);
                let rows = [
                    ['{{ lang._('This firewall') }}', (s.hostname || '-') + ' (' + (s.state || '-') + ', ' + (s.role === 'master' ? '{{ lang._('primary') }}' : (s.role === 'standby' ? '{{ lang._('secondary') }}' : s.role)) + ')'],
                    ['{{ lang._('Other firewall') }}', s.peer_addr ? (s.peer_hostname || '-') + ' (' + (s.peer_state || '-') + ')' : '-'],
                    ['{{ lang._('Management probe') }}', s.mgmt_probe ? (s.mgmt_probe === 'ok' ? '{{ lang._('this firewall reaches the probe targets') }}' : '{{ lang._('this firewall CANNOT reach the probe targets') }}') + (s.peer_mgmt_probe ? ' / ' + '{{ lang._('other firewall') }}: ' + s.peer_mgmt_probe : '') : '-'],
                    ['{{ lang._('Daemon') }}', s.daemon || (s.error ? 'error' : '')],
                    ['{{ lang._('Peer HA address') }}', s.peer_addr ? s.peer_addr + (s.peer_reachable === undefined ? '' : (s.peer_reachable ? ' ({{ lang._('reachable') }})' : ' ({{ lang._('unreachable') }})')) : '-'],
                    ['{{ lang._('OPNsense version') }}', (s.os_version || '-') + (s.peer_os_version ? ' (peer: ' + s.peer_os_version + ')' : '')],
                    ['{{ lang._('Config sync') }}', syncText(s.sync)],
                    ['{{ lang._('Paired at') }}', s.paired_at || '-'],
                    ['{{ lang._('HA link address') }}', s.ha_address ? s.ha_address + ':' + s.listen_port : '-'],
                    ['{{ lang._('Pairing window') }}', s.pairing_open ? ('{{ lang._('open until') }} ' + s.pairing_expires) : '{{ lang._('closed') }}'],
                    ['{{ lang._('Certificate fingerprint') }}', s.local_fp ? '<code title="' + esc(s.local_fp) + '" style="word-break:break-all">' + esc(s.local_fp) + '</code>' : '-']
                ];
                if (s.error) { rows.push(['{{ lang._('Error') }}', esc(s.error)]); }
                $('#statusTable tbody').html(rows.map(r => '<tr><td>' + r[0] + '</td><td>' + (r[0] === '{{ lang._('Certificate fingerprint') }}' ? r[1] : esc(r[1])) + '</td></tr>').join(''));
                renderTiles(s);
                $('#bannerLost').toggle(!!s.state_lost);
                $('#bannerMaint').toggle(!!s.maintenance);
                const problems = [];
                if (s.degraded) { problems.push('{{ lang._('Active but degraded') }}: ' + s.degraded); }
                if (s.warm && (s.warm.mode === 'warm' || s.warm.mode === 'hot') && s.state === 'standby' && !s.warm.ready) { problems.push('{{ lang._('Warm standby not ready, a takeover would use the classic path') }}: ' + (s.warm.reason || '')); }
                if (s.warm && (s.warm.mode === 'warm' || s.warm.mode === 'hot') && s.state === 'active' && s.peer_warm && s.peer_warm !== 'ready') { problems.push('{{ lang._('The standby is not ready for a warm takeover') }}: ' + s.peer_warm); }
                if (s.health) { problems.push('{{ lang._('Health problem on the active node') }}: ' + s.health); }
                if (s.drift) { problems.push('{{ lang._('Configuration drift') }}: ' + s.drift); }
                if (s.pfsync_problem) { problems.push('{{ lang._('State synchronisation (pfsync)') }}: ' + s.pfsync_problem); }
                if (s.package_diff) { problems.push(s.package_diff); }
                if (s.auto_blocked) { problems.push(s.auto_blocked); }
                $('#bannerProblem').html(problems.map(p => '<div>' + esc(p) + '</div>').join('')).toggle(problems.length > 0);
                $('#bannerVersion').toggle(!!s.version_mismatch);
                $('#btnBreak').toggle(s.role === 'master' || s.role === 'standby');
                $('#haActions').toggle(s.role === 'master' || s.role === 'standby');
                $('#pairHint').toggle(s.role === 'standalone');
            });
        }
        function refreshEvents() {
            ajaxGet('/api/activestandby/pairing/events', {}, function (r) {
                const col = {error: 'as-ev-error', warning: 'as-ev-warning', change: 'as-ev-change', info: 'as-ev-info'};
                var evh = (r.events || []).map(e => '<tr class="' + (col[e.kind] || 'as-ev-info') + '"><td class="as-ev-time">' + esc(e.time.replace('T', ' ').replace('Z', '')) + '</td><td>' + esc(e.msg) + '</td></tr>').join(''); $('#evTable tbody, #evTable2 tbody').html(evh);
            });
        }
        refreshEvents();
        setInterval(refreshEvents, 10000);
        refreshStatus();
        setInterval(refreshStatus, 3000);

        $('#toWizard').click(function (e) { e.preventDefault(); $('#maintabs a[href="#wizard"]').tab('show'); });
        $('#reloadNow').click(function (e) { e.preventDefault(); location.reload(); });
        function saveSettings() {
            $('#saveAct').prop('disabled', true);
            saveFormToEndpoint('/api/activestandby/settings/set', 'frm_general', function () {
                ajaxCall('/api/activestandby/service/reconfigure', {}, function () {
                    $('#saveAct').prop('disabled', false);
                    settingsDirty = false;
                    $('#saveResult').attr('class', 'alert alert-success').text('{{ lang._('Saved and applied. The other firewall adopts shared settings within seconds.') }}').show();
                    setTimeout(function () { $('#saveResult').fadeOut(); }, 6000);
                    refreshStatus();
                });
            });
            setTimeout(function () { $('#saveAct').prop('disabled', false); }, 8000); // validation errors keep the button usable
        }
        $('#saveAct').click(function () {
            if (!$('#activestandby\\.general\\.enabled').is(':checked') && $('#maintabs').data('paired')) {
                stdDialogConfirm('{{ lang._('Switch HA off?') }}', '{{ lang._('The HA service stops on this firewall: no heartbeat, no replication and no automatic failover. Continue?') }}', '{{ lang._('Save') }}', '{{ lang._('Cancel') }}', saveSettings);
            } else {
                saveSettings();
            }
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
            stdDialogConfirm('{{ lang._('Failover now') }}', '{{ lang._('The other firewall becomes active and this one the standby. Traffic is interrupted for a second or two. Continue?') }}', '{{ lang._('Switch roles') }}', '{{ lang._('Cancel') }}', function () {
                $('#btnFailover').prop('disabled', true);
                setTimeout(function () { $('#btnFailover').prop('disabled', false); }, 12000); // a second click would switch straight back
                doFailover(false);
            });
        });

        $('#btnBreak').click(function () {
            stdDialogConfirm('{{ lang._('Break pair') }}', '{{ lang._('Both firewalls become standalone. The former standby keeps its data interfaces DISABLED and needs a configuration restored before you connect it to the network, otherwise two firewalls would share one address. Break the pair?') }}', '{{ lang._('Break pair') }}', '{{ lang._('Cancel') }}', function () {
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
    /* event log: no bootstrap contextual row colours (unreadable in the dark theme); the severity is a coloured edge, the text keeps the theme colour */
    table.as-events > tbody > tr > td { background: transparent; border-top: 1px solid rgba(128,128,128,.25); vertical-align: top; }
    table.as-events .as-ev-time { white-space: nowrap; opacity: .75; border-left: 4px solid transparent; }
    table.as-events tr.as-ev-error .as-ev-time { border-left-color: #d9534f; }
    table.as-events tr.as-ev-warning .as-ev-time { border-left-color: #e0a100; }
    table.as-events tr.as-ev-change .as-ev-time { border-left-color: #5b8fd6; }
    table.as-events tr.as-ev-error td:last-child { font-weight: 600; }
    #standbyUi .as-big { padding: 34px 10px 10px 10px; text-align: center; }
    #standbyUi .btn-lg { padding: 16px 34px; font-size: 20px; margin: 14px 0; white-space: normal; }
    .as-tile, .as-tile .as-v { min-width: 0; overflow-wrap: anywhere; }
</style>

<div id="asLoading" class="content-box" style="padding:30px"><i class="fa fa-spinner fa-spin"></i> {{ lang._('Loading...') }}</div>

<div id="hero" style="display:none">
    <div class="as-tiles">
        <div class="as-tile"><div class="as-k">{{ lang._('This firewall') }}</div><div class="as-v"><span class="as-dot" id="tRoleDot"></span><span id="tRole">-</span></div><div class="as-s" id="tRoleSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Other firewall') }}</div><div class="as-v"><span class="as-dot" id="tPeerDot"></span><span id="tPeer">-</span></div><div class="as-s" id="tPeerSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Configuration sync') }}</div><div class="as-v"><span class="as-dot" id="tSyncDot"></span><span id="tSync">-</span></div><div class="as-s" id="tSyncSub"></div></div>
        <div class="as-tile"><div class="as-k">{{ lang._('Failover protection') }}</div><div class="as-v"><span class="as-dot" id="tProtDot"></span><span id="tProt">-</span></div><div class="as-s" id="tProtSub"></div></div>
    </div>
    <div id="bannerOffline" class="alert alert-warning" role="alert" style="display:none">{{ lang._('No answer from this firewall for a few seconds. During a failover this is normal: the page reloads when the other firewall answers.') }}</div>
    <div id="bannerReload" class="alert alert-info" role="alert" style="display:none">{{ lang._('The roles changed while you were editing settings. Reload when you are done.') }} <a href="#" id="reloadNow">{{ lang._('Reload now') }}</a></div>
    <div id="bannerKit" class="alert alert-info" style="display:none"><i class="fa fa-spinner fa-spin"></i> <span id="bannerKitTxt"></span> <a href="#" id="bannerKitStop" style="margin-left:10px">{{ lang._('Stop waiting') }}</a></div>
    <div id="bannerMaint" class="alert alert-info" style="display:none">{{ lang._('MAINTENANCE MODE is on: automatic failover, preemption and health switching are paused on both nodes. Switch it off under Operations (on the active firewall) or with the button on the standby page when you are done.') }}</div>
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
        <div id="sbMaintBox" class="alert alert-info" style="display:none; max-width:700px; margin:10px auto">{{ lang._('Maintenance mode is on: automatic failover is paused on both firewalls.') }} <button class="btn btn-default btn-sm" id="btnSbMaintOff">{{ lang._('Switch maintenance mode off') }}</button></div>
        <div id="takeResult" class="alert" style="display:none; max-width:700px; margin:10px auto"></div>
        <div id="peerDownBox" style="display:none; margin-top:20px">
            <p class="text-danger"><b>{{ lang._('The active firewall does not answer.') }}</b> {{ lang._('If it is really down you can take over without its help.') }}</p>
            <button class="btn btn-danger" id="btnTakeoverForce">{{ lang._('Emergency: force this firewall active') }}</button>
        </div>
        <p style="margin-top:30px"><a href="#" id="toggleSbEvents">{{ lang._('Show recent events') }}</a></p>
        <div id="sbEvents" style="display:none; text-align:left; max-width:900px; margin:0 auto">
            <table class="table table-condensed as-events" id="evTable2"><thead><tr><th>{{ lang._('Time (UTC)') }}</th><th>{{ lang._('Event') }}</th></tr></thead><tbody></tbody></table>
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
            <table class="table table-condensed as-events" id="evTable" style="max-width:900px"><thead><tr><th>{{ lang._('Time (UTC)') }}</th><th>{{ lang._('Event') }}</th></tr></thead><tbody></tbody></table>
            <button class="btn btn-danger" id="btnBreak" style="display:none">{{ lang._('Break pair') }}</button>
        </div>
        <div class="col-md-12" id="pairHint" style="display:none"><hr/><p>{{ lang._('This firewall is not part of a pair yet. Use the Setup wizard tab.') }} <a href="#" id="toWizard">{{ lang._('Open the Setup wizard') }}</a></p></div>
    </div>
    {{ partial("OPNsense/ActiveStandby/wizard") }}
    {{ partial("OPNsense/ActiveStandby/operations") }}
    <div id="settings" class="tab-pane fade in">
        {{ partial("layout_partials/base_form", ['fields': generalForm, 'id': 'frm_general']) }}
        <div class="col-md-12">
            <hr/>
            <button class="btn btn-primary" id="saveAct" type="button"><b>{{ lang._('Save') }}</b></button>
            <div id="saveResult" class="alert" style="display:none; margin-top:10px" role="status"></div>
        </div>
    </div>
</div>
</div>
