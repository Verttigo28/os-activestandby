<div id="operations" class="tab-pane fade in">
    <div class="col-md-12">
        <h2>{{ lang._('Maintenance mode') }}</h2>
        <p>{{ lang._('Pauses automatic failover, preemption and health-driven switching on BOTH nodes (split-brain protection stays on). It switches itself off after the chosen time.') }}</p>
        <div class="form-inline" style="margin-bottom:10px">
            <span id="opMaint" class="label label-default" style="margin-right:10px">-</span>
            <select id="opHours" class="form-control" style="width:auto">
                <option value="1">1 h</option><option value="2">2 h</option><option value="4" selected>4 h</option><option value="8">8 h</option><option value="24">24 h</option>
            </select>
            <button class="btn btn-warning" id="opMaintOn">{{ lang._('Enable') }}</button>
            <button class="btn btn-default" id="opMaintOff">{{ lang._('Disable') }}</button>
        </div>
        <div id="opMaintRes" class="alert" style="display:none"></div>

        <hr/>
        <h2>{{ lang._('Pre-flight check') }}</h2>
        <p>{{ lang._('Checks both nodes and runs a dry run of the standby taking over. Nothing is changed.') }}</p>
        <button class="btn btn-primary" id="opPre">{{ lang._('Run pre-flight') }}</button>
        <span id="opPreSum" style="margin-left:10px"></span>
        <div id="opPreOut" style="margin-top:10px"></div>

        <hr/>
        <h2>{{ lang._('Upgrade assistant') }}</h2>
        <p>{{ lang._('Updates both nodes one after the other without an outage: the standby is updated and rebooted first, then the roles are switched and the other node is updated. Do not change the configuration while it runs. Maintenance mode is switched on for the duration and the job stops (leaving maintenance mode on) if anything goes wrong.') }}</p>
        <div class="checkbox"><label><input type="checkbox" id="opFailback"/> {{ lang._('Give the active role back to the original node at the end') }}</label></div>
        <button class="btn btn-danger" id="opUpStart">{{ lang._('Start guided upgrade') }}</button>
        <button class="btn btn-default" id="opUpCancel" style="display:none">{{ lang._('Cancel') }}</button>
        <div id="opUpRes" class="alert" style="display:none; margin-top:10px"></div>
        <div id="opUpJob" style="display:none; margin-top:10px">
            <ol id="opUpPhases" class="list-unstyled" style="line-height:1.9"></ol>
            <pre id="opUpLog" style="max-height:220px; overflow:auto"></pre>
        </div>

        <hr/>
        <h2>{{ lang._('Failover history') }}</h2>
        <table class="table table-condensed table-striped" id="opHist" style="max-width:900px">
            <thead><tr><th>{{ lang._('Time (UTC)') }}</th><th>{{ lang._('Change') }}</th><th>{{ lang._('Duration') }}</th><th>{{ lang._('Reason') }}</th></tr></thead><tbody></tbody>
        </table>

        <hr/>
        <h2>{{ lang._('Monitoring') }}</h2>
        <ul>
            <li>{{ lang._('Prometheus: scrape') }} <code>/api/activestandby/metrics/index</code> {{ lang._('on each node with an OPNsense API key (basic auth).') }}</li>
            <li>{{ lang._('E-mail: in Monit add a custom service with the path') }} <code>/usr/local/opnsense/scripts/OPNsense/ActiveStandby/monit_check.sh</code> {{ lang._('and the test "status != 0".') }}</li>
            <li>{{ lang._('Webhook and syslog: see the Settings tab.') }}</li>
        </ul>
    </div>
</div>

<script>
$(function () {
    const esc = s => $('<div>').text(s === undefined || s === null ? '' : s).html();
    const lbl = {pass: 'success', warn: 'warning', fail: 'danger'};
    const PHASES = [
        ['r1-update', '{{ lang._('Update the standby') }}'], ['r1-reboot', '{{ lang._('Reboot the standby') }}'], ['r1-wait', '{{ lang._('Wait until the standby is back') }}'],
        ['failover', '{{ lang._('Switch the active role to the updated node') }}'],
        ['r2-update', '{{ lang._('Update the other node') }}'], ['r2-reboot', '{{ lang._('Reboot the other node') }}'], ['r2-wait', '{{ lang._('Wait until it is back') }}'],
        ['failback', '{{ lang._('Give the active role back (optional)') }}'], ['done', '{{ lang._('Finished') }}']
    ];

    function refreshMaint() {
        ajaxGet('/api/activestandby/pairing/status', {}, function (s) {
            const on = !!s.maintenance;
            $('#opMaint').attr('class', 'label label-' + (on ? 'warning' : 'default')).text(on ? '{{ lang._('ON until') }} ' + (s.maintenance_until || '').replace('T', ' ').replace('Z', ' UTC') : '{{ lang._('off') }}');
        });
    }
    $('#opMaintOn').click(function () {
        ajaxCall('/api/activestandby/pairing/maintenance', {on: '1', hours: $('#opHours').val()}, function (r) { refreshMaint(); });
    });
    $('#opMaintOff').click(function () {
        ajaxCall('/api/activestandby/pairing/maintenance', {on: '0'}, function (r) { refreshMaint(); });
    });

    $('#opPre').click(function () {
        $('#opPreSum').html('<i class="fa fa-spinner fa-spin"></i>');
        $('#opPreOut').empty();
        ajaxGet('/api/activestandby/pairing/preflight', {}, function (r) {
            $('#opPreSum').html('<span class="label label-' + (lbl[r.summary] || 'default') + '">' + esc((r.summary || '?').toUpperCase()) + '</span>');
            const sec = (title, list) => !list || !list.length ? '' :
                '<h4>' + title + '</h4><table class="table table-condensed" style="max-width:900px"><tbody>' +
                list.map(c => '<tr><td style="width:70px"><span class="label label-' + (lbl[c.status] || 'default') + '">' + esc(c.status) + '</span></td><td style="width:230px">' + esc(c.name) + '</td><td>' + esc(c.detail) + '</td></tr>').join('') + '</tbody></table>';
            $('#opPreOut').html(sec('{{ lang._('This node') }}', r.this_node) + sec('{{ lang._('Peer') }}', r.peer) + sec('{{ lang._('Dry run: could the standby take over now?') }}', r.dry_run));
        });
    });

    function refreshHistory() {
        ajaxGet('/api/activestandby/pairing/history', {}, function (r) {
            $('#opHist tbody').html((r.history || []).map(h =>
                '<tr><td>' + esc(h.time.replace('T', ' ').replace('Z', '')) + '</td><td>' + (h.type === 'promote' ? '{{ lang._('became ACTIVE') }}' : '{{ lang._('became STANDBY') }}') +
                '</td><td>' + (h.duration_ms / 1000).toFixed(1) + ' s</td><td>' + esc(h.reason) + '</td></tr>').join(''));
        });
    }

    function refreshJob() {
        ajaxGet('/api/activestandby/pairing/upgradestatus', {}, function (r) {
            const j = r.job;
            if (!j) { $('#opUpJob, #opUpCancel').hide(); return; }
            const running = ['done', 'failed', 'cancelled'].indexOf(j.phase) < 0;
            $('#opUpJob').show();
            $('#opUpCancel').toggle(running);
            $('#opUpStart').prop('disabled', running);
            const cur = j.phase === 'r2-handed-over' ? 'r2-update' : j.phase;
            let idx = PHASES.findIndex(p => p[0] === cur);
            if (cur === 'done') { idx = PHASES.length; }
            $('#opUpPhases').html(PHASES.filter(p => j.failback || p[0] !== 'failback').map((p, i) => {
                const pos = PHASES.findIndex(q => q[0] === p[0]);
                const icon = pos < idx ? '<i class="fa fa-check text-success"></i>' : (pos === idx && running ? '<i class="fa fa-spinner fa-spin"></i>' : '<i class="fa fa-circle-o text-muted"></i>');
                return '<li>' + icon + ' ' + esc(p[1]) + '</li>';
            }).join(''));
            $('#opUpLog').text((j.log || []).join('\n') + (j.err ? '\n\n' + '{{ lang._('STOPPED:') }} ' + j.err : ''));
            if (j.phase === 'failed') { $('#opUpRes').attr('class', 'alert alert-danger').text(j.err).show(); }
            if (j.phase === 'done') { $('#opUpRes').attr('class', 'alert alert-success').text('{{ lang._('Both nodes updated.') }}').show(); }
        });
    }
    $('#opUpStart').click(function () {
        stdDialogConfirm('{{ lang._('Start guided upgrade') }}', '{{ lang._('Both nodes will be updated and rebooted one after the other and the active role switches at least once. Continue?') }}', '{{ lang._('Start') }}', '{{ lang._('Cancel') }}', function () {
            $('#opUpRes').hide();
            ajaxCall('/api/activestandby/pairing/upgradestart', {failback: $('#opFailback').is(':checked') ? '1' : '0'}, function (r) {
                if (r.error) { $('#opUpRes').attr('class', 'alert alert-danger').text(r.error).show(); }
                refreshJob();
            });
        });
    });
    $('#opUpCancel').click(function () { ajaxCall('/api/activestandby/pairing/upgradecancel', {}, function () { refreshJob(); }); });

    refreshMaint(); refreshHistory(); refreshJob();
    setInterval(function () { refreshMaint(); refreshJob(); }, 5000);
    setInterval(refreshHistory, 20000);
});
</script>
