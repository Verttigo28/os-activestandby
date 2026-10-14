#!/usr/local/bin/php
<?php

/*
 * merge_config.php <active-config.xml> <local-config.xml> <out.xml>
 *
 * Standby side of config sync: take the active node's full config and put the node-local parts of
 * THIS node back in. Node-local (never replicated):
 *   - system/hostname
 *   - interfaces/<HA link key> and interfaces/<management key> (each node has its own IP there)
 *   - from OPNsense/ActiveStandby only: enabled, ha_interface, mgmt_interface, listen_port (everything else is shared)
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

$paths = ['/opnsense/system/hostname'];
// The HA and management interfaces are node-local: those of THIS node are kept as they are here, and those of the ACTIVE node are
// never imported, even when the two nodes use different interface keys. Otherwise the active's HA (or management) interface would
// arrive here as a data interface and its address would come up on this node at a takeover, next to the peer that still has it.
foreach ([$lx, $mx] as $x) {
    foreach (['ha_interface', 'mgmt_interface'] as $f) {
        $k = trim($x->evaluate("string(/opnsense/OPNsense/ActiveStandby/general/$f)"));
        if ($k !== '' && preg_match('/^[a-z0-9_]+$/i', $k) && !in_array("/opnsense/interfaces/$k", $paths, true)) {
            $paths[] = "/opnsense/interfaces/$k";
        }
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

// This plugin's settings are shared: the active node's values (timing, health, probes, notifications, ...) replace
// ours, so they are changed in one place. Only what is specific to THIS node stays local: whether the service is
// enabled, which interfaces are the HA link and management, and the peer API port.
$ml = $mx->query('/opnsense/OPNsense/ActiveStandby/general')->item(0);
$ll = $lx->query('/opnsense/OPNsense/ActiveStandby/general')->item(0);
if ($ll !== null) {
    if ($ml === null) {
        // the active node has no settings of its own yet: keep ours as they are
        $opn = $mx->query('/opnsense/OPNsense')->item(0);
        if ($opn === null) {
            $opn = $m->documentElement->appendChild($m->createElement('OPNsense'));
        }
        $as = $mx->query('/opnsense/OPNsense/ActiveStandby')->item(0);
        if ($as === null) {
            $as = $opn->appendChild($m->createElement('ActiveStandby'));
        }
        $as->appendChild($m->importNode($ll, true));
    } else {
        foreach (['enabled', 'ha_interface', 'mgmt_interface', 'listen_port'] as $f) {
            $mine = null;
            foreach ($ll->childNodes as $c) {
                if ($c->nodeName === $f) {
                    $mine = $c;
                }
            }
            $theirs = null;
            foreach ($ml->childNodes as $c) {
                if ($c->nodeName === $f) {
                    $theirs = $c;
                }
            }
            if ($mine === null) {
                if ($theirs !== null) {
                    $ml->removeChild($theirs);
                }
            } elseif ($theirs === null) {
                $ml->appendChild($m->importNode($mine, true));
            } else {
                $ml->replaceChild($m->importNode($mine, true), $theirs);
            }
        }
    }
}

// the one-time pairing key from a secondary kit is spent once the node is paired: do not keep it
foreach (['pair_token', 'pair_expires'] as $t) {
    foreach ($mx->query("/opnsense/OPNsense/ActiveStandby/general/$t") as $n) {
        $n->parentNode->removeChild($n);
    }
}

$m->formatOutput = true;
if ($m->save($argv[3]) === false) {
    fail('cannot write output');
}
