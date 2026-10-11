# Building and installing os-activestandby

## Layout
```
Makefile, pkg-descr            plugin-framework metadata (kept for a future submission to opnsense/plugins)
src/                           installed under /usr/local (src/sbin/hasyncd is built, not committed)
daemon/                        Go sources of hasyncd (standard library only)
build/build-pkg.sh             builds the .pkg with plain pkg(8), runs on FreeBSD/OPNsense
build/build.ps1                Windows driver: test, cross-compile, build the .pkg on a lab node, copy to dist/
.github/workflows/ci.yml       CI: gofmt/vet/test, FreeBSD cross-build, .pkg on version tags (FreeBSD VM) + release upload
lab/                           Hyper-V lab tooling and notes (not part of the package)
```

## Build the package
Anywhere with Go >= 1.22 and a FreeBSD/OPNsense host to run `pkg create` (any OPNsense node works, e.g. a spare VM):
```powershell
.\build\build.ps1 -Version 0.3.0 -BuildNode peer      # lab shortcut; produces dist\os-activestandby-0.3.0.pkg
```
By hand:
```sh
cd daemon && go test ./... && GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o ../src/sbin/hasyncd .
# copy the repo to a FreeBSD/OPNsense host, then:
sh build/build-pkg.sh /path/to/repo 0.3.0 /path/to/out
```
The package has no dependencies, ABI `FreeBSD:*:amd64`, and its install/removal scripts refresh the GUI caches and restart configd.

## Install / remove (both nodes, identical OPNsense and plugin versions)
```sh
pkg add os-activestandby-0.3.0.pkg       # then System > Active/Standby HA > Setup wizard
pkg delete os-activestandby              # break the pair first (the removal script warns if the node is still paired)
```

## GitHub
CI runs on every push and pull request; pushing a tag `vX.Y.Z` builds `os-activestandby-X.Y.Z.pkg` on a FreeBSD VM and attaches it to a GitHub release.
The package is unsigned: for production, host a signed repository or distribute the .pkg over a channel you trust.

## Plugins-framework build (optional)
The `Makefile` follows opnsense/plugins (`net/haproxy` layout) so the plugin can later be submitted there. That build needs the OPNsense tools/ports
environment; the prebuilt `src/sbin/hasyncd` must be present and executable.

## Emergency / uninstall
`hasyncd ctl break` on both nodes returns them to standalone. A former standby keeps its data interfaces disabled in `config.xml`: restore a backup
(`/conf/backup/`) or `/conf/activestandby/active.xml` and review IPs before connecting it to the network.
