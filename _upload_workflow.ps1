# One-shot: upload .github/workflows/release.yml via GitHub Contents API.
$ErrorActionPreference = 'Stop'

$repo    = 'GeekASMR/network-ultra-server'
$path    = '.github/workflows/release.yml'
$message = 'ci: add release workflow'

$srcText = [System.IO.File]::ReadAllText("$PWD\$path")
$lfText  = $srcText -replace "`r`n", "`n"
$bytes   = [System.Text.Encoding]::UTF8.GetBytes($lfText)
$b64     = [Convert]::ToBase64String($bytes)

$payload = @{
  message = $message
  content = $b64
} | ConvertTo-Json -Depth 3 -Compress

$tmp = [System.IO.Path]::GetTempFileName()
[System.IO.File]::WriteAllText($tmp, $payload, (New-Object System.Text.UTF8Encoding $false))

try {
  gh api --method PUT "repos/$repo/contents/$path" --input $tmp
} finally {
  Remove-Item $tmp -ErrorAction SilentlyContinue
}
