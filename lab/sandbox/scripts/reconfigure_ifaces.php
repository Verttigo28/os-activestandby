#!/usr/local/bin/php
<?php
// Sandbox stand-in for reconfigure_ifaces.php: configures one interface natively from HASYNC_CONFIG (no OPNsense framework).
//   iface <key>   MAC (spoofmac), address, up        routes / dns   nothing
// SBXNODE/iface_fail_<key> makes it fail, to test the fallback and the failure paths.
$mode = $argv[1] ?? '';
$dir = getenv('SBXNODE');
file_put_contents("$dir/stub.log", sprintf("%.3f reconfigure_ifaces %s %s\n", microtime(true), $mode, $argv[2] ?? ''), FILE_APPEND);
if ($mode === 'iface') {
    $key = $argv[2] ?? '';
    if (file_exists("$dir/iface_fail_$key")) {
        fwrite(STDERR, "injected failure for $key\n");
        exit(1);
    }
    $x = simplexml_load_file(getenv('HASYNC_CONFIG'));
    $n = $x->interfaces->$key ?? null;
    if ($n === null) {
        fwrite(STDERR, "no such interface $key\n");
        exit(1);
    }
    $dev = (string)$n->if;
    $mac = (string)$n->spoofmac;
    if ($mac !== '') {
        passthru('ifconfig ' . escapeshellarg($dev) . ' link ' . escapeshellarg($mac));
    }
    if ((string)$n->ipaddr !== '') {
        passthru('ifconfig ' . escapeshellarg($dev) . ' inet ' . escapeshellarg($n->ipaddr . '/' . $n->subnet));
    }
    passthru('ifconfig ' . escapeshellarg($dev) . ' up');
}
exit(0);
