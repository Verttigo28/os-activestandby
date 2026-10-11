#!/usr/local/bin/php
<?php

/*
 * Pure-XML helper (no OPNsense framework, so it is safe and fast to run at early boot).
 *
 *   ifaces.php list    [config.xml]            JSON: data interfaces and every device that must be down on standby
 *   ifaces.php stdview <in.xml> <out.xml>      write the "standby view": <enable> removed from all data interfaces
 *
 * "Data interface" = every assigned interface except the HA link and the management interface
 * (both read from OPNsense/ActiveStandby/general in the same file) and except virtual/loopback devices.
 */

const NOT_DATA = '/^(lo|enc|pflog|pfsync|tun|tap|wg|ovpn|ipsec|gif|gre)\d*$/';

function load(string $path): SimpleXMLElement
{
    $x = @simplexml_load_file($path);
    if ($x === false) {
        fwrite(STDERR, "cannot parse $path\n");
        exit(1);
    }
    return $x;
}

function skipKeys(SimpleXMLElement $cfg): array
{
    $g = $cfg->OPNsense->ActiveStandby->general ?? null;
    return $g === null ? [] : array_values(array_filter([(string)$g->ha_interface, (string)$g->mgmt_interface]));
}

function virtualDevices(SimpleXMLElement $cfg): array
{
    $v = [];
    foreach (['vlans' => ['vlan', 'vlanif'], 'laggs' => ['lagg', 'laggif'], 'bridges' => ['bridge', 'bridgeif'],
        'gres' => ['gre', 'greif'], 'gifs' => ['gif', 'gifif']] as $sec => [$item, $field]) {
        foreach ($cfg->$sec->$item ?? [] as $n) {
            $v[] = (string)$n->$field;
        }
    }
    return $v;
}

function dataInterfaces(SimpleXMLElement $cfg): array
{
    $skip = skipKeys($cfg);
    $virtual = virtualDevices($cfg);
    $out = [];
    foreach ($cfg->interfaces->children() as $key => $node) {
        $dev = (string)$node->if;
        if ($dev === '' || in_array((string)$key, $skip, true) || preg_match(NOT_DATA, $dev)) {
            continue;
        }
        $out[] = [
            'key' => (string)$key,
            'dev' => $dev,
            'enabled' => (string)$node->enable !== '',
            'spoofmac' => (string)$node->spoofmac,
            'virtual' => in_array($dev, $virtual, true),
        ];
    }
    return $out;
}

$cmd = $argv[1] ?? '';
if ($cmd === 'list') {
    $cfg = load($argv[2] ?? '/conf/config.xml');
    $data = dataInterfaces($cfg);
    $down = array_column($data, 'dev');
    // parents of assigned VLANs and members of assigned LAGGs must stay down as well
    foreach ($cfg->vlans->vlan ?? [] as $v) {
        if (in_array((string)$v->vlanif, $down, true)) {
            $down[] = (string)$v->if;
        }
    }
    foreach ($cfg->laggs->lagg ?? [] as $l) {
        if (in_array((string)$l->laggif, $down, true)) {
            foreach (explode(',', (string)$l->members) as $m) {
                $down[] = trim($m);
            }
        }
    }
    $skipDevs = [];
    foreach ($cfg->interfaces->children() as $key => $node) {
        if (in_array((string)$key, skipKeys($cfg), true)) {
            $skipDevs[] = (string)$node->if;
        }
    }
    // never force the HA/mgmt devices down, even if they are a VLAN parent of a data interface
    $down = array_values(array_diff(array_unique(array_filter($down)), $skipDevs));
    // virtual device types present in the config; promote must (re)create them before bringing interfaces up
    $types = [];
    foreach (
        ['lagg' => ['laggs', 'lagg'], 'vlan' => ['vlans', 'vlan'], 'vxlan' => ['vxlans', 'vxlan'],
        'gre' => ['gres', 'gre'], 'gif' => ['gifs', 'gif'], 'bridge' => ['bridges', 'bridge']] as $t => [$sec, $item]
    ) {
        if (isset($cfg->$sec) && count($cfg->$sec->$item) > 0) {
            $types[] = $t;
        }
    }
    echo json_encode(['data' => $data, 'force_down' => $down, 'keep_up' => $skipDevs, 'device_types' => $types]), "\n";
} elseif ($cmd === 'stdview') {
    $cfg = load($argv[2] ?? '');
    $skip = skipKeys($cfg);
    foreach ($cfg->interfaces->children() as $key => $node) {
        $dev = (string)$node->if;
        if (in_array((string)$key, $skip, true) || preg_match(NOT_DATA, $dev) || $dev === '') {
            continue;
        }
        unset($node->enable);
    }
    $dom = dom_import_simplexml($cfg)->ownerDocument;
    $dom->formatOutput = true;
    if ($dom->save($argv[3] ?? '') === false) {
        fwrite(STDERR, "cannot write output\n");
        exit(1);
    }
} else {
    fwrite(STDERR, "usage: ifaces.php list [file] | stdview <in> <out>\n");
    exit(2);
}
