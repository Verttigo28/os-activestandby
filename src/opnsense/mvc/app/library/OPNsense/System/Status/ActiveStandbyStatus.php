<?php

/*
 * Shows a notice in the OPNsense status bar on every page while this node is the STANDBY of an active/standby pair:
 * configuration made here is not kept, the active node's configuration replaces it.
 * The daemon publishes its role in a world-readable marker file (the GUI cannot read the state directory).
 */

namespace OPNsense\System\Status;

use OPNsense\System\AbstractStatus;
use OPNsense\System\SystemStatusCode;

class ActiveStandbyStatus extends AbstractStatus
{
    public function __construct()
    {
        $this->internalPriority = 5;
        $this->internalTitle = gettext('Active/Standby HA');
        $this->internalLocation = '/ui/activestandby';
    }

    public function collectStatus()
    {
        $role = @file_get_contents('/var/run/activestandby.state');
        if ($role === 'standby') {
            $this->internalMessage = gettext(
                'This firewall is the STANDBY node. Its configuration is locked and replaced by the active node: ' .
                'make changes on the active node, or make this node active first.'
            );
            $this->internalStatus = SystemStatusCode::NOTICE;
            $this->internalTimestamp = @filemtime('/var/run/activestandby.state') ?: time();
        }
    }
}
