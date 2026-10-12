<?php

/*
 * Shows a notice in the OPNsense status bar on every page:
 *   - while this node is the STANDBY of an active/standby pair (its configuration is locked and replaced by the active node),
 *   - and, on either node, when something needs attention: maintenance mode left on, configuration sync failing, the other
 *     firewall silent, configuration drift, different versions.
 * The daemon publishes this in a world-readable marker file (the GUI cannot read the state directory): the first line is the
 * role, the second line a space separated list of flags.
 */

namespace OPNsense\System\Status;

use OPNsense\System\AbstractStatus;
use OPNsense\System\SystemStatusCode;

class ActiveStandbyStatus extends AbstractStatus
{
    private const MARKER = '/var/run/activestandby.state';

    public function __construct()
    {
        $this->internalPriority = 5;
        $this->internalTitle = gettext('Active/Standby HA');
        $this->internalLocation = '/ui/activestandby';
    }

    public function collectStatus()
    {
        $raw = @file_get_contents(self::MARKER);
        if ($raw === false || $raw === '') {
            return;
        }
        $lines = explode("\n", trim($raw));
        $role = $lines[0];
        $flags = isset($lines[1]) && $lines[1] !== '' ? explode(' ', $lines[1]) : [];
        $messages = [];
        $status = SystemStatusCode::NOTICE;

        if ($role === 'standby') {
            $messages[] = gettext(
                'This firewall is the STANDBY node. Its configuration is locked and replaced by the active node: ' .
                'make changes on the active node, or make this node active first.'
            );
        }
        $texts = [
            'maintenance' => [SystemStatusCode::WARNING, gettext('HA maintenance mode is on: automatic failover is paused on both firewalls.')],
            'syncfail' => [SystemStatusCode::ERROR, gettext('HA configuration sync is failing: the other firewall does not have the current configuration.')],
            'peerdown' => [SystemStatusCode::ERROR, gettext('The other HA firewall does not answer: there is no failover protection.')],
            'drift' => [SystemStatusCode::WARNING, gettext('HA configuration drift detected: the standby configuration was changed outside of sync.')],
            'versions' => [SystemStatusCode::WARNING, gettext('The two HA firewalls run different OPNsense or plugin versions.')],
        ];
        foreach ($flags as $f) {
            if (isset($texts[$f])) {
                $messages[] = $texts[$f][1];
                // the worst condition decides the colour (lower value = more severe: ERROR -1, WARNING 0, NOTICE 1)
                if ($texts[$f][0]->value < $status->value) {
                    $status = $texts[$f][0];
                }
            }
        }
        if (count($messages) > 0) {
            $this->internalMessage = implode(' ', $messages);
            $this->internalStatus = $status;
            $this->internalTimestamp = @filemtime(self::MARKER) ?: time();
        }
    }
}
