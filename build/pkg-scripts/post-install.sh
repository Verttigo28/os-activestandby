#!/bin/sh
# +POST_INSTALL of os-activestandby (copied into the package by build-pkg.sh; checked by ShellCheck in the CI)
/usr/local/etc/rc.configure_plugins POST_INSTALL >/dev/null 2>&1 || true
rm -f /tmp/opnsense_menu_cache.xml /tmp/opnsense_acl_cache.json
/usr/sbin/service configd restart >/dev/null 2>&1 || true

# An upgrade: pkg does not run the deinstall scripts of the old version, so the daemon that runs is still the old binary. It is
# restarted, inside a maintenance window: the peer must not take the second of silence for a failure (with a short detection time it
# would take over while this node still serves).
if /usr/local/etc/rc.d/hasyncd onestatus >/dev/null 2>&1; then
    # this boot was handled by the daemon that runs: the new one must not take its first start for the start of a boot
    sysctl -n kern.boottime 2>/dev/null | sed -n 's/.*sec = \([0-9]*\),.*/\1/p' > /var/run/activestandby.boot
    status=$(/usr/local/sbin/hasyncd ctl status 2>/dev/null)
    opened=""
    case "$status" in
    *'"maintenance":false'*)
        # a window of our own; a maintenance mode that is already on (the operator's, a guided upgrade) is left exactly as it is
        /usr/local/sbin/hasyncd ctl maintenance on --hours 1 >/dev/null 2>&1 &&
            opened=$(/usr/local/sbin/hasyncd ctl status 2>/dev/null | grep -o '"maintenance_until":"[^"]*"' | head -n 1)
        ;;
    esac
    /usr/local/etc/rc.d/hasyncd onerestart >/dev/null 2>&1 || true
    if [ -n "$opened" ]; then
        # detached into its own session by daemon(8): a pkg whose session ends, or a daemon that kills the children of its install
        # (the plugin synchronisation), must not take the closing of the window with it (seen on the lab: maintenance left on)
        # shellcheck disable=SC2016 # the script is meant to be expanded by the detached shell, not by this one
        OPENED="$opened" /usr/sbin/daemon -f /bin/sh -c '
            i=0
            while [ "$i" -lt 60 ]; do
                sleep 2
                i=$((i + 2))
                if /usr/local/sbin/hasyncd ctl status 2>/dev/null | grep -q "\"daemon\":\"up\""; then
                    sleep 10 # the restarted daemon hears its peer again before automatic failover resumes
                    # only the window this script opened: if someone changed the maintenance mode meanwhile, it is theirs
                    if /usr/local/sbin/hasyncd ctl status 2>/dev/null | grep -q "$OPENED"; then
                        /usr/local/sbin/hasyncd ctl maintenance off >/dev/null 2>&1
                    fi
                    break
                fi
            done
        '
    fi
fi
