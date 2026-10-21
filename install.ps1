# ymcp installer for Windows.
#
#   irm https://raw.githubusercontent.com/xlsft/ymcp/main/install.ps1 | iex
#
# Environment:
#   YMCP_VERSION      release tag to install (default: latest), e.g. v0.1.0
#   YMCP_INSTALL_DIR  where to put ymcp.exe (default: %LOCALAPPDATA%\Programs\ymcp)
#   YMCP_NO_SETUP=1   skip `ymcp setup` (credentials and MCP clients)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = 'xlsft/ymcp'
$Version = if ($env:YMCP_VERSION) { $env:YMCP_VERSION } else { 'latest' }
$InstallDir = if ($env:YMCP_INSTALL_DIR) { $env:YMCP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ymcp' }

Write-Host ''
Write-Host ' ymcp ' -NoNewline -ForegroundColor White -BackgroundColor DarkRed
Write-Host ' YouTrack MCP installer'
Write-Host "It's fun to fetch with the YMCP" -ForegroundColor DarkGray
Write-Host ''

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { throw "Unsupported architecture $env:PROCESSOR_ARCHITECTURE" }
}
$Archive = "ymcp_windows_$Arch.zip"
$Base = if ($Version -eq 'latest') { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }

$Tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("ymcp-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $Tmp | Out-Null
try {
  Write-Host "Downloading $Archive ($Version)..." -ForegroundColor DarkGray
  Invoke-WebRequest "$Base/$Archive" -OutFile (Join-Path $Tmp $Archive)
  Invoke-WebRequest "$Base/checksums.txt" -OutFile (Join-Path $Tmp 'checksums.txt')

  $Expected = (Get-Content (Join-Path $Tmp 'checksums.txt') | Where-Object { $_ -match " $([regex]::Escape($Archive))$" }) -split '\s+' | Select-Object -First 1
  if (-not $Expected) { throw "No checksum for $Archive" }
  $Actual = (Get-FileHash (Join-Path $Tmp $Archive) -Algorithm SHA256).Hash.ToLower()
  if ($Expected -ne $Actual) { throw "Checksum mismatch for $Archive" }
  Write-Host '✓ Checksum verified' -ForegroundColor Green

  Expand-Archive (Join-Path $Tmp $Archive) -DestinationPath $Tmp -Force
  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  $Exe = Join-Path $InstallDir 'ymcp.exe'
  Copy-Item (Join-Path $Tmp 'ymcp.exe') $Exe -Force
  Write-Host "✓ Installed $(& $Exe version) to $Exe" -ForegroundColor Green
} finally {
  Remove-Item $Tmp -Recurse -Force -ErrorAction SilentlyContinue
}

$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($UserPath -split ';') -contains $InstallDir)) {
  [Environment]::SetEnvironmentVariable('Path', "$UserPath;$InstallDir", 'User')
  $env:Path = "$env:Path;$InstallDir"
  Write-Host "✓ Added $InstallDir to your PATH (new terminals pick it up)" -ForegroundColor Green
}

if ($env:YMCP_NO_SETUP -eq '1') {
  Write-Host ''
  Write-Host 'Next: run ymcp setup to sign in and register ymcp in your MCP clients.'
} else {
  & $Exe setup
}
