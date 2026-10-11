#!/bin/sh
# Builds os-activestandby-<version>.pkg with plain pkg(8). Run on FreeBSD/OPNsense (needs `pkg create`).
#   build-pkg.sh <repo-dir> <version> <out-dir>
# <repo-dir> contains src/ with src/sbin/hasyncd already built (build/build.ps1 or the CI job does that).
set -eu

REPO=${1:?repo dir}
VER=${2:?version, e.g. 0.3.0}
OUT=${3:?output dir}
NAME=os-activestandby

[ -x "$REPO/src/sbin/hasyncd" ] || { echo "src/sbin/hasyncd missing or not executable: build the daemon first" >&2; exit 1; }

WORK=$(mktemp -d /tmp/pkgbuild.XXXXXX)
STAGE=$WORK/stage
META=$WORK/meta
mkdir -p "$STAGE/usr/local" "$META" "$OUT"
cp -R "$REPO/src/." "$STAGE/usr/local/"

# ---- permissions ----
find "$STAGE/usr/local" -type d -exec chmod 0755 {} +
find "$STAGE/usr/local" -type f -exec chmod 0644 {} +
chmod 0755 "$STAGE/usr/local/sbin/hasyncd" "$STAGE/usr/local/etc/rc.d/hasyncd" \
    "$STAGE"/usr/local/etc/rc.syshook.d/early/05-activestandby \
    "$STAGE"/usr/local/etc/rc.syshook.d/config/50-activestandby \
    "$STAGE"/usr/local/opnsense/scripts/OPNsense/ActiveStandby/*.php

# ---- plist (paths relative to the prefix /usr/local) ----
(
    cd "$STAGE/usr/local"
    echo "@owner root"
    echo "@group wheel"
    find . -type f | sed 's#^\./##' | sort | while read -r f; do
        if [ -x "$f" ]; then echo "@(root,wheel,0755) $f"; else echo "@(root,wheel,0644) $f"; fi
    done
) > "$WORK/plist"

# ---- scripts ----
cat > "$META/+POST_INSTALL" <<'EOF'
/usr/local/etc/rc.configure_plugins POST_INSTALL >/dev/null 2>&1 || true
rm -f /tmp/opnsense_menu_cache.xml /tmp/opnsense_acl_cache.json
/usr/sbin/service configd restart >/dev/null 2>&1 || true
EOF
cat > "$META/+PRE_DEINSTALL" <<'EOF'
if [ -f /conf/activestandby/state.json ] && grep -q '"role": "\(master\|standby\)"' /conf/activestandby/state.json; then
    echo "WARNING: this node is still paired. Break the pair first (System > Active/Standby HA, or: hasyncd ctl break)." >&2
fi
/usr/local/etc/rc.d/hasyncd onestop >/dev/null 2>&1 || true
EOF
cat > "$META/+POST_DEINSTALL" <<'EOF'
rm -f /tmp/opnsense_menu_cache.xml /tmp/opnsense_acl_cache.json
/usr/sbin/service configd restart >/dev/null 2>&1 || true
EOF

# ---- manifest ----
cat > "$META/+MANIFEST" <<EOF
name: "$NAME"
version: "$VER"
origin: "opnsense/$NAME"
comment: "Active/standby HA for OPNsense: shared IP and MAC, standby interfaces down, full config sync"
maintainer: "baptiste.ferrando@netmanage.ch"
www: "https://github.com/"
abi: "FreeBSD:*:amd64"
prefix: "/usr/local"
licenselogic: "single"
licenses: ["BSD2CLAUSE"]
desc: <<EOD
SonicWall-style active/standby high availability for OPNsense: one IP per interface, identical IP and MAC
on both nodes, the standby keeps all data interfaces down, the active node replicates its whole
configuration (revisions with rollback), automatic failover with split-brain protection, pfsync,
IPsec/dpinger follow the active role. Pair two identical nodes with the setup wizard.
EOD
EOF

pkg create -m "$META" -r "$STAGE" -p "$WORK/plist" -o "$OUT"
rm -rf "$WORK"
ls -l "$OUT/$NAME-$VER.pkg"
