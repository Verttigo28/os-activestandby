#!/usr/local/bin/php
<?php

/*
 * Run by the node that is being promoted, before its data interfaces are configured.
 * Creates the virtual devices of the replicated configuration (LAGG, VLAN, bridge, GRE, GIF) with the same
 * OPNsense functions the boot sequence uses. The configd "configure" actions cannot be used: they only process
 * items the GUI flagged as changed on THIS node, and a standby never saw those changes.
 *
 * The internal functions change between OPNsense releases (e.g. interfaces_vlan_configure() lost its $verbose argument
 * and takes a device name in 26.7.6), so every call is adapted by reflection, isolated by try/catch, and the result is
 * verified: the script exits non-zero and names every device that does not exist afterwards.
 */

require_once 'config.inc';
require_once 'util.inc';
require_once 'interfaces.inc';

global $config;

/** call $fn once per device if it wants a device argument, otherwise once without arguments */
function configure_with(string $fn, array $devices): array
{
    $errors = [];
    if (!function_exists($fn) || count($devices) === 0) {
        return $errors;
    }
    try {
        $params = (new ReflectionFunction($fn))->getParameters();
        if (count($params) > 0 && !$params[0]->isOptional() && $params[0]->getName() === 'device') {
            foreach ($devices as $dev) {
                try {
                    $fn($dev);
                } catch (Throwable $e) {
                    $errors[] = "$fn($dev): " . $e->getMessage();
                }
            }
        } else {
            $fn();
        }
    } catch (Throwable $e) {
        $errors[] = "$fn: " . $e->getMessage();
    }
    return $errors;
}

$sections = [
    // function                      => [section, item, field]
    'interfaces_lagg_configure' => ['laggs', 'lagg', 'laggif'],
    'interfaces_vlan_configure' => ['vlans', 'vlan', 'vlanif'],
    'interfaces_gre_configure' => ['gres', 'gre', 'greif'],
    'interfaces_gif_configure' => ['gifs', 'gif', 'gifif'],
    'interfaces_bridge_configure' => ['bridges', 'bridge', 'bridgeif'],
];

$wanted = [];
$errors = [];
foreach ($sections as $fn => [$sec, $item, $field]) {
    $devices = [];
    foreach ($config[$sec][$item] ?? [] as $entry) {
        if (!empty($entry[$field])) {
            $devices[] = $entry[$field];
        }
    }
    $wanted = array_merge($wanted, $devices);
    $errors = array_merge($errors, configure_with($fn, $devices));
}

$missing = [];
foreach ($wanted as $dev) {
    exec('/sbin/ifconfig ' . escapeshellarg($dev) . ' 2>/dev/null', $o, $rc);
    if ($rc !== 0) {
        $missing[] = $dev;
    }
}

echo 'devices wanted: ' . (count($wanted) ? implode(' ', $wanted) : '(none)') . "\n";
foreach ($errors as $e) {
    echo "error: $e\n";
}
if (count($missing) > 0) {
    echo 'MISSING after creation: ' . implode(' ', $missing) . "\n";
    exit(1);
}
echo "all devices present\n";
