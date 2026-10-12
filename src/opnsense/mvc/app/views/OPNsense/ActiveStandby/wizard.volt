<div id="wizard" class="tab-pane fade in">
    <div class="col-md-12">
        <h2>{{ lang._('Setup wizard') }}</h2>
        <p>{{ lang._('Set up a pair of firewalls. Run the wizard on the firewall that holds your production configuration (the primary): it can build the second firewall for you. Only if the second firewall is already installed and running, run the wizard there too and choose Secondary.') }}</p>

        <div class="wz-step" id="wz1">
            <h3>1. {{ lang._('Interfaces') }}</h3>
            <div class="form-group" style="max-width:600px">
                <label>{{ lang._('HA link (dedicated cable between the two nodes, static IPv4, different on each node)') }}</label>
                <select class="form-control" id="wzHa"></select>
            </div>
            <div class="form-group" style="max-width:600px">
                <label>{{ lang._('Management interface (optional, stays up on the standby, second heartbeat path)') }}</label>
                <select class="form-control" id="wzMgmt"></select>
            </div>
            <div class="checkbox"><label><input type="checkbox" id="wzRule" checked/> {{ lang._('Create a firewall rule that allows traffic on the HA link if none exists') }}</label></div>
            <div id="wzCheck" style="margin:10px 0"></div>
            <button class="btn btn-default" id="wzCheckBtn">{{ lang._('Check') }}</button>
            <button class="btn btn-primary" id="wzNext1" disabled>{{ lang._('Save and continue') }}</button>
            <div id="wzApply" class="alert" style="display:none; margin-top:10px"></div>
        </div>

        <div class="wz-step" id="wz2" style="display:none">
            <h3>2. {{ lang._('Role of this node') }}</h3>
            <div class="radio"><label><input type="radio" name="wzRole" value="primary"/>
                <b>{{ lang._('Primary') }}</b> &mdash; {{ lang._('this node keeps its configuration and is active first.') }}</label></div>
            <div class="radio"><label><input type="radio" name="wzRole" value="secondary"/>
                <b>{{ lang._('Secondary') }}</b> &mdash; {{ lang._('this node starts as standby. Its configuration (except hostname and the HA and management interface) is REPLACED by the primary and cannot be recovered.') }}</label></div>
            <button class="btn btn-default" id="wzBack2">{{ lang._('Back') }}</button>
            <button class="btn btn-primary" id="wzNext2" disabled>{{ lang._('Continue') }}</button>
        </div>

        <div class="wz-step" id="wz3p" style="display:none">
            <h3>3. {{ lang._('Add the second firewall') }}</h3>
            <div class="well" style="max-width:820px">
                <h4>{{ lang._('Automatic (recommended)') }}</h4>
                <p>{{ lang._('Download a ready-made configuration file for the second firewall. It carries the second firewall\'s own name and addresses, the same interface settings and a one-time pairing key. Pairing then happens by itself.') }}</p>
                <div class="row">
                    <div class="col-sm-4"><div class="form-group"><label>{{ lang._('Hostname of the second firewall') }}</label><input class="form-control" id="kitHost"/></div></div>
                    <div class="col-sm-4"><div class="form-group"><label>{{ lang._('Its HA link address') }}</label><input class="form-control" id="kitHa" placeholder="10.99.0.2"/></div></div>
                    <div class="col-sm-4" id="kitMgmtBox" style="display:none"><div class="form-group"><label>{{ lang._('Its management address (IPv4 or dhcp)') }}</label><input class="form-control" id="kitMgmt" placeholder="dhcp"/></div></div>
                </div>
                <button class="btn btn-primary" id="kitDownload"><i class="fa fa-download"></i> {{ lang._('Download configuration for the second firewall') }}</button>
                <div id="kitRes" class="alert" style="display:none; margin-top:12px"></div>
                <div id="kitSteps" style="display:none; margin-top:12px">
                    <ol style="line-height:1.8">
                        <li>{{ lang._('Install OPNsense on the second machine (same version and hardware, same network ports).') }}</li>
                        <li>{{ lang._('Install the os-activestandby plugin on it.') }}</li>
                        <li>{{ lang._('Cable the HA link, then restore the downloaded file: System > Configuration > Backups > Restore.') }}</li>
                        <li>{{ lang._('Reboot it. This page notices when it comes up and pairs with it automatically.') }}</li>
                    </ol>
                    <div id="kitWait" class="alert alert-info"><i class="fa fa-spinner fa-spin"></i> <span id="kitWaitTxt"></span></div>
                    <button class="btn btn-default btn-sm" id="kitCancel">{{ lang._('Stop waiting') }}</button>
                </div>
            </div>
            <p><a href="#" id="wzManualToggle">{{ lang._('Pair manually with a token instead') }}</a></p>
            <div id="wzManual" style="display:none">
                <p>{{ lang._('Complete the wizard on the secondary first, it shows an address and a one-time token.') }}</p>
                <div class="form-group" style="max-width:400px"><label>{{ lang._('Secondary HA link address') }}</label><input class="form-control" id="wzPeer" placeholder="10.99.0.2"/></div>
                <div class="form-group" style="max-width:400px"><label>{{ lang._('Pairing token') }}</label><input class="form-control" id="wzToken" placeholder="XXXXX-XXXXX-XXXXX-XXXXX"/></div>
                <button class="btn btn-primary" id="wzPair">{{ lang._('Pair') }}</button>
                <div id="wzPairRes" class="alert" style="display:none; margin-top:10px"></div>
            </div>
            <button class="btn btn-default wzBackRole" style="margin-top:10px">{{ lang._('Back') }}</button>
        </div>

        <div class="wz-step" id="wz3s" style="display:none">
            <h3>3. {{ lang._('Wait for the primary') }}</h3>
            <p>{{ lang._('Open the pairing window here, then run the wizard on the PRIMARY and enter the address and the token shown below. The window stays open for 10 minutes. Everything on this firewall except its hostname and the HA and management interface will be replaced by the configuration of the primary.') }}</p>
            <button class="btn btn-primary" id="wzPrepare">{{ lang._('Open pairing window') }}</button>
            <button class="btn btn-default" id="wzPrepCancel" style="display:none">{{ lang._('Close the pairing window') }}</button>
            <div id="wzPrepRes" class="alert" style="display:none; margin-top:10px"></div>
            <button class="btn btn-default wzBackRole" style="margin-top:10px">{{ lang._('Back') }}</button>
        </div>

        <div class="wz-step" id="wz4" style="display:none">
            <h3>4. {{ lang._('Done') }}</h3>
            <div class="alert alert-success" id="wzDone"></div>
            <p>{{ lang._('The secondary now receives the complete configuration of the primary. Watch the sync state on the Status tab.') }}</p>
            <button class="btn btn-primary" id="wzToStatus">{{ lang._('Go to status') }}</button>
        </div>
    </div>
</div>

<script>
$(function () {
    let ifs = [];
    const esc = s => $('<div>').text(s === undefined || s === null ? '' : s).html();
    const step = id => { $('.wz-step').hide(); $('#' + id).show(); };
    const alertBox = (sel, cls, html) => $(sel).attr('class', 'alert ' + cls).html(html).show();

    ajaxGet('/api/activestandby/wizard/interfaces', {}, function (r) {
        ifs = r.interfaces || [];
        const lbl = i => i.descr + ' (' + i.device + ', ' + i.ipv4 + (i.enabled ? '' : ', disabled') + ')';
        $('#wzHa').html('<option value="">-</option>' + ifs.map(i => '<option value="' + esc(i.key) + '">' + esc(lbl(i)) + '</option>').join(''));
        $('#wzMgmt').html('<option value="">{{ lang._('(none)') }}</option>' + ifs.map(i => '<option value="' + esc(i.key) + '">' + esc(lbl(i)) + '</option>').join(''));
    });

    function check(done) {
        ajaxGet('/api/activestandby/wizard/precheck', {ha: $('#wzHa').val(), mgmt: $('#wzMgmt').val()}, function (r) {
            let h = '';
            (r.errors || []).forEach(e => h += '<div class="alert alert-danger">' + esc(e) + '</div>');
            (r.warnings || []).forEach(w => h += '<div class="alert alert-warning">' + esc(w) + '</div>');
            if (!h) { h = '<div class="alert alert-success">{{ lang._('No problems found.') }}</div>'; }
            $('#wzCheck').html(h);
            $('#wzNext1').prop('disabled', (r.errors || []).length > 0);
            if (done) { done(r); }
        });
    }
    $('#wzCheckBtn').click(() => check());
    $('#wzHa, #wzMgmt').change(function () { $('#wzNext1').prop('disabled', true); if ($('#wzHa').val()) { check(); } }); // checked as soon as the choice changes
    // start from what is saved, so a reload in the middle of the wizard does not lose the choice
    ajaxGet('/api/activestandby/settings/get', {}, function (r) {
        const g = (r.activestandby || {}).general || {};
        const sel = o => { for (const k in (o || {})) { if (o[k] && o[k].selected) { return k; } } return ''; };
        const ha = sel(g.ha_interface), mg = sel(g.mgmt_interface);
        if (ha) { $('#wzHa').val(ha); }
        if (mg) { $('#wzMgmt').val(mg); }
        if (ha) { check(); }
    });

    $('#wzNext1').click(function () {
        const ha = $('#wzHa').val(), mgmt = $('#wzMgmt').val();
        alertBox('#wzApply', 'alert-info', '{{ lang._('Saving...') }}');
        check(function (pre) {
            if ((pre.errors || []).length) { $('#wzApply').hide(); return; }
            const finish = function () {
                ajaxCall('/api/activestandby/service/reconfigure', {}, function () {
                    let tries = 0;
                    const wait = setInterval(function () {
                        ajaxGet('/api/activestandby/pairing/status', {}, function (s) {
                            if (s.daemon === 'up' || ++tries > 20) {
                                clearInterval(wait);
                                $('#wzApply').hide();
                                s.daemon === 'up' ? step('wz2') : alertBox('#wzApply', 'alert-danger', '{{ lang._('The HA daemon did not start. Check /var/log/hasyncd.log.') }}');
                            }
                        });
                    }, 1000);
                });
            };
            ajaxCall('/api/activestandby/settings/set', {activestandby: {general: {enabled: '1', ha_interface: ha, mgmt_interface: mgmt}}}, function (r) {
                if (r.result !== 'saved') { alertBox('#wzApply', 'alert-danger', esc(JSON.stringify(r.validations || r))); return; }
                if (window.asReloadSettings) { window.asReloadSettings(); }
                if ($('#wzRule').is(':checked') && !pre.has_ha_rule) {
                    // only what the HA link needs, from the HA segment to this firewall: the peer API and update proxy (TCP 7690-7691),
                    // ping, and pfsync. Not "any": the GUI, SSH and DNS stay closed on this interface.
                    const base = {enabled: '1', action: 'pass', interface: ha, direction: 'in', ipprotocol: 'inet', source_net: ha, destination_net: '(self)'};
                    ajaxCall('/api/firewall/filter/addRule', {rule: Object.assign({}, base, {protocol: 'TCP', destination_port: '7690-7691', description: 'ActiveStandby: peer API and update proxy'})}, function () {
                        ajaxCall('/api/firewall/filter/addRule', {rule: Object.assign({}, base, {protocol: 'ICMP', description: 'ActiveStandby: ping on the HA link'})}, function () {
                            ajaxCall('/api/firewall/filter/addRule', {rule: Object.assign({}, base, {protocol: 'pfsync', description: 'ActiveStandby: pfsync'})}, function () {
                                ajaxCall('/api/firewall/filter/apply', {}, finish);
                            });
                        });
                    });
                } else { finish(); }
            });
        });
    });

    $('#wzBack2').click(() => step('wz1'));
    $('.wzBackRole').click(() => step('wz2'));
    $('input[name=wzRole]').change(function () { $('#wzNext2').prop('disabled', false); });
    $('#wzNext2').click(function () {
        const primary = $('input[name=wzRole]:checked').val() === 'primary';
        if (primary) { step('wz3p'); return; }
        stdDialogConfirm('{{ lang._('Make this firewall the secondary?') }}', '{{ lang._('The configuration of this firewall (except hostname and the HA and management interface) will be REPLACED by the primary and cannot be recovered. Only continue on the firewall that should become the standby.') }}', '{{ lang._('Continue as secondary') }}', '{{ lang._('Cancel') }}', function () { step('wz3s'); });
    });

    function paired(msg) {
        $('#wzDone').text(msg);
        step('wz4');
    }

    $('#wzManualToggle').click(function (e) { e.preventDefault(); $('#wzManual').toggle(); });
    ajaxGet('/api/activestandby/wizard/kitdefaults', {}, function (r) {
        $('#kitHost').val(r.hostname || '');
        $('#kitHa').val(r.ha || '');
        if (r.mgmt_key) { $('#kitMgmtBox').show(); $('#kitMgmt').attr('placeholder', r.mgmt_static ? '192.168.x.y' : 'dhcp').val(r.mgmt_static ? '' : 'dhcp'); }
    });
    let pollKit = null;
    function watchKit() {
        $('#kitSteps').show();
        clearInterval(pollKit);
        pollKit = setInterval(function () {
            ajaxGet('/api/activestandby/pairing/status', {}, function (s) {
                if (s.role === 'master') { clearInterval(pollKit); paired('{{ lang._('The second firewall came up and was paired. It is now the standby and receives the complete configuration.') }}'); return; }
                if (!s.kit) { clearInterval(pollKit); $('#kitWait').attr('class', 'alert alert-warning').text('{{ lang._('Stopped waiting for the second firewall.') }}'); return; }
                $('#kitWaitTxt').text('{{ lang._('Waiting for the second firewall at') }} ' + s.kit.peer + (s.kit.error ? ' (' + s.kit.error + ')' : ''));
            });
        }, 3000);
    }
    $('#kitDownload').click(function () {
        const btn = $(this);
        stdDialogConfirm('{{ lang._('Download the configuration of the second firewall?') }}', '{{ lang._('The file is a complete copy of the configuration of this firewall, including users, password hashes, keys and a pairing key. Anyone who gets it can impersonate this firewall. Keep it safe and delete it after use. Creating a new file invalidates the previous one.') }}', '{{ lang._('Download') }}', '{{ lang._('Cancel') }}', function () {
            btn.prop('disabled', true);
            alertBox('#kitRes', 'alert-info', '{{ lang._('Building...') }}');
            ajaxCall('/api/activestandby/pairing/kit', {ha: $('#kitHa').val(), mgmt: $('#kitMgmt').val(), hostname: $('#kitHost').val()}, function (r) {
                btn.prop('disabled', false);
                if (!r || r.error || !r.xml) { alertBox('#kitRes', 'alert-danger', esc((r && r.error) || '{{ lang._('No answer from the firewall.') }}')); return; }
                const url = URL.createObjectURL(new Blob([r.xml], {type: 'text/xml'}));
                const a = document.createElement('a');
                a.href = url; a.download = r.filename;
                document.body.appendChild(a); a.click(); a.remove();
                alertBox('#kitRes', 'alert-success', '{{ lang._('Downloaded') }} <b>' + esc(r.filename) + '</b>. {{ lang._('It contains secrets: keep it safe and delete it afterwards. It is valid for 48 hours.') }} <a href="' + url + '" download="' + esc(r.filename) + '">{{ lang._('Download again') }}</a>');
                watchKit();
            });
        });
    });
    $('#kitCancel').click(function () { ajaxCall('/api/activestandby/pairing/kitcancel', {}, function () {}); });
    ajaxGet('/api/activestandby/pairing/status', {}, function (s) { if (s.kit) { watchKit(); } });

    $('#wzPair').click(function () {
        const btn = $(this).prop('disabled', true);
        alertBox('#wzPairRes', 'alert-info', '{{ lang._('Pairing...') }}');
        ajaxCall('/api/activestandby/pairing/pair', {peer: $('#wzPeer').val(), token: $('#wzToken').val()}, function (r) {
            btn.prop('disabled', false);
            if (!r || !r.status && !r.error) { alertBox('#wzPairRes', 'alert-danger', '{{ lang._('No answer from the firewall.') }}'); return; }
            if (r.error) {
                let h = esc(r.error);
                if (r.diffs && r.diffs.length) { h += '<ul>' + r.diffs.map(d => '<li>' + esc(d) + '</li>').join('') + '</ul>'; }
                alertBox('#wzPairRes', 'alert-danger', h);
            } else {
                paired('{{ lang._('Paired. This node is the primary and active.') }}');
            }
        });
    });

    let pollPrep = null;
    $('#wzPrepare').click(function () {
        ajaxCall('/api/activestandby/pairing/prepare', {}, function (r) {
            if (r.error) { alertBox('#wzPrepRes', 'alert-danger', esc(r.error)); return; }
            const until = Date.parse(r.expires);
            const show = function () {
                const left = Math.max(0, Math.round((until - Date.now()) / 1000));
                alertBox('#wzPrepRes', left > 0 ? 'alert-info' : 'alert-warning', left > 0
                    ? '{{ lang._('On the primary enter address') }} <b>' + esc(r.address) + '</b> {{ lang._('and token') }} <b style="font-size:150%; font-family:monospace">' + esc(r.token) + '</b><br><small>{{ lang._('Time left') }}: ' + Math.floor(left / 60) + ':' + ('0' + (left % 60)).slice(-2) + '</small>'
                    : '{{ lang._('The pairing window has closed. Open it again.') }}');
                $('#wzPrepCancel').toggle(left > 0);
                return left;
            };
            show();
            clearInterval(pollPrep);
            pollPrep = setInterval(function () {
                if (show() === 0) { clearInterval(pollPrep); return; }
                ajaxGet('/api/activestandby/pairing/status', {}, function (s) {
                    if (s.role === 'standby') { clearInterval(pollPrep); $('#wzPrepCancel').hide(); paired('{{ lang._('Paired. This node is the secondary (standby). Its configuration will be replaced by the primary.') }}'); }
                });
            }, 2000);
        });
    });
    $('#wzPrepCancel').click(function () { clearInterval(pollPrep); ajaxCall('/api/activestandby/pairing/cancel', {}, function () { $('#wzPrepRes').hide(); $('#wzPrepCancel').hide(); }); });
    $('#wzToStatus').click(function () { $('#maintabs a[href="#status"]').tab('show'); location.reload(); });
});
</script>
