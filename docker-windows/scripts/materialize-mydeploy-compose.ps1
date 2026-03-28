param(
  [Parameter(Mandatory = $true)]
  [string]$SourceCompose,

  [Parameter(Mandatory = $true)]
  [string]$DestCompose,

  [Parameter(Mandatory = $true)]
  [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $SourceCompose)) {
  throw "Source compose file not found: $SourceCompose"
}

$repoRootNormalized = ($RepoRoot.TrimEnd('\') -replace '\\', '/')
$repoRootQuoted = '"' + $repoRootNormalized + '"'

$lines = Get-Content -LiteralPath $SourceCompose
$updated = $false
$outLines = [System.Collections.Generic.List[string]]::new()

foreach ($line in $lines) {
  if ($line -match '^(\s*context:\s*).*$') {
    $line = "$($matches[1])$repoRootQuoted"
    $updated = $true
  }

  [void]$outLines.Add($line)
}

if (-not $updated) {
  throw 'Unable to find build.context entry in compose file.'
}

$destDir = Split-Path -Parent $DestCompose
if (-not (Test-Path -LiteralPath $destDir)) {
  New-Item -ItemType Directory -Path $destDir -Force | Out-Null
}

Set-Content -LiteralPath $DestCompose -Value $outLines -Encoding UTF8
Write-Output "MATERIALIZED_COMPOSE=$DestCompose"
