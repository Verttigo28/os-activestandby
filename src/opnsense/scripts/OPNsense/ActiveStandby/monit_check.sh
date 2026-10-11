#!/bin/sh

# For Monit (Services > Monit): a "custom" service with this path and the test "status != 0" alerts by e-mail
# (using Monit's own SMTP settings) when the HA pair is degraded. Exit status: 0 ok, 1 warning, 2 critical.
/usr/local/sbin/hasyncd ctl health >/dev/null
exit $?
