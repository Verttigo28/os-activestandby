<?php

namespace OPNsense\ActiveStandby\Menu;

use OPNsense\Base\Menu\MenuContainer;
use OPNsense\Core\Config;

/**
 * While Active/Standby HA is enabled it replaces OPNsense's built-in CARP/XMLRPC "High Availability"
 * pages, so that menu entry is hidden to avoid two competing HA mechanisms.
 */
class Menu extends MenuContainer
{
    public function collect()
    {
        $general = Config::getInstance()->object()->OPNsense->ActiveStandby->general ?? null;
        if ($general !== null && (string)$general->enabled === '1') {
            $this->appendItem('System', 'HighAvailability', ['visibility' => 'delete']);
        }
    }
}
