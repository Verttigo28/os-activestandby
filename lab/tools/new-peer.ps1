# Creates and installs a third OPNsense VM ("opn-peer") used as the remote end of an IPsec tunnel.
# NICs: hn0 LAN-side test net (ha-lan), hn1 WAN (ha-wan), hn2 MGMT (Default Switch, SSH). Lab-only credentials.
param([string]$Name = 'opn-peer')
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\hv.ps1"
$lab = Resolve-Path "$PSScriptRoot\.."
$iso = "$lab\OPNsense-26.7-dvd-amd64.iso"
$p = "$lab\vms\$Name"; New-Item -ItemType Directory -Force $p | Out-Null

if (-not (Get-VM $Name -ErrorAction SilentlyContinue)) {
    New-VM -Name $Name -Generation 2 -MemoryStartupBytes 4GB -NewVHDPath "$p\$Name.vhdx" -NewVHDSizeBytes 20GB -SwitchName 'ha-lan' -Path $p | Out-Null
    Set-VM -Name $Name -ProcessorCount 4 -AutomaticCheckpointsEnabled $false -StaticMemory
    Set-VMFirmware -VMName $Name -EnableSecureBoot Off
    Rename-VMNetworkAdapter -VMName $Name -Name (Get-VMNetworkAdapter -VMName $Name).Name -NewName lan
    Add-VMNetworkAdapter -VMName $Name -Name wan -SwitchName 'ha-wan'
    Add-VMNetworkAdapter -VMName $Name -Name mgmt -SwitchName 'Default Switch'
    $i = 1; foreach ($a in 'lan', 'wan', 'mgmt') { Set-VMNetworkAdapter -VMName $Name -Name $a -StaticMacAddress ('00155D0C0C{0:X2}' -f $i) -MacAddressSpoofing On; $i++ }
    Add-VMDvdDrive -VMName $Name -Path $iso
    Set-VMFirmware -VMName $Name -FirstBootDevice (Get-VMDvdDrive -VMName $Name)
}
Start-VM $Name
Start-Sleep 75
Send-VMText $Name "installer`n"; Start-Sleep 2; Send-VMText $Name "opnsense`n"; Start-Sleep 14
Send-VMKey $Name 13;  Start-Sleep 4            # default keymap
Send-VMKey $Name 40;  Start-Sleep 1; Send-VMKey $Name 13; Start-Sleep 4   # Install (UFS)
Send-VMKey $Name 40;  Start-Sleep 1; Send-VMKey $Name 13; Start-Sleep 4   # disk da0 (second entry)
Send-VMKey $Name 13                                                       # confirm erase
Start-Sleep 200                                                           # install
Send-VMKey $Name 13; Start-Sleep 3                                        # Root Password
$labPw = if ($env:OPN_LAB_PW) { $env:OPN_LAB_PW } else { (Get-Content (Join-Path (Split-Path $PSScriptRoot -Parent) '.lab-password') -Raw).Trim() }
Send-VMText $Name "$labPw`n"; Start-Sleep 2; Send-VMText $Name "$labPw`n"; Start-Sleep 6
Send-VMKey $Name 40; Start-Sleep 1; Send-VMKey $Name 13; Start-Sleep 6    # Complete Install
Send-VMKey $Name 13                                                       # Reboot now
Start-Sleep 20
Stop-VM $Name -TurnOff -Force
Get-VMDvdDrive -VMName $Name | Remove-VMDvdDrive
Set-VMFirmware -VMName $Name -FirstBootDevice (Get-VMHardDiskDrive -VMName $Name)
Start-VM $Name
Write-Host "installed; booting from disk"
