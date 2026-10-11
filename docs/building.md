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
.\build\build.ps1 -Version 0.5.0 -BuildNode peer      # lab shortcut; produces dist\os-activestandby-<version>.pkg
```
By hand:
```sh
cd daemon && go test ./... && GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o ../src/sbin/hasyncd .
# copy the repo to a FreeBSD/OPNsense host, then:
sh build/build-pkg.sh /path/to/repo 0.5.0 /path/to/out
```
The package has no dependencies, ABI `FreeBSD:*:amd64`, and its install/removal scripts refresh the GUI caches and restart configd.

## Install / remove (both nodes, identical OPNsense and plugin versions)
```sh
pkg add os-activestandby-<version>.pkg       # then System > Active/Standby HA > Setup wizard
pkg delete os-activestandby              # break the pair first (the removal script warns if the node is still paired)
```

## GitHub
CI runs on every push and pull request: gofmt, vet and tests with the race detector (Linux and a FreeBSD cross-vet), lint of PHP, XML, JavaScript and shell. Pushing a tag `vX.Y.Z` builds `os-activestandby-X.Y.Z.pkg` on a FreeBSD VM and attaches it, with `SHA256SUMS` and generated release notes, to a GitHub release (the daemon version comes from the tag).
The package is unsigned: for production, host a signed repository or distribute the .pkg over a channel you trust.

## Plugins-framework build (optional)
The `Makefile` follows opnsense/plugins (`net/haproxy` layout) so the plugin can later be submitted there. That build needs the OPNsense tools/ports
environment; the prebuilt `src/sbin/hasyncd` must be present and executable.

## Emergency / uninstall
`hasyncd ctl break` on both nodes returns them to standalone. A former standby keeps its data interfaces disabled in `config.xml`: restore a backup
(`/conf/backup/`) or `/conf/activestandby/active.xml` and review IPs before connecting it to the network.

## Signing

Every release is protected in two independent ways.

**1. Build provenance (always on).** CI attaches a signed GitHub attestation to the `.pkg`. Anyone can check that the file was built by this repository's workflow from the tagged commit:
```sh
gh attestation verify os-activestandby-<version>.pkg --repo Verttigo28/opn-betterHA
```
`SHA256SUMS` is published next to the package as well.

**2. A signed pkg repository (once the key exists).** `pkg` signs a repository catalog, not single files. CI builds the repository with your private key and publishes it on the `gh-pages` branch; clients then use `pkg install`/`pkg upgrade` with signature checking.

One-time setup by the maintainer:
1. On Windows run `.\build\gen-signing-key.ps1` in PowerShell (it uses the openssl that ships with Git for Windows); elsewhere `sh build/gen-signing-key.sh`. Either creates `repo.key` (private) and `keys/activestandby.pub` (public) and prints the key fingerprint. On Windows, `Get-Content repo.key -Raw | Set-Clipboard` copies the private key for the secret. Commit `keys/activestandby.pub`, never `repo.key`.
2. GitHub, repository **Settings > Secrets and variables > Actions > New repository secret**: name `PKG_SIGNING_KEY`, value the complete content of `repo.key` including the `-----BEGIN` and `-----END` lines. Store `repo.key` in a password manager or delete it from the workstation.
3. GitHub, **Settings > Pages**: source *Deploy from a branch*, branch `gh-pages`, folder `/ (root)`. The branch appears after the first tagged build with the secret set.
4. Publish the key fingerprint in the README so users can compare it (done: README, section Package signing).

Without the secret the release still works (steps 1 above only); the repository steps are skipped.

Client setup (each firewall):
```sh
fetch -o /usr/local/etc/pkg/keys/activestandby.pub https://raw.githubusercontent.com/Verttigo28/opn-betterHA/main/keys/activestandby.pub
cat > /usr/local/etc/pkg/repos/activestandby.conf <<'CONF'
activestandby: {
  url: "https://verttigo28.github.io/opn-betterHA",
  signature_type: "pubkey",
  pubkey: "/usr/local/etc/pkg/keys/activestandby.pub",
  enabled: yes
}
CONF
pkg update -r activestandby && pkg install -r activestandby os-activestandby
```
(create `/usr/local/etc/pkg/keys` and `/usr/local/etc/pkg/repos` if they do not exist; compare the key's fingerprint with the one in the README before trusting it.)

Rotating the key: generate a new pair, replace the secret and `keys/activestandby.pub`, tell users to replace the public key; the next release re-signs the catalog.
