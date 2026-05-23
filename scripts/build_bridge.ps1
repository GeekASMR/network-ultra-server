# Build network-ultra-bridge.exe with size-optimised flags + optional UPX pack.
#
# Why we do this:
#   * Default Go build → 9.8 MB (debug symbols + DWARF + full Go runtime)
#   * `-ldflags="-s -w" -trimpath` strips them → 6.9 MB (-30%)
#   * UPX --best --lzma packs it → 2.2 MB (-78% from default)
#
# UPX is auto-downloaded to %TEMP%\upx if not on PATH. Set $env:NU_NO_UPX=1 to
# skip packing (e.g. some antivirus engines flag UPX-packed exes; provide an
# unpacked fallback).
#
# Usage:
#   pwsh scripts\build_bridge.ps1
#   $env:NU_NO_UPX = "1"; pwsh scripts\build_bridge.ps1   # skip UPX
#
$ErrorActionPreference = 'Stop'

$ScriptDir   = Split-Path -Parent $MyInvocation.MyCommand.Path
$ServerRoot  = Split-Path -Parent $ScriptDir
$Output      = Join-Path $ServerRoot 'bin\network-ultra-bridge.exe'

Set-Location $ServerRoot

# 1. go build with size-optimised flags
Write-Host "[1/3] go build with -ldflags='-s -w' -trimpath..." -ForegroundColor Cyan
go build -trimpath -ldflags="-s -w" -o $Output ./cmd/bridge
if ($LASTEXITCODE -ne 0) { throw "go build failed" }
$origSize = (Get-Item $Output).Length
Write-Host ("    {0,7:N2} MB stripped" -f ($origSize / 1MB)) -ForegroundColor Green

if ($env:NU_NO_UPX -eq "1") {
    Write-Host "[2/3] UPX skipped (NU_NO_UPX=1)" -ForegroundColor Yellow
    Write-Host "[3/3] done. final = $($origSize / 1MB) MB" -ForegroundColor Green
    return
}

# 2. Locate or download UPX
$upx = $null
$onPath = Get-Command upx -ErrorAction SilentlyContinue
if ($onPath) {
    $upx = $onPath.Path
    Write-Host "[2/3] UPX on PATH: $upx" -ForegroundColor Cyan
} else {
    $upxDir = Join-Path $env:TEMP 'upx'
    $upxExe = Get-ChildItem $upxDir -Recurse -Filter 'upx.exe' -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($upxExe) {
        $upx = $upxExe.FullName
        Write-Host "[2/3] UPX cached: $upx" -ForegroundColor Cyan
    } else {
        Write-Host "[2/3] downloading UPX 4.2.4..." -ForegroundColor Cyan
        $url = "https://github.com/upx/upx/releases/download/v4.2.4/upx-4.2.4-win64.zip"
        $proxies = @("", "https://gh-proxy.com/", "https://ghproxy.net/")
        $zipPath = Join-Path $env:TEMP 'upx.zip'
        $downloaded = $false
        foreach ($prefix in $proxies) {
            $finalUrl = "$prefix$url"
            try {
                Invoke-WebRequest -Uri $finalUrl -OutFile $zipPath -UseBasicParsing -TimeoutSec 60 -ErrorAction Stop
                $downloaded = $true
                Write-Host "    via $finalUrl" -ForegroundColor DarkGray
                break
            } catch {
                continue
            }
        }
        if (-not $downloaded) {
            Write-Host "    UPX 下载失败,跳过压缩" -ForegroundColor Yellow
            return
        }
        Expand-Archive -Path $zipPath -DestinationPath $upxDir -Force
        $upxExe = Get-ChildItem $upxDir -Recurse -Filter 'upx.exe' | Select-Object -First 1
        $upx = $upxExe.FullName
        Write-Host "    extracted to $upx" -ForegroundColor DarkGray
    }
}

# 3. Pack with UPX (--best --lzma is the most aggressive preset)
Write-Host "[3/3] UPX --best --lzma..." -ForegroundColor Cyan
& $upx --best --lzma --quiet $Output
if ($LASTEXITCODE -ne 0) { throw "upx pack failed" }
$finalSize = (Get-Item $Output).Length
$savings = 1.0 - ($finalSize / $origSize)
Write-Host ("    {0,7:N2} MB packed ({1:P0} smaller than stripped)" -f ($finalSize / 1MB), $savings) -ForegroundColor Green
Write-Host ""
Write-Host "✓ done. $Output" -ForegroundColor Green
