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
/*
 * A VLAN with a MAC of its own is not received on a real NIC unless the NIC is promiscuous (the receive filter holds the
 * MAC of the parent only; measured on an Intel X710 under a failover LAGG: ARP works, no unicast packet arrives). So a
 * VLAN on an assigned parent shares the parent's MAC (parents are pinned first), and a VLAN on a parent that is not
 * assigned gets its own MAC together with <promisc> (OPNsense applies it each time the interface is configured).
 */
$vlanParent = [];
foreach ($cfg->vlans->vlan ?? [] as $v) {
    $vlanParent[(string)$v->vlanif] = (string)$v->if;
}
$parentNode = [];
foreach ($cfg->interfaces->children() as $key => $node) {
    if (!in_array((string)$key, $skip) && !empty((string)$node->enable) && !isset($vlanParent[(string)$node->if])) {
        $parentNode[(string)$node->if] = $node;
    }
}
$order = [];
foreach ($cfg->interfaces->children() as $key => $node) {
    $order[(isset($vlanParent[(string)$node->if]) ? 1 : 0) . '|' . count($order)] = [(string)$key, $node];
}
ksort($order);
foreach ($order as [$key, $node]) {
    if (in_array((string)$key, $skip) || empty((string)$node->enable)) {
        continue;
    }
    $dev = (string)$node->if;
    $mac = $details[$dev]['macaddr'] ?? '';
    if (
        !preg_match('/^([0-9a-f]{2}:){5}[0-9a-f]{2}$/i', $mac) || $mac === '00:00:00:00:00:00' ||
        (hexdec(substr($mac, 0, 2)) & 1) || preg_match('/^(lo|enc|pflog|pfsync|tun|wg|ovpn[sc]?)\d*$/', $dev)
    ) {
        continue; // not an ethernet-like device (loopback, tunnel, multicast/zero address, ...)
    }
    if ((string)$node->spoofmac === '') {
        $new = strtolower($mac);
        $parent = $parentNode[$vlanParent[$dev] ?? ''] ?? null;
        if ($virtual && $parent !== null && (string)$parent->spoofmac !== '') {
            $new = strtolower((string)$parent->spoofmac); // a VLAN on an assigned parent shares the parent's MAC
        } elseif ($virtual) {
            do {
                $new = sprintf('02:%s', implode(':', str_split(bin2hex(random_bytes(5)), 2)));
            } while (isset($used[$new]));
            $used[$new] = true;
            if (isset($vlanParent[$dev])) {
                $node->promisc = '1'; // own MAC on a VLAN: the NIC must accept it
            }
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
