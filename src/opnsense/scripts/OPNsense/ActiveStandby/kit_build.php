#!/usr/local/bin/php
<?php

/*
 * Builds the configuration file for the SECOND firewall from this (primary) node's configuration.
 *
 *   kit_build.php <config.xml> <secondary HA ip> <secondary mgmt ip|dhcp|-> <hostname> <token> <expires> <primary HA ip>
 *
 * The result is the "standby view" of the config (data interfaces without <enable>, so the node is silent on the
 * wire) with the second firewall's own hostname and HA/management addresses and the one-time pairing token.
 * It is written to stdout; nothing here touches the running system.
 */

[$self, $cfgPath, $haIp, $mgmtArg, $hostname, $token, $expires, $primaryHa] = array_pad($argv, 8, '');

function fail(string $msg): void
{
    fwrite(STDERR, $msg . "\n");
    exit(1);
}

$tmp = tempnam('/tmp', 'kit');
exec(escapeshellcmd(__DIR__ . '/ifaces.php') . ' stdview ' . escapeshellarg($cfgPath) . ' ' . escapeshellarg($tmp) . ' 2>&1', $o, $rc);
if ($rc !== 0) {
    @unlink($tmp);
    fail('standby view failed: ' . implode(' ', $o));
}
$x = simplexml_load_file($tmp);
@unlink($tmp);
if ($x === false) {
    fail('cannot parse the generated configuration');
}

$g = $x->OPNsense->ActiveStandby->general ?? null;
if ($g === null || (string)$g->ha_interface === '') {
    fail('Active/Standby is not configured on this node (HA link interface missing)');
}
$haKey = (string)$g->ha_interface;
$mgmtKey = (string)$g->mgmt_interface;

// HA link: the secondary's own address, same subnet
$ha = $x->interfaces->$haKey ?? null;
if ($ha === null) {
    fail("HA interface $haKey not found");
}
$mask = (int)$ha->subnet;
$bits = (0xFFFFFFFF << (32 - $mask)) & 0xFFFFFFFF;
if (ip2long($haIp) === false || (ip2long($haIp) & $bits) !== (ip2long((string)$ha->ipaddr) & $bits)) {
    fail("The HA address $haIp is not in the HA subnet " . (string)$ha->ipaddr . "/$mask");
}
$ha->ipaddr = $haIp;

// management interface (optional)
if ($mgmtKey !== '') {
    $m = $x->interfaces->$mgmtKey ?? null;
    if ($m !== null) {
        if ($mgmtArg === 'dhcp') {
            $m->ipaddr = 'dhcp';
            unset($m->subnet);
        } elseif ($mgmtArg !== '-' && $mgmtArg !== '') {
            $m->ipaddr = $mgmtArg;
        } elseif ((string)$m->ipaddr !== 'dhcp') {
            fail('Enter a management address for the second firewall (or dhcp): the management interface of this node has a fixed address.');
        }
    }
}

$x->system->hostname = $hostname;

// settings and one-time token
$g->enabled = '1';
foreach (['pair_token' => $token, 'pair_expires' => $expires] as $k => $v) {
    if (isset($g->$k)) {
        $g->$k = $v;
    } else {
        $g->addChild($k, $v);
    }
}

echo $x->asXML();
