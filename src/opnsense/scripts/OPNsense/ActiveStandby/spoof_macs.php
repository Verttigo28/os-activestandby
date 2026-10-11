#!/usr/local/bin/php
<?php

/*
 * Run on the MASTER right after pairing.
 * Gives every enabled data interface a shared MAC in <spoofmac> (config.xml) that both nodes will use.
 * Default: a generated locally administered "virtual" MAC (02:xx:...), independent of either node's hardware, so
 * replacing a node never changes the MAC the network sees. With the setting off the live hardware MAC is pinned
 * instead (a no-op for the running master). Interfaces that already have a <spoofmac> are left alone.
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

$virtual = (string)$mdl->general->virtual_mac !== '0';
$used = [];
foreach (Config::getInstance()->object()->interfaces->children() as $n) {
    if ((string)$n->spoofmac !== '') {
        $used[strtolower((string)$n->spoofmac)] = true;
    }
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
        $new = strtolower($mac);
        if ($virtual) {
            do {
                $new = sprintf('02:%s', implode(':', str_split(bin2hex(random_bytes(5)), 2)));
            } while (isset($used[$new]));
            $used[$new] = true;
        }
        $node->spoofmac = $new;
        $pinned[] = "$key($dev)=" . $new;
    }
}

if (count($pinned) > 0) {
    Config::getInstance()->save();
}
echo count($pinned) > 0 ? 'pinned ' . implode(' ', $pinned) : 'nothing to pin';
echo "\n";
