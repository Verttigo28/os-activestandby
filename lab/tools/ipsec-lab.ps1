# Creates a site-to-site IKEv2/PSK tunnel through the OPNsense API on a node.
#   . .\opn.ps1 ; . .\ipsec-lab.ps1
#   Add-LabTunnel -Psk <shared-secret> -Node b -Local 10.0.0.1 -Remote 10.0.0.99 -LocalTs 192.168.1.0/24 -RemoteTs 192.168.50.0/24 -Start start
#   Add-LabTunnel -Psk <shared-secret> -Node peer -Local 10.0.0.99 -Remote 10.0.0.1 -LocalTs 192.168.50.0/24 -RemoteTs 192.168.1.0/24 -Start none
function Add-LabTunnel {
    param([string]$Node, [string]$Local, [string]$Remote, [string]$LocalTs, [string]$RemoteTs, [string]$Start = 'start', [Parameter(Mandatory)][string]$Psk)
    $j = { param($s) $s | ConvertFrom-Json }
    $c = & $j (Api $Node POST /api/ipsec/connections/addConnection ("connection[enabled]=1&connection[version]=2&connection[local_addrs]=$Local&connection[remote_addrs]=$Remote" +
        "&connection[proposals]=aes256-sha256-modp2048&connection[mobike]=0&connection[dpd_delay]=2&connection[dpd_timeout]=6&connection[description]=lab-s2s"))
    if ($c.result -ne 'saved') { throw "connection: $($c | ConvertTo-Json -Compress)" }
    $u = $c.uuid
    $l = & $j (Api $Node POST /api/ipsec/connections/addLocal "local[connection]=$u&local[enabled]=1&local[round]=0&local[auth]=psk&local[id]=$Local")
    $r = & $j (Api $Node POST /api/ipsec/connections/addRemote "remote[connection]=$u&remote[enabled]=1&remote[round]=0&remote[auth]=psk&remote[id]=$Remote")
    $ch = & $j (Api $Node POST /api/ipsec/connections/addChild "child[connection]=$u&child[enabled]=1&child[mode]=tunnel&child[start_action]=$Start&child[esp_proposals]=aes256-sha256-modp2048&child[local_ts]=$LocalTs&child[remote_ts]=$RemoteTs&child[rekey_time]=3600")
    $k = & $j (Api $Node POST /api/ipsec/pre_shared_keys/addItem "preSharedKey[ident]=$Local&preSharedKey[remote_ident]=$Remote&preSharedKey[keyType]=PSK&preSharedKey[Key]=$Psk")
    foreach ($x in @($l, $r, $ch, $k)) { if ($x.result -ne 'saved') { throw "part failed: $($x | ConvertTo-Json -Compress)" } }
    "connection $u created on $Node"
}

