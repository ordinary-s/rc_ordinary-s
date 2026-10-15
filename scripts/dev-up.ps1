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
    $certificateDir = Join-Path $root '.local-certs'
    New-Item -ItemType Directory -Force -Path $certificateDir | Out-Null
    & docker compose -p rcnotif-dev cp cert-init:/certs/ca.crt (Join-Path $certificateDir 'ca.crt')
    if ($LASTEXITCODE -ne 0) { throw "could not export local CA certificate (exit $LASTEXITCODE)" }
    $port = if ($env:APP_PORT) { $env:APP_PORT } else { '8443' }
    Write-Host "Demo stack started. Readiness: https://localhost:$port/readyz"
    Write-Host "Local-only CA: $certificateDir\ca.crt (use curl.exe --cacert or trust it only if you choose)"
} catch {
    Write-Error $_
    exit 1
} finally {
    Pop-Location
}
