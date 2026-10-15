Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$runId = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [Guid]::NewGuid().ToString('N').Substring(0,8)
$project = 'rcnotif-verify-' + $runId.ToLowerInvariant()
$reportDir = Join-Path $root (Join-Path 'reports' $runId)
$evidenceDir = Join-Path $root (Join-Path 'docs/evidence' $runId)
New-Item -ItemType Directory -Force -Path $reportDir, $evidenceDir | Out-Null
$log = Join-Path $reportDir 'verify.log'
$summary = Join-Path $reportDir 'summary.txt'
$exitCode = 0
$composeTouched = $false
$runnerId = ''
$oldLocation = Get-Location
Push-Location $root
$env:APP_IMAGE = 'rc-notification:0.1.0'
$env:TEST_IMAGE = 'rc-notification-test:0.1.0'
$env:CERT_IMAGE = 'rc-notification-cert-init:0.1.0'
$env:ADMIN_ACTOR = 'integration-test'
$env:DELIVERY_BUDGET = '90s'
$env:MQ_CONSUME_TIMEOUT = '2m'
$env:VERIFY_RUN_ID = $runId
$env:APP_BIND = '127.0.0.1'
$env:APP_PORT = [string](Get-Random -Minimum 24000 -Maximum 52000)
$composeArgs = @('-p',$project,'--project-directory',$root,'-f',(Join-Path $root 'compose.yaml'),'-f',(Join-Path $root 'compose.verify.yaml'))

function Invoke-DockerStep([string]$Name, [string[]]$Arguments) {
    Write-Output ("## " + $Name + " command=docker " + ($Arguments -join ' ')) | Tee-Object -FilePath $log -Append
    & docker @Arguments 2>&1 | Tee-Object -FilePath $log -Append
    $code = $LASTEXITCODE
    ("exit_code=" + $code) | Tee-Object -FilePath $log -Append
    ("stage_" + ($Name -replace '[^a-zA-Z0-9]+','_').Trim('_').ToLowerInvariant() + "_exit_code=$code") | Add-Content -LiteralPath $summary -Encoding utf8
    if ($code -ne 0) { throw "$Name failed with exit code $code" }
}

function Wait-Healthy([string]$Service, [int]$TimeoutSeconds) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $idArgs = @('compose') + $composeArgs + @('ps','-q',$Service)
        $serviceId = (& docker @idArgs 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -eq 0 -and $serviceId) {
            $health = (& docker inspect $serviceId --format '{{.State.Health.Status}}' 2>$null | Select-Object -First 1)
            if ($LASTEXITCODE -eq 0 -and $health -eq 'healthy') { return $serviceId }
            if ($LASTEXITCODE -eq 0 -and $health -eq 'unhealthy') { throw "$Service health check reported unhealthy" }
        }
        Start-Sleep -Seconds 2
    }
    throw "$Service did not become healthy within $TimeoutSeconds seconds"
}

function Wait-File([string]$Path, [int]$TimeoutSeconds) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out waiting for $([IO.Path]::GetFileName($Path))"
}

try {
    $started = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    @("run_id=$runId","project=$project","started_at_utc=$started","project_root=$root") | Set-Content -LiteralPath $summary -Encoding utf8
    Invoke-DockerStep 'docker info' @('info')
    Invoke-DockerStep 'docker version' @('version')
    Invoke-DockerStep 'docker compose version' @('compose','version')
    Invoke-DockerStep 'compose config --quiet' (@('compose') + $composeArgs + @('config','--quiet'))
    Invoke-DockerStep 'build application image' @('build','--pull','--target','runtime','--tag',$env:APP_IMAGE,$root)
    Invoke-DockerStep 'build test-runner image' @('build','--pull','--target','test-runner','--tag',$env:TEST_IMAGE,$root)
    Invoke-DockerStep 'build certificate initializer image' @('build','--pull','--target','cert-init','--tag',$env:CERT_IMAGE,$root)
    $composeTouched = $true
    Invoke-DockerStep 'start isolated stack' (@('compose') + $composeArgs + @('up','-d','app'))
    $appId = Wait-Healthy 'app' 300
    'stack_health=PASS' | Add-Content -LiteralPath $summary -Encoding utf8

    $runnerArgs = @('compose') + $composeArgs + @('run','-d','--no-deps','-e','TEST_REPORT=/reports/sigterm.json','-e','TEST_CONTROL_DIR=/reports','test-runner','go','test','-tags=integration','-run','^TestSIGTERMInterruptsInFlightDelivery$','-count=1','-v','./tests/integration')
    '## SIGTERM in-flight delivery test' | Tee-Object -FilePath $log -Append
    $runnerOutput = & docker @runnerArgs 2>&1
    $runnerCode = $LASTEXITCODE
    $runnerOutput | Tee-Object -FilePath $log -Append | Out-Null
    if ($runnerCode -ne 0) { throw "could not start SIGTERM integration runner: $runnerCode" }
    $runnerId = [string]($runnerOutput | Select-Object -Last 1)
    if (-not $runnerId) { throw 'Docker did not return a SIGTERM runner container id' }
    'sigterm_runner_start_exit_code=0' | Add-Content -LiteralPath $summary -Encoding utf8
    Wait-File (Join-Path $reportDir 'sigterm-armed') 120
    Invoke-DockerStep 'stop app during outbound HTTP with SIGTERM' (@('compose') + $composeArgs + @('stop','-t','20','app'))
    $appStopState = & docker inspect $appId --format '{{.State.Status}}|{{.State.ExitCode}}' 2>&1
    if ($LASTEXITCODE -ne 0 -or $appStopState -ne 'exited|0') { throw "app did not exit cleanly after SIGTERM: $appStopState" }
    $appLogs = & docker compose @composeArgs logs --no-color app 2>&1
    if ($LASTEXITCODE -ne 0 -or (($appLogs -join "`n") -notmatch 'shutdown signal received') -or (($appLogs -join "`n") -notmatch 'server stopped')) { throw 'SIGTERM logs did not confirm graceful shutdown' }
    $appLogs | Set-Content -LiteralPath (Join-Path $reportDir 'sigterm-app.log') -Encoding utf8
    'stopped' | Set-Content -LiteralPath (Join-Path $reportDir 'sigterm-stopped') -NoNewline -Encoding utf8
    Invoke-DockerStep 'restart app after SIGTERM test' (@('compose') + $composeArgs + @('start','app'))
    [void](Wait-Healthy 'app' 180)
    'healthy' | Set-Content -LiteralPath (Join-Path $reportDir 'sigterm-resume') -NoNewline -Encoding utf8
    $deadline = [DateTime]::UtcNow.AddMinutes(4)
    $runnerFinished = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $runnerState = & docker inspect $runnerId --format '{{.State.Running}}|{{.State.ExitCode}}' 2>$null | Select-Object -First 1
        if ($LASTEXITCODE -eq 0 -and $runnerState -and $runnerState.StartsWith('false|')) { $runnerFinished = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $runnerFinished) { throw 'SIGTERM integration runner did not finish within four minutes' }
    & docker logs $runnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    $runnerState = & docker inspect $runnerId --format '{{.State.Running}}|{{.State.ExitCode}}'
    $runnerCode = [int]($runnerState -split '\|')[1]
    "sigterm_integration_exit_code=$runnerCode" | Add-Content -LiteralPath $summary -Encoding utf8
    & docker rm $runnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not remove completed SIGTERM runner: $LASTEXITCODE" }
    $runnerId = ''
    if ($runnerCode -ne 0) { throw "SIGTERM integration failed with exit code $runnerCode" }

    Invoke-DockerStep 'full Docker verification' (@('compose') + $composeArgs + @('run','--rm','test-runner'))
    'full_verification_exit_code=0' | Add-Content -LiteralPath $summary -Encoding utf8
}
catch {
    $exitCode = if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { $LASTEXITCODE } else { 1 }
    ("failure=" + $_.Exception.Message) | Add-Content -LiteralPath $summary -Encoding utf8
}
finally {
    if ($composeTouched) {
        if ($runnerId) {
            & docker rm -f $runnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
        }
        foreach ($service in @('app','mysql','namesrv','broker','mock-target')) {
            $idArgs = @('compose') + $composeArgs + @('ps','-q',$service)
            $serviceId = (& docker @idArgs 2>$null | Select-Object -First 1)
            if ($LASTEXITCODE -eq 0 -and $serviceId) {
                $logArgs = @('compose') + $composeArgs + @('logs','--no-color','--tail=200',$service)
                & docker @logArgs 2>&1 | Set-Content -LiteralPath (Join-Path $reportDir ($service + '.log')) -Encoding utf8
                if ($LASTEXITCODE -ne 0 -and $exitCode -eq 0) { $exitCode = $LASTEXITCODE }
            }
        }
        $appLog = Join-Path $reportDir 'app.log'
        if (Test-Path -LiteralPath $appLog) {
            $canaryPattern = 'notification-body-secret-canary-9ba8|demo-client-token|demo-admin-token|Authorization:|Cookie:'
            if (Select-String -LiteralPath $appLog -Pattern $canaryPattern -Quiet) {
                'log_secret_scan=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8
                if ($exitCode -eq 0) { $exitCode = 1 }
            } else { 'log_secret_scan=PASS' | Add-Content -LiteralPath $summary -Encoding utf8 }
            $contextFile = Join-Path $reportDir 'log-context.tsv'
            $contextFailure = $false
            if (Test-Path -LiteralPath $contextFile) {
                foreach ($line in Get-Content -LiteralPath $contextFile) {
                    if (-not $line) { continue }
                    $parts = $line -split "`t", 2
                    if ($parts.Count -ne 2) { $contextFailure = $true; break }
                    $matches = @(Select-String -LiteralPath $appLog -SimpleMatch ('task_id=' + $parts[0]) | Where-Object { $_.Line.Contains('msg="notification processing finished"') -and $_.Line.Contains('trace_id=' + $parts[1]) })
                    if ($matches.Count -lt 1) { $contextFailure = $true; break }
                }
                if ($contextFailure) {
                    'log_context_scan=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8
                    if ($exitCode -eq 0) { $exitCode = 1 }
                } else { 'log_context_scan=PASS' | Add-Content -LiteralPath $summary -Encoding utf8 }
                Get-Content -LiteralPath $contextFile | Set-Content -LiteralPath (Join-Path $evidenceDir 'log-context.tsv') -Encoding utf8
            } elseif ($exitCode -eq 0) {
                'log_context_scan=FAIL (integration context file missing)' | Add-Content -LiteralPath $summary -Encoding utf8
                $exitCode = 1
            }
            if (Test-Path -LiteralPath $appLog) {
                Get-Content -LiteralPath $appLog | Where-Object { $_.Contains('msg="notification processing finished"') } | Select-Object -Last 100 | Set-Content -LiteralPath (Join-Path $evidenceDir 'app-results.log') -Encoding utf8
            }
        }
        $ps = @('compose') + $composeArgs + @('ps','-a')
        & docker @ps 2>&1 | Set-Content -LiteralPath (Join-Path $reportDir 'compose-ps.txt') -Encoding utf8
        $down = @('compose') + $composeArgs + @('down','--volumes','--remove-orphans')
        & docker @down 2>&1 | Tee-Object -FilePath $log -Append
        $downCode = $LASTEXITCODE
        "cleanup_exit_code=$downCode" | Add-Content -LiteralPath $summary -Encoding utf8
        if ($downCode -ne 0 -and $exitCode -eq 0) { $exitCode = $downCode }
    }
    foreach ($image in @(@('app_image_id',$env:APP_IMAGE),@('test_image_id',$env:TEST_IMAGE),@('cert_init_image_id',$env:CERT_IMAGE))) {
        $imageId = & docker image inspect $image[1] --format '{{.Id}}' 2>&1
        if ($LASTEXITCODE -eq 0) { ("$($image[0])=$imageId") | Add-Content -LiteralPath $summary -Encoding utf8 }
    }
    if ($exitCode -eq 0) { 'result=PASS' | Add-Content -LiteralPath $summary -Encoding utf8 } else { 'result=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8 }
    "finished_at_utc=$((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))" | Add-Content -LiteralPath $summary -Encoding utf8
    Copy-Item -LiteralPath $summary -Destination (Join-Path $evidenceDir 'summary.txt') -Force
    foreach ($file in @('checks.log','checks-summary.txt','integration.json','sigterm.json','compose-ps.txt')) {
        $source = Join-Path $reportDir $file
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $evidenceDir $file) -Force }
    }
    Pop-Location
}
Write-Output "run_id=$runId"
Write-Output "report=$reportDir"
Write-Output "evidence=$evidenceDir"
Write-Output "exit_code=$exitCode"
exit $exitCode
