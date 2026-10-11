# Creates the key pair that signs the pkg repository (Windows). Run from the repository root:
#   .\build\gen-signing-key.ps1
# The PRIVATE key (repo.key) never goes into git.
$ErrorActionPreference = 'Stop'
$ssl = (Get-Command openssl -ErrorAction SilentlyContinue).Source
if (-not $ssl) {
    foreach ($p in 'C:\Program Files\Git\usr\bin\openssl.exe', 'C:\Program Files (x86)\Git\usr\bin\openssl.exe') { if (Test-Path $p) { $ssl = $p; break } }
}
if (-not $ssl) { throw 'openssl not found. Install Git for Windows (it ships openssl) or run the script from Git Bash: sh build/gen-signing-key.sh' }
if (Test-Path repo.key) { throw 'repo.key already exists here: refusing to overwrite it' }
$ErrorActionPreference = "Continue" # openssl prints progress on stderr, which PowerShell 5.1 would treat as an error
& $ssl genrsa -out repo.key 4096 2>$null
if ($LASTEXITCODE) { throw "openssl genrsa failed" }
& $ssl rsa -in repo.key -pubout -out repo.pub 2>$null
if ($LASTEXITCODE) { throw "openssl rsa failed" }
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force keys | Out-Null
Copy-Item repo.pub keys\activestandby.pub -Force
$ErrorActionPreference = "Continue"
$der = [IO.Path]::GetTempFileName()
& $ssl rsa -pubin -in repo.pub -outform DER -out $der 2>$null
$fp = (& $ssl dgst -sha256 $der 2>$null) -replace "^.*= ", ""
Remove-Item $der
$ErrorActionPreference = "Stop"
"private key : repo.key   (keep it secret, do not commit)"
"public key  : keys\activestandby.pub   (commit this one)"
"fingerprint : $fp"
""
"Copy the private key to the clipboard with:   Get-Content repo.key -Raw | Set-Clipboard"
"GitHub > repository > Settings > Secrets and variables > Actions > New repository secret"
"  name  PKG_SIGNING_KEY    value: paste (the BEGIN/END lines included)"
"Then keep repo.key in a password manager and delete it from this folder."
