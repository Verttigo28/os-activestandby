#!/usr/local/bin/php
<?php

/*
 * Run on the MASTER right after pairing.
 * Pins the live MAC address of every enabled data interface into <spoofmac> in config.xml.
 * For the running master this changes nothing (the MAC is already in use), but the config that is
 * replicated to the standby then makes the standby's interfaces come up with identical MACs.
 * The HA link and the management interface are node-local and are never touched.
 */

require_once('config.inc');
require_once('interfaces.inc');

use OPNsense\Core\Config;
use OPNsense\ActiveStandby\ActiveStandby;

$mdl = new ActiveStandby();
$skip = array_filter([(string)$mdl->general->ha_interface, (string)$mdl->general->mgmt_interface]);
if (count($skip) == 0) {
    fwrite(STDERR, "no HA interface configured\n");
    exit(1);
}

$details = legacy_interfaces_details();
$cfg = Config::getInstance()->object();
$pinned = [];
foreach ($cfg->interfaces->children() as $key => $node) {
    if (in_array((string)$key, $skip) || empty((string)$node->enable)) {
        continue;
    }
    $dev = (string)$node->if;
    $mac = $details[$dev]['macaddr'] ?? '';
    if (
        !preg_match('/^([0-9a-f]{2}:){5}[0-9a-f]{2}$/i', $mac) || $mac === '00:00:00:00:00:00' ||
        (hexdec(substr($mac, 0, 2)) & 1) || preg_match('/^(lo|enc|pflog|pfsync|tun|wg|ovpn)\d*$/', $dev)
    ) {
        continue; // not an ethernet-like device (loopback, tunnel, multicast/zero address, ...)
    }
    if ((string)$node->spoofmac === '') {
        $node->spoofmac = strtolower($mac);
        $pinned[] = "$key($dev)=" . strtolower($mac);
    }
}

if (count($pinned) > 0) {
    Config::getInstance()->save();
}
echo count($pinned) > 0 ? 'pinned ' . implode(' ', $pinned) : 'nothing to pin';
echo "\n";
