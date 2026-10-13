Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
Push-Location $root
try {
    & docker info | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Docker Engine is unavailable (exit $LASTEXITCODE)" }
    & docker compose version
    if ($LASTEXITCODE -ne 0) { throw "Docker Compose is unavailable (exit $LASTEXITCODE)" }
    & docker compose config --quiet
    if ($LASTEXITCODE -ne 0) { throw "Compose configuration is invalid (exit $LASTEXITCODE)" }
    & docker compose -p rcnotif-dev up -d --build app
    if ($LASTEXITCODE -ne 0) { throw "demo stack failed (exit $LASTEXITCODE)" }
    $port = if ($env:APP_PORT) { $env:APP_PORT } else { '8080' }
    Write-Host "Demo stack started. Readiness: http://127.0.0.1:$port/readyz"
} catch {
    Write-Error $_
    exit 1
} finally {
    Pop-Location
}
