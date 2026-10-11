<?php

namespace OPNsense\ActiveStandby\Api;

use OPNsense\Base\ApiControllerBase;
use OPNsense\Core\Backend;

/**
 * Thin wrapper: every action maps to `hasyncd ctl <cmd>` through configd.
 * User input is validated here, configd passes it on as arguments, never through a shell string.
 */
class PairingController extends ApiControllerBase
{
    private function ctl(string $action, array $params = []): array
    {
        $out = trim((new Backend())->configdpRun("activestandby $action", $params));
        $json = json_decode($out, true);
        return is_array($json) ? $json : ['error' => $out === '' ? 'no answer from hasyncd' : $out];
    }

    public function statusAction()
    {
        return $this->ctl('ctl_status');
    }

    /** Maintenance mode on/off for both nodes (automatic failover paused). */
    public function maintenanceAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        $on = $this->request->getPost('on') === '1' ? 'on' : 'off';
        $hours = (int)$this->request->getPost('hours');
        return $this->ctl('ctl_maintenance', [$on, (string)max(1, min(72, $hours ?: 4))]);
    }

    /** Health of both nodes plus a dry run of the standby taking over (changes nothing). */
    public function preflightAction()
    {
        return $this->ctl('ctl_preflight');
    }

    public function historyAction()
    {
        return $this->ctl('ctl_history');
    }

    public function healthAction()
    {
        return $this->ctl('ctl_health');
    }

    public function upgradestartAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        return $this->ctl($this->request->getPost('failback') === '1' ? 'ctl_upgrade_start_failback' : 'ctl_upgrade_start');
    }

    public function upgradecancelAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_upgrade_cancel') : ['error' => 'POST required'];
    }

    public function upgradestatusAction()
    {
        return $this->ctl('ctl_upgrade_status');
    }

    public function infoAction()
    {
        return $this->ctl('ctl_info');
    }

    /** Standby side: open the pairing window and return the one-time token. */
    public function prepareAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_prepare') : ['error' => 'POST required'];
    }

    public function cancelAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_cancel') : ['error' => 'POST required'];
    }

    /** Master side: pair with a prepared standby. */
    public function pairAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        $peer = trim((string)$this->request->getPost('peer'));
        $token = trim((string)$this->request->getPost('token'));
        if (filter_var($peer, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) === false) {
            return ['error' => gettext('Peer must be the IPv4 address of the other node on the HA link.')];
        }
        if (!preg_match('/^[A-Za-z2-7]{5}(-?[A-Za-z2-7]{5}){3}$/', $token)) {
            return ['error' => gettext('Invalid pairing token format.')];
        }
        return $this->ctl('ctl_pair', [$peer, $token]);
    }

    /** Primary: build the configuration file for the second firewall (restore it there, pairing is automatic). */
    public function kitAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        $ha = trim((string)$this->request->getPost('ha'));
        $mgmt = trim((string)$this->request->getPost('mgmt'));
        $host = trim((string)$this->request->getPost('hostname'));
        if (filter_var($ha, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) === false) {
            return ['error' => gettext('Enter the HA link IPv4 address of the second firewall.')];
        }
        if ($mgmt !== '' && $mgmt !== 'dhcp' && filter_var($mgmt, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) === false) {
            return ['error' => gettext('The management address must be an IPv4 address or dhcp.')];
        }
        if (!preg_match('/^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/', $host)) {
            return ['error' => gettext('Invalid hostname.')];
        }
        return $this->ctl('ctl_kit', [$ha, $mgmt === '' ? '-' : $mgmt, $host]);
    }

    public function kitcancelAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_kit_cancel') : ['error' => 'POST required'];
    }

    /** Graceful switchover; works from either node. */
    public function failoverAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        return $this->ctl($this->request->getPost('force') === '1' ? 'ctl_failover_force' : 'ctl_failover');
    }

    /** Make this node active. Without force this is refused while the peer is alive and active. */
    public function promoteAction()
    {
        if (!$this->request->isPost()) {
            return ['error' => 'POST required'];
        }
        return $this->ctl($this->request->getPost('force') === '1' ? 'ctl_promote_force' : 'ctl_promote');
    }

    public function demoteAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_demote') : ['error' => 'POST required'];
    }

    public function eventsAction()
    {
        return $this->ctl('ctl_events');
    }

    public function resyncAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_resync') : ['error' => 'POST required'];
    }

    public function breakAction()
    {
        return $this->request->isPost() ? $this->ctl('ctl_break') : ['error' => 'POST required'];
    }
}
