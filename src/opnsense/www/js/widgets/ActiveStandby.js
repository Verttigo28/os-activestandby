/*
 * Dashboard widget for os-activestandby: role, peer, heartbeat links, config sync and the last event.
 */
export default class ActiveStandby extends BaseTableWidget {
    constructor() {
        super();
        this.tickCount = 0;
        this.lastEvent = null;
    }

    getGridOptions() {
        return {sizeToContent: 400};
    }

    getMarkup() {
        return $('<div></div>').append(this.createTable('as-table', {headerPosition: 'left'}));
    }

    esc(s) {
        return $('<div>').text(s === undefined || s === null ? '' : String(s)).html();
    }

    badge(text, color) {
        return `<span class="badge badge-pill" style="background-color:${color}">${this.esc(text)}</span>`;
    }

    async onWidgetTick() {
        const s = await this.ajaxCall('/api/activestandby/pairing/status');
        if (!s || s.role === undefined || s.role === 'standalone') {
            $('#as-table').html(`<a href="/ui/activestandby">${this.translations.unconfigured}</a>`);
            return;
        }
        // events change rarely: refresh every 5th tick only
        if (this.tickCount++ % 5 === 0) {
            const e = await this.ajaxCall('/api/activestandby/pairing/events');
            this.lastEvent = e && e.events && e.events.length ? e.events[0] : null;
        }
        const t = this.translations;
        const active = s.state === 'active';
        const stateBadge = this.badge(active ? t.active : t.standby, active ? 'green' : '#6c757d');
        const role = s.role === 'master' ? 'primary' : 'secondary';

        let peer = s.peer_state ? this.badge(s.peer_state === 'active' ? t.active : t.standby, s.peer_state === 'active' ? 'green' : '#6c757d') : '-';
        peer += ' ' + this.esc(s.peer_addr || '') + ' ' + (s.peer_alive ? t.reachable : `<b style="color:#c00">${t.unreachable}</b>`);

        const p = s.paths || {};
        const dot = ok => `<span style="color:${ok ? 'green' : '#c00'}">&#9679;</span>`;
        let links = `${dot(p.ha)} ${t.ha}`;
        if (s.peer_mgmt) {
            links += ` &nbsp; ${dot(p.mgmt)} ${t.mgmt}`;
        }

        let sync = '-';
        if (s.sync) {
            if (s.sync.state === 'in_sync') {
                sync = this.badge(t.insync, 'green') + ` rev ${this.esc(s.sync.rev)}`;
            } else if (s.sync.state === 'behind') {
                sync = this.badge(`${s.sync.behind} ${t.behind}`, '#e68a00');
            } else {
                sync = this.badge(t.failed, '#c00') + ' ' + this.esc(s.sync.error || '');
            }
        }

        const rows = [
            [t.thisnode, `${stateBadge} (${role}) ${this.esc(s.ha_address || '')}`],
            [t.peer, peer],
            [t.links, links],
            [t.sync, sync],
        ];
        if (s.version_mismatch) {
            rows.push([t.versions, `<span style="color:#e68a00">${this.esc(s.os_version)} / ${this.esc(s.peer_os_version)}</span>`]);
        }
        if (s.auto_blocked) {
            rows.push([t.held, `<span style="color:#c00">${this.esc(s.auto_blocked)}</span>`]);
        }
        if (this.lastEvent) {
            rows.push([t.lastevent, `<small>${this.esc(this.lastEvent.time.replace('T', ' ').replace('Z', ''))}<br>${this.esc(this.lastEvent.msg)}</small>`]);
        }
        rows.forEach((r, i) => this.updateTable('as-table', [r], `as_row_${i}`));
    }
}
