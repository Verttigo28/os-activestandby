#!/usr/local/bin/php
<?php

/*
 * Faster interface bring-up for promote. The stock "configctl interface reconfigure <if>" runs, per interface, the
 * interface itself AND then routing, IPsec, DHCP and DNS (each restart of dnsmasq/unbound costs a few hundred ms).
 * Here those parts are separate steps so they run once and side by side:
 *
 *   reconfigure_ifaces.php iface <key>   configure one interface only (several can run at the same time)
 *   reconfigure_ifaces.php routes        routing table and default gateway
 *   reconfigure_ifaces.php dns           DHCP and DNS services (re-read the replicated lease files)
 *   reconfigure_ifaces.php all <key>...  everything, like the stock action for several interfaces
 */

require_once("config.inc");
require_once('auth.inc');
require_once("util.inc");
require_once("filter.inc");
require_once("system.inc");
require_once("interfaces.inc");

$mode = $argv[1] ?? '';
$rc = 0;
try {
    switch ($mode) {
        case 'iface':
            // reload=false: the interface itself only. reload=true also re-creates the device and, per interface, restarts routing,
            // IPsec, DHCP and DNS (Unbound alone costs about 0.5 s each): those run once, after all interfaces, in the steps below.
            interface_configure(true, trim($argv[2] ?? ''), false);
            break;
        case 'routes':
            system_routing_configure(true);
            break;
        case 'dns':
            plugins_configure('dhcp', true);
            plugins_configure('dns', true);
            break;
        case 'all':
            foreach (array_slice($argv, 2) as $key) {
                interface_configure(true, trim($key), true);
            }
            break;
        default:
            fwrite(STDERR, "usage: reconfigure_ifaces.php iface <key> | routes | dns | all <key>...\n");
            $rc = 2;
    }
} catch (\Throwable $e) {
    fwrite(STDERR, "reconfigure {$mode}: " . $e->getMessage() . "\n");
    $rc = 1;
}
exit($rc);
