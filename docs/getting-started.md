# Getting started

## Requirements

- Two machines with **identical hardware layout**: the same NIC names (for example `ixl0..ixl3`), the same OPNsense version, the same installed plugins. Pairing compares OPNsense version, kernel, NIC names and plugin versions and refuses to pair when they differ.
- A **dedicated HA link**: one cable between the two machines (a single port is enough), static IPv4 on each side, for example 10.99.0.1/24 and 10.99.0.2/24.
- Optionally a **management interface** on a shared management network. It stays up on the standby, carries a second heartbeat path and lets you reach the standby's GUI.
- Static IPv4 addresses on the interfaces that will be shared. The wizard warns about DHCP/PPPoE, CARP virtual IPs and the built-in High Availability sync (clear those first).

## Install

On both machines:

```sh
fetch https://github.com/Verttigo28/opn-betterHA/releases/download/v<version>/os-activestandby-<version>.pkg
fetch https://github.com/Verttigo28/opn-betterHA/releases/download/v<version>/SHA256SUMS
grep os-activestandby SHA256SUMS      # compare with: sha256 os-activestandby-<version>.pkg
pkg add os-activestandby-<version>.pkg
```

Verify the download: compare the checksum, or better `gh attestation verify os-activestandby-<version>.pkg --repo Verttigo28/opn-betterHA`. With the signed pkg repository set up (see [building](building.md#signing)) `pkg install` checks the signature itself. Reload the GUI: the page is under **System > Active/Standby HA** and the built-in High Availability menu is hidden.

## Set up the pair

### Option A: secondary kit (recommended)

1. On the primary open **Setup wizard**. Step 1: choose the HA link interface (and optionally management) and let the wizard add the firewall rule for the HA link. Step 2: **Primary**.
2. Step 3, **Automatic**: enter hostname, HA address and (if used) management address (`dhcp` or an IPv4) of the second firewall and click **Download configuration**.
3. Prepare the second machine: install OPNsense (same version) and the plugin, cable the HA link, then **System > Configuration > Backups > Restore** the downloaded file and reboot.
4. The primary keeps trying to reach the HA address; the Status page shows *Waiting for the second firewall* and pairs by itself within seconds of the second firewall coming up. The second firewall then receives the complete configuration and turns silent.

The kit is the primary's configuration in its *silent standby form* (data interfaces not enabled), with the second firewall's own hostname and HA/management addresses and a one-time pairing key. It is valid for 48 hours and **contains secrets** (users, keys): delete it afterwards.

### Option B: manual pairing with a token

1. Run the wizard on both nodes. On the secondary choose *Secondary* and **Open pairing window**: it shows its address and a one-time token.
2. On the primary choose *Primary*, enter the secondary's HA address and the token, **Pair**.

In both cases: the **primary** keeps its configuration and is active first; the **secondary** starts as standby and its configuration (except hostname and the HA and management interface) is replaced by the primary's.

## Check

- **Status** shows both nodes, sync state and failover protection.
- **Operations > Pre-flight** verifies both nodes and runs a dry run of the standby taking over, without moving traffic.
- Try **Failover now** in a maintenance window and watch the lost traffic.

## From now on

- Make configuration changes on the **active** node only. A change made on the standby is rejected (its configuration is locked) and would be replaced anyway; the status bar says so on every page.
- Day-to-day use, updates, monitoring and troubleshooting: [operations](operations.md).
