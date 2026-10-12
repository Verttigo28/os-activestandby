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

const NOT_DATA = '/^(lo|enc|pflog|pfsync|tun|tap|wg|ovpn[sc]?|ipsec|gif|gre)\d*$/';

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


/*
 * Warm standby plan: for every enabled data interface, what the daemon needs to bring it up natively (ifconfig) at takeover
 * without running the PHP interface code, or why it cannot ("eligible": false, "reason"). Only static IPv4 (plus static IPv6
 * and IP aliases), a shared MAC and a plain NIC or VLAN qualify; everything else keeps the classic takeover.
 */
function warmPlan(SimpleXMLElement $cfg): array
{
    $vlans = [];
    foreach ($cfg->vlans->vlan ?? [] as $v) {
        $vlans[(string)$v->vlanif] = (string)$v->if;
    }
    $vips = [];
    $badVip = [];
    foreach ($cfg->virtualip->vip ?? [] as $v) {
        $k = (string)$v->interface;
        if ((string)$v->mode === 'ipalias' && filter_var((string)$v->subnet, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) !== false
            && (int)$v->subnet_bits >= 1 && (int)$v->subnet_bits <= 32) {
            $vips[$k][] = (string)$v->subnet . '/' . (int)$v->subnet_bits;
        } else {
            $badVip[$k] = (string)$v->mode;
        }
    }
    $out = [];
    foreach (dataInterfaces($cfg) as $i) {
        $node = $cfg->interfaces->{$i['key']};
        $why = '';
        $v4 = $v6 = '';
        $parent = $vlans[$i['dev']] ?? '';
        if (!$i['enabled']) {
            $why = 'interface is disabled';
        } elseif (!preg_match('/^[a-z][a-z0-9_.]{0,30}$/', $i['dev'])) {
            $why = 'unusable device name';
        } elseif ($parent === '' && !preg_match('/^[a-z]+[0-9]+$/', $i['dev'])) {
            $why = 'device type is not supported by the warm standby';
        } elseif ($parent === '' && preg_match('/^(lagg|bridge|gre|gif|vxlan|qinq|epair|ppp|pppoe|pptp|l2tp|vlan)\d*$/', $i['dev'])) {
            $why = 'device type ' . preg_replace('/\d+$/', '', $i['dev']) . ' is not supported by the warm standby';
        } elseif (!preg_match('/^([0-9a-f]{2}:){5}[0-9a-f]{2}$/i', $i['spoofmac']) || (hexdec(substr($i['spoofmac'], 0, 2)) & 1)) {
            $why = 'no shared MAC address (spoofmac) is set';
        } elseif (filter_var((string)$node->ipaddr, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) === false
            || (int)$node->subnet < 1 || (int)$node->subnet > 32) {
            $why = 'IPv4 address is not static';
        } elseif (isset($badVip[$i['key']])) {
            $why = 'virtual IP of type ' . $badVip[$i['key']] . ' on this interface';
        } else {
            $v4 = (string)$node->ipaddr . '/' . (int)$node->subnet;
            $a6 = (string)$node->ipaddrv6;
            if ($a6 !== '' && $a6 !== 'none') {
                if (filter_var($a6, FILTER_VALIDATE_IP, FILTER_FLAG_IPV6) !== false && (int)$node->subnetv6 >= 1 && (int)$node->subnetv6 <= 128) {
                    $v6 = $a6 . '/' . (int)$node->subnetv6;
                } else {
                    $why = 'IPv6 address is not static';
                }
            }
        }
        $out[$i['key']] = ['key' => $i['key'], 'dev' => $i['dev'], 'mac' => strtolower($i['spoofmac']), 'mtu' => (int)$node->mtu,
            'v4' => $v4, 'v6' => $v6, 'vips' => $vips[$i['key']] ?? [], 'parent' => $parent, 'eligible' => $why === '', 'reason' => $why];
    }
    // a VLAN is only as warm as its parent: an assigned parent must be eligible itself
    $byDev = [];
    foreach ($out as $e) {
        $byDev[$e['dev']] = $e['key'];
    }
    foreach ($out as $k => $e) {
        if ($e['eligible'] && $e['parent'] !== '' && isset($byDev[$e['parent']]) && !$out[$byDev[$e['parent']]]['eligible']) {
            $out[$k]['eligible'] = false;
            $out[$k]['reason'] = 'its parent ' . $e['parent'] . ' cannot be warm: ' . $out[$byDev[$e['parent']]]['reason'];
        }
    }
    return ['ifaces' => array_values($out)];
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
    // never force the HA/mgmt devices down, nor the NIC (parent) or LAGG members they are built on: an HA or management
    // VLAN that shares its trunk with data VLANs would otherwise be cut together with them
    foreach ($cfg->vlans->vlan ?? [] as $v) {
        if (in_array((string)$v->vlanif, $skipDevs, true)) {
            $skipDevs[] = (string)$v->if;
        }
    }
    foreach ($cfg->laggs->lagg ?? [] as $l) {
        if (in_array((string)$l->laggif, $skipDevs, true)) {
            foreach (explode(',', (string)$l->members) as $m) {
                $skipDevs[] = trim($m);
            }
        }
    }
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
    $g = $cfg->OPNsense->ActiveStandby->general ?? null;
    echo json_encode(['data' => $data, 'force_down' => $down, 'keep_up' => array_values(array_unique($skipDevs)), 'device_types' => $types,
        'ha_key' => $g === null ? '' : (string)$g->ha_interface, 'mgmt_key' => $g === null ? '' : (string)$g->mgmt_interface]), "\n";
} elseif ($cmd === 'plan') {
    echo json_encode(warmPlan(load($argv[2] ?? '/conf/config.xml'))), "\n";
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
    fwrite(STDERR, "usage: ifaces.php list [file] | plan [file] | stdview <in> <out>\n");
    exit(2);
}
