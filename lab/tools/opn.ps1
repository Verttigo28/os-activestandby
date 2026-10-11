# Lab SSH helpers. Usage: . .\opn.ps1 ; Opn a "ifconfig -a" ; Opn-Put a local.sh /tmp/x.sh
# Lab-only root password; never used outside the Hyper-V lab (see lab/README.md).
$pwFile = Join-Path (Split-Path $PSScriptRoot -Parent) '.lab-password'
$script:LabPw = if ($env:OPN_LAB_PW) { $env:OPN_LAB_PW } elseif (Test-Path $pwFile) { (Get-Content $pwFile -Raw).Trim() } else { throw 'set OPN_LAB_PW or create lab/.lab-password (the root password of the lab VMs)' }
$script:Dir = Join-Path $env:LOCALAPPDATA 'Temp\claude\lab-askpass'
New-Item -ItemType Directory -Force $script:Dir | Out-Null
Set-Content (Join-Path $script:Dir 'askpass.cmd') "@echo $script:LabPw" -Encoding ascii
$env:SSH_ASKPASS = Join-Path $script:Dir 'askpass.cmd'; $env:SSH_ASKPASS_REQUIRE = 'force'; $env:DISPLAY = 'x'

function Get-OpnIp([string]$n) { ((Get-VMNetworkAdapter -VMName "opn-$n" -Name mgmt).IPAddresses | ? { $_ -match '^\d+\.' } | Select -First 1) }
$script:SshOpts = '-o','StrictHostKeyChecking=no','-o','UserKnownHostsFile=NUL','-o','LogLevel=ERROR','-o','ConnectTimeout=10'
function Opn([string]$n, [string]$cmd) { ssh @script:SshOpts root@$(Get-OpnIp $n) $cmd }
function Opn-Put([string]$n, [string]$local, [string]$remote) { scp @script:SshOpts $local "root@$(Get-OpnIp $n):$remote" }

# Run a POSIX sh script (here-string) on a node: Opn-Sh a @' ...script... '@
function Opn-Sh([string]$n, [string]$body) {
    $f = Join-Path $script:Dir "run_$n.sh"
    [IO.File]::WriteAllText($f, ($body -replace "`r`n","`n"))
    Opn-Put $n $f /tmp/run.sh | Out-Null
    ssh @script:SshOpts root@$(Get-OpnIp $n) 'sh /tmp/run.sh'
}

# REST helper: Api a GET /api/activestandby/pairing/status ; Api a POST /api/... 'k=v&k2=v2'
function Api([string]$n, [string]$method, [string]$path, [string]$body = '') {
    $k = (Get-Content (Join-Path $script:Dir 'apikeys.json') | ConvertFrom-Json).$n
    # the credentials go to curl through a netrc file in the user-only lab folder, not on the command line where every
    # local user could read them
    $ip = Get-OpnIp $n
    $u, $p = $k -split ':', 2
    $netrc = Join-Path $script:Dir 'netrc'
    Set-Content $netrc "machine $ip login $u password $p" -Encoding ascii
    $a = @('-sk', '--netrc-file', $netrc, '-X', $method, "https://$ip$path")
    if ($method -eq 'POST') { if (-not $body) { $body = '_=1' }; $a += @('-d', $body) }
    curl.exe @a
}

