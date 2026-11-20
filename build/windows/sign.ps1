# Signs Windows binaries with the release certificate.
#
# Run by the release workflow, which signs twice: the executable and the engine
# before the installer packs them, then the installer itself. The certificate
# arrives as a base64 .pfx in WINDOWS_CERTIFICATE, with its password in
# WINDOWS_CERTIFICATE_PASSWORD.
#
#   ./build/windows/sign.ps1 -Path build\bin\lotus.exe, build\bin\engine\tectonic.exe

param(
    [Parameter(Mandatory)]
    [string[]]$Path
)

$ErrorActionPreference = "Stop"

# signtool ships with the Windows SDK but is not on the runner's PATH. Take the
# newest SDK's copy.
$signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" |
    Where-Object { $_.Directory.Parent.Name -as [version] } |
    Sort-Object { [version]$_.Directory.Parent.Name } -Descending |
    Select-Object -First 1
if (-not $signtool) {
    throw "signtool.exe not found in the Windows SDK"
}

$pfx = Join-Path $env:RUNNER_TEMP "cert.pfx"
[IO.File]::WriteAllBytes($pfx, [Convert]::FromBase64String($env:WINDOWS_CERTIFICATE))
try {
    foreach ($target in $Path) {
        & $signtool.FullName sign /f $pfx /p $env:WINDOWS_CERTIFICATE_PASSWORD /fd SHA256 `
            /tr http://timestamp.digicert.com /td SHA256 $target
        if ($LASTEXITCODE -ne 0) {
            throw "signtool failed on $target"
        }
    }
} finally {
    Remove-Item $pfx -ErrorAction SilentlyContinue
}
