param(
  [Parameter(Mandatory = $true)]
  [string]$SourceCompose,

  [Parameter(Mandatory = $true)]
  [string]$DestCompose
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $SourceCompose)) {
  throw "Source compose file not found: $SourceCompose"
}

$lines = Get-Content -LiteralPath $SourceCompose
$usedPorts = [System.Collections.Generic.HashSet[int]]::new()
$reserved = [System.Collections.Generic.HashSet[int]]::new()
$changes = [System.Collections.Generic.List[string]]::new()
$outLines = [System.Collections.Generic.List[string]]::new()
$changed = $false

try {
  $listeners = [System.Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
  foreach ($listener in $listeners) {
    [void]$usedPorts.Add([int]$listener.Port)
  }
}
catch {
}

try {
  $containerIds = docker ps -q 2>$null
  foreach ($containerId in $containerIds) {
    if ([string]::IsNullOrWhiteSpace($containerId)) {
      continue
    }

    try {
      $inspect = docker inspect $containerId 2>$null | ConvertFrom-Json
      if ($null -eq $inspect -or $inspect.Count -lt 1) {
        continue
      }

      $ports = $inspect[0].NetworkSettings.Ports
      if ($null -eq $ports) {
        continue
      }

      foreach ($property in $ports.PSObject.Properties) {
        $bindings = $property.Value
        if ($null -eq $bindings) {
          continue
        }

        foreach ($binding in $bindings) {
          if ($null -ne $binding.HostPort -and $binding.HostPort -match '^[0-9]+$') {
            [void]$usedPorts.Add([int]$binding.HostPort)
          }
        }
      }
    }
    catch {
      continue
    }
  }
}
catch {
}

function Get-NextFreePort {
  param([int]$StartPort)

  $port = $StartPort
  while ($reserved.Contains($port) -or $usedPorts.Contains($port)) {
    $port++
    if ($port -gt 65535) {
      throw "No free port found starting from $StartPort"
    }
  }

  [void]$reserved.Add($port)
  [void]$usedPorts.Add($port)
  return $port
}

foreach ($line in $lines) {
  $m = [regex]::Match($line, '^(?<prefix>\s*-\s*")(?<host>\d+):(?<container>\d+)(?<suffix>".*)$')
  if (-not $m.Success) {
    [void]$outLines.Add($line)
    continue
  }

  $hostPort = [int]$m.Groups['host'].Value
  $containerPort = [int]$m.Groups['container'].Value
  $selectedPort = Get-NextFreePort -StartPort $hostPort

  if ($selectedPort -ne $hostPort) {
    $changed = $true
    [void]$changes.Add("$hostPort->$selectedPort (container $containerPort)")
  }

  $newLine = "{0}{1}:{2}{3}" -f $m.Groups['prefix'].Value, $selectedPort, $containerPort, $m.Groups['suffix'].Value
  [void]$outLines.Add($newLine)
}

if ($changed) {
  Set-Content -LiteralPath $DestCompose -Value $outLines -Encoding UTF8
  Write-Output "AUTO_COMPOSE=$DestCompose"
  Write-Output 'AUTO_CHANGED=1'
  foreach ($change in $changes) {
    Write-Output "AUTO_PORT_CHANGE=$change"
  }
}
else {
  Write-Output "AUTO_COMPOSE=$SourceCompose"
  Write-Output 'AUTO_CHANGED=0'
}
