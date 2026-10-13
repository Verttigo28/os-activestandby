# Lab (Hyper-V) — opn-a / opn-b

OPNsense 26.7, 4 vCPU / 4 GB. Switches: `ha-wan`, `ha-lan`, `ha-link` (private), `Default Switch` (MGMT/SSH).
NICs per VM in order: hn0 WAN, hn1 LAN, hn2 HA (opn-a 10.99.0.1, opn-b 10.99.0.2), hn3 MGMT (DHCP). MAC spoofing on.
Lab-only root password and API keys live in `%LOCALAPPDATA%\Temp\claude\lab-askpass` (not in the repo).

```powershell
cd lab\tools
.\deploy.ps1                       # cross-compile hasyncd (FreeBSD) + install plugin on a and b
. .\opn.ps1                        # helpers: Opn, Opn-Sh, Opn-Put, Api
Api a GET /api/activestandby/pairing/status
. .\hv.ps1; Get-VMShot opn-a x.png # console screenshot; Send-VMText opn-a "root`n"
```
Go unit tests (pairing protocol, no VM needed): `cd daemon; go test ./...`

## Phase A manual test (what was run)
1. Enable on both: `Api n POST /api/activestandby/settings/set 'activestandby[general][enabled]=1&activestandby[general][ha_interface]=opt1&activestandby[general][mgmt_interface]=opt2'` then `.../service/reconfigure`.
2. Standby: `Api b POST /api/activestandby/pairing/prepare` -> token. Master: `Api a POST /api/activestandby/pairing/pair 'peer=10.99.0.2&token=<token>'`.
3. Wrong token is rejected; `status` on both shows master/standby and `peer_reachable: true`.
4. Mismatch: `Add-VMNetworkAdapter -VMName opn-b -Name extra -SwitchName ha-lan` then pair again -> refused with the NIC diff. Remove it with `Remove-VMNetworkAdapter`.
5. `Api a POST /api/activestandby/pairing/break` resets both nodes.

## Third VM and IPsec test
`lab/tools/new-peer.ps1` installs `opn-peer` (hn0 LAN 192.168.50.1 on ha-lan, hn1 WAN 10.0.0.99 on ha-wan, hn2 MGMT). After basic setup over the console,
`. .\ipsec-lab.ps1; Add-LabTunnel ...` creates the tunnel on the active node (replicates to the standby) and on the peer; see docs/operations.md for results.

The root password of the lab VMs is read from `lab/.lab-password` (untracked) or the `OPN_LAB_PW` environment variable.

## Sandbox on one FreeBSD machine
`lab/sandbox` runs two real daemons against each other on a single FreeBSD/OPNsense machine (vnet jails, epair interfaces, bridges as switches), with a double-active detector; see its [README](sandbox/README.md). `lab/warm-probe.sh` and `lab/hw-probe.sh` measure what a NIC does when its port goes down or its MAC changes.
