<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Base\ApiControllerBase;
use OPNsense\Core\Config;

/**
 * Read-only helpers for the setup wizard: which interfaces can be used, and what in the current
 * configuration conflicts with active/standby HA.
 */
class WizardController extends ApiControllerBase
{
    private const VIRTUAL = '/^(lo|enc|pflog|pfsync|tun|tap|wg|ovpn|ipsec|gif|gre)\d*$/';

    public function interfacesAction()
    {
        $out = [];
        foreach (Config::getInstance()->object()->interfaces->children() as $key => $n) {
            $dev = (string)$n->if;
            if ($dev === '' || preg_match(self::VIRTUAL, $dev)) {
                continue;
            }
            $ip = (string)$n->ipaddr;
            $static = filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) !== false;
            $out[] = [
                'key' => (string)$key,
                'descr' => (string)$n->descr !== '' ? (string)$n->descr : strtoupper((string)$key),
                'device' => $dev,
                'enabled' => (string)$n->enable !== '',
                'ipv4' => $static ? $ip . '/' . (string)$n->subnet : ($ip !== '' ? $ip : '-'),
                'static' => $static,
            ];
        }
        return ['interfaces' => $out];
    }

    /** Suggested values for the second firewall's kit. */
    public function kitdefaultsAction()
    {
        $cfg = Config::getInstance()->object();
        $g = $cfg->OPNsense->ActiveStandby->general ?? null;
        $out = ['hostname' => (string)$cfg->system->hostname . '-b', 'ha' => '', 'mgmt_static' => false, 'mgmt_key' => ''];
        if ($g !== null && (string)$g->ha_interface !== '') {
            $ip = (string)$cfg->interfaces->{(string)$g->ha_interface}->ipaddr;
            $p = explode('.', $ip);
            if (count($p) === 4) {
                $p[3] = (int)$p[3] >= 254 ? (int)$p[3] - 1 : (int)$p[3] + 1;
                $out['ha'] = implode('.', $p);
            }
            $mk = (string)$g->mgmt_interface;
            if ($mk !== '' && isset($cfg->interfaces->$mk)) {
                $out['mgmt_key'] = $mk;
                $out['mgmt_static'] = (string)$cfg->interfaces->$mk->ipaddr !== 'dhcp';
            }
        }
        return $out;
    }

    /** @return array{errors: string[], warnings: string[], has_ha_rule: bool} */
    public function precheckAction()
    {
        $ha = preg_replace('/[^a-z0-9_]/i', '', (string)$this->request->get('ha'));
        $mgmt = preg_replace('/[^a-z0-9_]/i', '', (string)$this->request->get('mgmt'));
        $cfg = Config::getInstance()->object();
        $errors = [];
        $warnings = [];

        if ($ha === '' || !isset($cfg->interfaces->$ha)) {
            $errors[] = gettext('Select the HA link interface.');
        } else {
            $ip = (string)$cfg->interfaces->$ha->ipaddr;
            if (filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) === false) {
                $errors[] = gettext('The HA link interface needs a static IPv4 address (different on each node).');
            }
            if ((string)$cfg->interfaces->$ha->enable === '') {
                $errors[] = gettext('The HA link interface is not enabled.');
            }
        }
        if ($mgmt !== '' && $mgmt === $ha) {
            $errors[] = gettext('Management and HA link must be different interfaces.');
        }

        // everything that would be shared must be static
        foreach ($cfg->interfaces->children() as $key => $n) {
            if ((string)$key === $ha || (string)$key === $mgmt || (string)$n->enable === '') {
                continue;
            }
            $v4 = (string)$n->ipaddr;
            if (in_array($v4, ['dhcp', 'pppoe', 'pptp', 'l2tp', 'ppp'], true)) {
                $warnings[] = sprintf(
                    gettext('Interface %s uses %s. Shared IP and MAC only make sense with static addresses.'),
                    (string)$key,
                    $v4
                );
            }
        }

        // the OPNsense built-in HA (CARP + XMLRPC) must not run next to this plugin
        foreach ($cfg->virtualip->vip ?? [] as $vip) {
            if ((string)$vip->mode === 'carp') {
                $warnings[] = gettext('CARP virtual IPs exist. They are not used by Active/Standby HA; remove them before relying on this setup.');
                break;
            }
        }
        $core = $cfg->hasync ?? null;
        if ($core !== null && ((string)$core->synchronizetoip !== '' || (string)$core->pfsyncinterface !== '')) {
            $warnings[] = gettext('The built-in High Availability sync (System > High Availability) is configured. Clear it so the two mechanisms do not fight.');
        }

        // is traffic on the HA link allowed?
        $hasRule = false;
        if ($ha !== '') {
            foreach ($cfg->OPNsense->Firewall->Filter->rules->rule ?? [] as $r) {
                if (in_array($ha, explode(',', (string)$r->interface), true) && (string)$r->action === 'pass') {
                    $hasRule = true;
                }
            }
            foreach ($cfg->filter->rule ?? [] as $r) {
                if ((string)$r->interface === $ha && (string)$r->type === 'pass') {
                    $hasRule = true;
                }
            }
            if (!$hasRule) {
                $warnings[] = gettext('No pass rule on the HA link interface. Heartbeat, config sync and pfsync need one; the wizard can create it.');
            }
        }
        return ['errors' => $errors, 'warnings' => $warnings, 'has_ha_rule' => $hasRule];
    }
}
