#!/usr/local/bin/php
<?php

/*
 * merge_config.php <active-config.xml> <local-config.xml> <out.xml>
 *
 * Standby side of config sync: take the active node's full config and put the node-local parts of
 * THIS node back in. Node-local (never replicated):
 *   - system/hostname
 *   - interfaces/<HA link key> and interfaces/<management key> (each node has its own IP there)
 *   - OPNsense/ActiveStandby (this plugin's own settings)
 * The keys of the HA/management interfaces are read from the LOCAL file. Pure DOM, no framework.
 */

function fail(string $m): void
{
    fwrite(STDERR, "$m\n");
    exit(1);
}

if ($argc < 4) {
    fail('usage: merge_config.php <active.xml> <local.xml> <out.xml>');
}

$load = function (string $f): DOMDocument {
    $d = new DOMDocument();
    $d->preserveWhiteSpace = false;
    if (!@$d->load($f) || $d->documentElement === null || $d->documentElement->nodeName !== 'opnsense') {
        fail("$f is not a valid OPNsense config");
    }
    return $d;
};
$m = $load($argv[1]);
$l = $load($argv[2]);
$mx = new DOMXPath($m);
$lx = new DOMXPath($l);

if ($mx->query('/opnsense/interfaces')->length === 0 || $mx->query('/opnsense/system')->length === 0) {
    fail('active config has no interfaces/system section, refusing');
}

$paths = ['/opnsense/system/hostname', '/opnsense/OPNsense/ActiveStandby'];
foreach (['ha_interface', 'mgmt_interface'] as $f) {
    $k = trim($lx->evaluate("string(/opnsense/OPNsense/ActiveStandby/general/$f)"));
    if ($k !== '' && preg_match('/^[a-z0-9_]+$/i', $k)) {
        $paths[] = "/opnsense/interfaces/$k";
    }
}

foreach ($paths as $p) {
    $ln = $lx->query($p)->item(0);
    $mn = $mx->query($p)->item(0);
    if ($ln === null) {
        $mn?->parentNode->removeChild($mn);
        continue;
    }
    $imp = $m->importNode($ln, true);
    if ($mn !== null) {
        $mn->parentNode->replaceChild($imp, $mn);
        continue;
    }
    // create missing parents
    $parent = $m->documentElement;
    $segs = explode('/', trim(dirname($p), '/'));
    array_shift($segs); // "opnsense"
    foreach ($segs as $s) {
        $next = null;
        foreach ($parent->childNodes as $c) {
            if ($c->nodeName === $s) {
                $next = $c;
                break;
            }
        }
        if ($next === null) {
            $next = $parent->appendChild($m->createElement($s));
        }
        $parent = $next;
    }
    $parent->appendChild($imp);
}

$m->formatOutput = true;
if ($m->save($argv[3]) === false) {
    fail('cannot write output');
}
