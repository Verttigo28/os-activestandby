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

        function refreshStatus() {
            ajaxGet('/api/activestandby/pairing/status', {}, function (s) {
                let rows = [
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
                $('#bannerStandby').toggle(s.state === 'standby' && s.role !== 'standalone');
                $('#bannerLost').toggle(!!s.state_lost);
                $('#bannerVersion').toggle(!!s.version_mismatch);
                $('#btnBreak').toggle(s.role === 'master' || s.role === 'standby');
                $('#haActions').toggle(s.role === 'master' || s.role === 'standby');
                $('#pairBox').toggle(s.role === 'standalone');
            });
        }
        function refreshEvents() {
            ajaxGet('/api/activestandby/pairing/events', {}, function (r) {
                const col = {error: 'danger', warning: 'warning', change: 'info', info: ''};
                $('#evTable tbody').html((r.events || []).map(e => '<tr class="' + (col[e.kind] || '') + '"><td>' + esc(e.time.replace('T', ' ').replace('Z', '')) + '</td><td>' + esc(e.msg) + '</td></tr>').join(''));
            });
        }
        refreshEvents();
        setInterval(refreshEvents, 10000);
        refreshStatus();
        setInterval(refreshStatus, 5000);

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

        function ha(path, data, title, text) {
            stdDialogConfirm(title, text, '{{ lang._('Yes') }}', '{{ lang._('Cancel') }}', function () {
                $('#actResult').attr('class', 'alert alert-info').text('{{ lang._('Working...') }}').show();
                ajaxCall(path, data, function (r) { showResult('#actResult', r); refreshStatus(); });
            });
        }
        $('#btnResync').click(function () { ha('/api/activestandby/pairing/resync', {}, '{{ lang._('Full resync') }}', '{{ lang._('Push the complete configuration to the standby again?') }}'); });
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
        $('#btnPromote').click(function () { ha('/api/activestandby/pairing/promote', {force: '1'}, '{{ lang._('Force active') }}', '{{ lang._('Make THIS node active without asking the peer. If the peer is still active both nodes will fight over the same IPs. Continue?') }}'); });
        $('#btnDemote').click(function () { ha('/api/activestandby/pairing/demote', {}, '{{ lang._('Force standby') }}', '{{ lang._('Put THIS node in standby: its data interfaces go down. Continue?') }}'); });

        $('#btnBreak').click(function () {
            stdDialogConfirm('{{ lang._('Break pair') }}', '{{ lang._('Both nodes become standalone again. Continue?') }}', '{{ lang._('Yes') }}', '{{ lang._('Cancel') }}', function () {
                ajaxCall('/api/activestandby/pairing/break', {}, function (r) { showResult('#breakResult', r); refreshStatus(); });
            });
        });
    });
</script>

<ul class="nav nav-tabs" data-tabs="tabs" id="maintabs">
    <li><a data-toggle="tab" href="#wizard">{{ lang._('Setup wizard') }}</a></li>
    <li class="active"><a data-toggle="tab" href="#status">{{ lang._('Status and Pairing') }}</a></li>
    <li><a data-toggle="tab" href="#settings">{{ lang._('Settings') }}</a></li>
</ul>
<div class="tab-content content-box">
    <div id="status" class="tab-pane fade in active">
        <div class="col-md-12">
            <div id="bannerStandby" class="alert alert-warning" style="display:none">{{ lang._('This node is STANDBY. Changes made here are overwritten by the active node; make configuration changes on the active node.') }}</div>
            <div id="bannerVersion" class="alert alert-warning" style="display:none">{{ lang._('The two nodes run different OPNsense or plugin versions. This is expected while updating one node. Finish updating both; configuration sync is refused across core versions.') }}</div>
            <div id="bannerLost" class="alert alert-danger" style="display:none">{{ lang._('The saved HA state of this node was unreadable. It runs as a silent standby without a peer. Break the pair and pair again.') }}</div>
            <h2>{{ lang._('Node status') }}</h2>
            <table class="table table-condensed" id="statusTable" style="max-width:700px"><tbody></tbody></table>
            <div id="haActions" style="display:none; margin-bottom:10px">
                <button class="btn btn-warning" id="btnFailover">{{ lang._('Failover now') }}</button>
                <button class="btn btn-default" id="btnPromote">{{ lang._('Force this node active') }}</button>
                <button class="btn btn-default" id="btnResync">{{ lang._('Full resync') }}</button>
                <button class="btn btn-default" id="btnDemote">{{ lang._('Force this node standby') }}</button>
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
    <div id="settings" class="tab-pane fade in">
        {{ partial("layout_partials/base_form", ['fields': generalForm, 'id': 'frm_general']) }}
        <div class="col-md-12">
            <hr/>
            <button class="btn btn-primary" id="saveAct" type="button"><b>{{ lang._('Save') }}</b></button>
        </div>
    </div>
</div>
