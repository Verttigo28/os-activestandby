# Build hasyncd for FreeBSD and install the plugin tree on lab nodes.
# Usage: .\deploy.ps1 [-Nodes a,b] [-SkipBuild]
param([string[]]$Nodes = @('a','b'), [switch]$SkipBuild)
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\opn.ps1"
$root = Resolve-Path "$PSScriptRoot\..\.."
$go = "C:\Program Files\Go\bin\go.exe"

if (-not $SkipBuild) {
    Push-Location "$root\daemon"
    $env:GOOS = 'freebsd'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & $go build -trimpath -ldflags '-s -w' -o "$root\src\sbin\hasyncd" .
    if ($LASTEXITCODE) { throw 'go build failed' }
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
    Pop-Location
}

$tar = Join-Path $env:TEMP 'activestandby.tar'
Remove-Item $tar -ErrorAction SilentlyContinue
tar -cf $tar -C "$root\src" .
foreach ($n in $Nodes) {
    Opn-Put $n $tar /tmp/activestandby.tar | Out-Null
    Opn-Sh $n @'
set -e
umask 022
tar -xf /tmp/activestandby.tar -C /usr/local --no-same-owner --no-same-permissions
find /usr/local/opnsense /usr/local/etc -path "*ctiveStandby*" -exec chmod go-w {} +; chmod go-w /usr/local/opnsense/scripts/OPNsense /usr/local/opnsense/mvc/app/models/OPNsense /usr/local/opnsense/mvc/app/controllers/OPNsense /usr/local/opnsense/mvc/app/views/OPNsense
chmod 0755 /usr/local/etc/rc.syshook.d/early/05-activestandby /usr/local/etc/rc.syshook.d/config/50-activestandby /usr/local/sbin/hasyncd /usr/local/etc/rc.d/hasyncd /usr/local/opnsense/scripts/OPNsense/ActiveStandby/*.php /usr/local/opnsense/scripts/OPNsense/ActiveStandby/*.sh
chmod 0644 /usr/local/opnsense/service/conf/actions.d/actions_activestandby.conf
/usr/local/etc/rc.configure_plugins >/dev/null 2>&1 || true
service configd restart >/dev/null 2>&1
rm -f /tmp/opnsense_menu_cache.xml /tmp/opnsense_acl_cache.json
configctl template reload OPNsense/ActiveStandby
echo "deployed: $(/usr/local/sbin/hasyncd version)"
'@
}

