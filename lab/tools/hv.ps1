# Hyper-V console helpers: send keystrokes / take screenshots of a VM (WMI, needs Hyper-V admin).
# Usage: . .\hv.ps1 ; Send-VMText opn-a "root`n" ; Get-VMShot opn-a out.png
Add-Type -AssemblyName System.Drawing

function Get-VMCim($name) { Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_ComputerSystem -Filter "ElementName='$name'" }

function Send-VMText([string]$name, [string]$text, [int]$delayMs = 60) {
    # TypeText is unreliable on Hyper-V; use virtual-key codes with explicit shift.
    $kb = Get-CimAssociatedInstance -InputObject (Get-VMCim $name) -ResultClassName Msvm_Keyboard
    $sym = @{ ' '=@(32,0); '.'=@(190,0); '-'=@(189,0); '/'=@(191,0); ','=@(188,0); ';'=@(186,0); '='=@(187,0); "'"=@(222,0);
              '_'=@(189,1); ':'=@(186,1); '+'=@(187,1); '"'=@(222,1); '?'=@(191,1); '!'=@(49,1); '@'=@(50,1); '#'=@(51,1);
              '$'=@(52,1); '%'=@(53,1); '&'=@(55,1); '*'=@(56,1); '('=@(57,1); ')'=@(48,1); '>'=@(190,1); '<'=@(188,1); '|'=@(220,1); '\'=@(220,0) }
    foreach ($c in $text.ToCharArray()) {
        $shift = 0
        if ($c -eq "`n") { $vk = 13 }
        elseif ($sym.ContainsKey([string]$c)) { $vk = $sym[[string]$c][0]; $shift = $sym[[string]$c][1] }
        elseif ([char]::IsUpper($c)) { $vk = [int]$c; $shift = 1 }
        else { $vk = [int][char]::ToUpper($c) }
        if ($shift) { Invoke-CimMethod -InputObject $kb -MethodName PressKey -Arguments @{keyCode=16} | Out-Null }
        Invoke-CimMethod -InputObject $kb -MethodName TypeKey -Arguments @{keyCode=$vk} | Out-Null
        if ($shift) { Invoke-CimMethod -InputObject $kb -MethodName ReleaseKey -Arguments @{keyCode=16} | Out-Null }
        Start-Sleep -Milliseconds $delayMs
    }
}

function Send-VMKey([string]$name, [int]$keyCode) {
    $kb = Get-CimAssociatedInstance -InputObject (Get-VMCim $name) -ResultClassName Msvm_Keyboard
    Invoke-CimMethod -InputObject $kb -MethodName TypeKey -Arguments @{keyCode=$keyCode} | Out-Null
}

function Get-VMShot([string]$name, [string]$path) {
    $svc = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_VirtualSystemManagementService
    $vm  = Get-VMCim $name
    $w = 1024; $h = 768
    $r = Invoke-CimMethod -InputObject $svc -MethodName GetVirtualSystemThumbnailImage -Arguments @{
        TargetSystem = $vm; WidthPixels = [uint16]$w; HeightPixels = [uint16]$h }
    $px = $r.ImageData
    $bmp = New-Object System.Drawing.Bitmap $w, $h, ([System.Drawing.Imaging.PixelFormat]::Format16bppRgb565)
    $bd = $bmp.LockBits((New-Object System.Drawing.Rectangle 0,0,$w,$h), 'WriteOnly', $bmp.PixelFormat)
    [System.Runtime.InteropServices.Marshal]::Copy([byte[]]$px, 0, $bd.Scan0, [Math]::Min($px.Length, $bd.Stride*$h))
    $bmp.UnlockBits($bd); $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png); $bmp.Dispose()
}
