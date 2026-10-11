#!/usr/local/bin/php
<?php

/*
 * Run by the node that is being promoted, before its data interfaces are configured.
 * Creates the virtual devices of the replicated configuration (LAGG, VLAN, bridge, GRE, GIF) with the same
 * OPNsense functions the boot sequence uses. The configd "configure" actions cannot be used: they only process
 * items the GUI flagged as changed on THIS node, and a standby never saw those changes.
 */

require_once 'config.inc';
require_once 'util.inc';
require_once 'interfaces.inc';

global $config;

$done = [];
interfaces_lagg_configure();
$done[] = 'lagg';
interfaces_vlan_configure();
$done[] = 'vlan';
foreach ($config['gres']['gre'] ?? [] as $gre) {
    interfaces_gre_configure($gre['greif']);
    $done[] = $gre['greif'];
}
foreach ($config['gifs']['gif'] ?? [] as $gif) {
    interfaces_gif_configure($gif['gifif']);
    $done[] = $gif['gifif'];
}
foreach ($config['bridges']['bridge'] ?? [] as $bridge) {
    interfaces_bridge_configure($bridge['bridgeif']);
    $done[] = $bridge['bridgeif'];
}
echo 'created ' . implode(' ', $done) . "\n";
