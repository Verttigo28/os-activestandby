#!/usr/local/bin/php
<?php

/*
 * Reconfigures several interfaces in ONE php process (promote speed): the stock "configctl interface reconfigure <if>"
 * starts a new php, loads the config and runs the whole interface code once per interface.
 * Usage: reconfigure_ifaces.php wan lan opt3 ...
 */

require_once("config.inc");
require_once('auth.inc');
require_once("util.inc");
require_once("filter.inc");
require_once("system.inc");
require_once("interfaces.inc");

$rc = 0;
foreach (array_slice($argv, 1) as $key) {
    try {
        interface_configure(true, trim($key), true);
    } catch (\Throwable $e) {
        fwrite(STDERR, "reconfigure {$key}: " . $e->getMessage() . "\n");
        $rc = 1;
    }
}
exit($rc);
