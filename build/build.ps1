# Local build on Windows: tests, cross-compiles hasyncd for FreeBSD, then builds the .pkg on a FreeBSD/OPNsense host
# (default: the lab VM "peer") and copies it to dist/.
#   .\build\build.ps1 [-Version 0.3.0] [-BuildNode peer]
param([string]$Version = '', [string]$BuildNode = 'peer')
$ErrorActionPreference = 'Stop'
$root = Resolve-Path "$PSScriptRoot\.."
$go = 'C:\Program Files\Go\bin\go.exe'
if (-not $Version) { $Version = (Select-String -Path "$root\daemon\daemon.go" -Pattern 'var version = "([0-9.]+)').Matches[0].Groups[1].Value }

Push-Location "$root\daemon"
& $go vet ./... ; if ($LASTEXITCODE) { throw 'go vet failed' }
& $go test ./... ; if ($LASTEXITCODE) { throw 'go test failed' }
$env:GOOS = 'freebsd'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
& $go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$root\src\sbin\hasyncd" . ; if ($LASTEXITCODE) { throw 'go build failed' }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
Pop-Location

. "$root\lab\tools\opn.ps1"
$tar = Join-Path $env:TEMP 'activestandby-build.tar'
Remove-Item $tar -ErrorAction SilentlyContinue
tar -cf $tar -C $root src build/build-pkg.sh
Opn-Put $BuildNode $tar /tmp/activestandby-build.tar | Out-Null
Opn-Sh $BuildNode @"
set -e
rm -rf /tmp/asbuild && mkdir -p /tmp/asbuild && tar -xf /tmp/activestandby-build.tar -C /tmp/asbuild
chmod +x /tmp/asbuild/src/sbin/hasyncd
sh /tmp/asbuild/build/build-pkg.sh /tmp/asbuild $Version /tmp/asbuild/out
"@
New-Item -ItemType Directory -Force "$root\dist" | Out-Null
scp @script:SshOpts "root@$(Get-OpnIp $BuildNode):/tmp/asbuild/out/os-activestandby-$Version.pkg" "$root\dist\" | Out-Null
Get-Item "$root\dist\os-activestandby-$Version.pkg" | Select Name, Length
