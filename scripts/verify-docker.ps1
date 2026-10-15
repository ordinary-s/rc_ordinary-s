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
$outageRunnerId = ''
$env:APP_IMAGE = 'rc-notification:0.1.0'
$env:TEST_IMAGE = 'rc-notification-test:0.1.0'
$env:CERT_IMAGE = 'rc-notification-cert-init:0.1.0'
$env:ADMIN_ACTOR = 'integration-test'
$env:DELIVERY_BUDGET = '90s'
$env:MQ_CONSUME_TIMEOUT = '2m'
$env:VERIFY_RUN_ID = $runId
$env:APP_BIND = '127.0.0.1'
$env:APP_PORT = [string](Get-Random -Minimum 24000 -Maximum 52000)

function Invoke-DockerStep([string]$Name, [string[]]$Arguments) {
    Write-Output ("## " + $Name + " command=docker " + ($Arguments -join ' ')) | Tee-Object -FilePath $log -Append
    & docker @Arguments 2>&1 | Tee-Object -FilePath $log -Append
    $code = $LASTEXITCODE
    ("exit_code=" + $code) | Tee-Object -FilePath $log -Append
    if ($code -ne 0) { throw "$Name failed with exit code $code" }
}

function Save-LimitedLogs([string[]]$ComposeArgs) {
    foreach ($service in @('app','mysql','namesrv','broker','mock-target')) {
        $path = Join-Path $reportDir ($service + '.log')
        $args = @('compose') + $ComposeArgs + @('logs','--no-color','--tail=200',$service)
        & docker @args 2>&1 | Set-Content -LiteralPath $path -Encoding utf8
        if ($LASTEXITCODE -ne 0) { throw "could not collect $service logs: $LASTEXITCODE" }
    }
}

try {
    $started = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    @("run_id=$runId","project=$project","started_at_utc=$started","project_root=$root") | Set-Content -LiteralPath $summary -Encoding utf8
    Invoke-DockerStep 'docker info' @('info')
    Invoke-DockerStep 'docker version' @('version')
    Invoke-DockerStep 'docker compose version' @('compose','version')
    $composeArgs = @('-p',$project,'-f',(Join-Path $root 'compose.yaml'),'-f',(Join-Path $root 'compose.verify.yaml'))
    Invoke-DockerStep 'compose config --quiet' (@('compose') + $composeArgs + @('config','--quiet'))
    Invoke-DockerStep 'build application image' @('build','--pull','--target','runtime','--tag',$env:APP_IMAGE,(Join-Path $root '.'))
    Invoke-DockerStep 'build test-runner image' @('build','--pull','--target','test-runner','--tag',$env:TEST_IMAGE,(Join-Path $root '.'))
    Invoke-DockerStep 'build local certificate initializer image' @('build','--pull','--target','cert-init','--tag',$env:CERT_IMAGE,(Join-Path $root '.'))
    $composeTouched = $true
    Invoke-DockerStep 'start isolated stack' (@('compose') + $composeArgs + @('up','-d','app'))
    $deadline = [DateTime]::UtcNow.AddMinutes(5)
    $appHealthy = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $idArgs = @('compose') + $composeArgs + @('ps','-q','app')
        $appId = (& docker @idArgs 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -eq 0 -and $appId) {
            $health = (& docker inspect $appId --format '{{.State.Health.Status}}' 2>$null | Select-Object -First 1)
            if ($LASTEXITCODE -eq 0 -and $health -eq 'healthy') { $appHealthy = $true; break }
            if ($health -eq 'unhealthy') { throw 'application health check reported unhealthy' }
        }
        Start-Sleep -Seconds 2
    }
    if (-not $appHealthy) { throw 'stack did not become ready within five minutes' }
    'stack_health=PASS' | Add-Content -LiteralPath $summary -Encoding utf8

    $runnerArgs = @('compose') + $composeArgs + @('run','-d','--no-deps','-e','TEST_REPORT=/reports/outage.json','-e','TEST_CONTROL_DIR=/reports','test-runner','go','test','-tags=integration','-run','^TestBrokerOutageOutboxRecoveryAndExpiry$','-count=1','-v','./tests/integration')
    '## broker outage coordinated test' | Tee-Object -FilePath $log -Append
    $runnerOutput = & docker @runnerArgs 2>&1
    $runnerCode = $LASTEXITCODE
    $runnerOutput | Tee-Object -FilePath $log -Append | Out-Null
    if ($runnerCode -ne 0) { throw "could not start broker outage integration runner: $runnerCode" }
    $outageRunnerId = [string]($runnerOutput | Select-Object -Last 1)
    if (-not $outageRunnerId) { throw 'Docker did not return the outage runner container id' }
    'broker_outage_runner_started=0' | Add-Content -LiteralPath $summary -Encoding utf8
    $armed = Join-Path $reportDir 'broker-outage-armed'
    $queued = Join-Path $reportDir 'broker-outage-queued'
    $stopped = Join-Path $reportDir 'broker-outage-stopped'
    $resume = Join-Path $reportDir 'broker-outage-resume'
    $deadline = [DateTime]::UtcNow.AddMinutes(2)
    while (-not (Test-Path -LiteralPath $armed) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path -LiteralPath $armed)) { throw 'outage runner did not arm within two minutes' }
    Invoke-DockerStep 'stop broker for outage test' (@('compose') + $composeArgs + @('stop','broker'))
    'broker_outage_stop_exit_code=0' | Add-Content -LiteralPath $summary -Encoding utf8
    'stopped' | Set-Content -LiteralPath $stopped -NoNewline -Encoding utf8
    $deadline = [DateTime]::UtcNow.AddMinutes(2)
    while (-not (Test-Path -LiteralPath $queued) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path -LiteralPath $queued)) { throw 'outage runner did not confirm durable pending Outbox within two minutes' }
    Invoke-DockerStep 'start broker after outage' (@('compose') + $composeArgs + @('start','broker'))
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    $brokerHealthy = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $brokerIdArgs = @('compose') + $composeArgs + @('ps','-q','broker')
        $brokerId = (& docker @brokerIdArgs 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -eq 0 -and $brokerId) {
            $brokerHealth = (& docker inspect $brokerId --format '{{.State.Health.Status}}' 2>$null | Select-Object -First 1)
            if ($LASTEXITCODE -eq 0 -and $brokerHealth -eq 'healthy') { $brokerHealthy = $true; break }
        }
        Start-Sleep -Seconds 2
    }
    if (-not $brokerHealthy) { throw 'broker did not become healthy after restart' }
    'healthy' | Set-Content -LiteralPath $resume -NoNewline -Encoding utf8
    $deadline = [DateTime]::UtcNow.AddMinutes(4)
    $runnerFinished = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $runnerState = & docker inspect $outageRunnerId --format '{{.State.Running}}|{{.State.ExitCode}}' 2>$null | Select-Object -First 1
        if ($LASTEXITCODE -eq 0 -and $runnerState -and $runnerState.StartsWith('false|')) { $runnerFinished = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $runnerFinished) { throw 'broker outage integration runner did not finish within four minutes' }
    & docker logs $outageRunnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    $runnerState = & docker inspect $outageRunnerId --format '{{.State.Running}}|{{.State.ExitCode}}'
    $runnerCode = [int]($runnerState -split '\|')[1]
    "broker_outage_integration_exit_code=$runnerCode" | Add-Content -LiteralPath $summary -Encoding utf8
    & docker rm $outageRunnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not remove completed outage runner: $LASTEXITCODE" }
    $outageRunnerId = ''
    if ($runnerCode -ne 0) { throw "broker outage integration failed with exit code $runnerCode" }

    $runnerArgs = @('compose') + $composeArgs + @('run','-d','--no-deps','-e','TEST_REPORT=/reports/sigterm.json','-e','TEST_CONTROL_DIR=/reports','test-runner','go','test','-tags=integration','-run','^TestSIGTERMInterruptsInFlightDelivery$','-count=1','-v','./tests/integration')
    '## SIGTERM in-flight delivery test' | Tee-Object -FilePath $log -Append
    $runnerOutput = & docker @runnerArgs 2>&1
    $runnerCode = $LASTEXITCODE
    $runnerOutput | Tee-Object -FilePath $log -Append | Out-Null
    if ($runnerCode -ne 0) { throw "could not start SIGTERM integration runner: $runnerCode" }
    $outageRunnerId = [string]($runnerOutput | Select-Object -Last 1)
    if (-not $outageRunnerId) { throw 'Docker did not return the SIGTERM runner container id' }
    'sigterm_runner_started=0' | Add-Content -LiteralPath $summary -Encoding utf8
    $sigtermArmed = Join-Path $reportDir 'sigterm-armed'
    $sigtermStopped = Join-Path $reportDir 'sigterm-stopped'
    $sigtermResume = Join-Path $reportDir 'sigterm-resume'
    $deadline = [DateTime]::UtcNow.AddMinutes(2)
    while (-not (Test-Path -LiteralPath $sigtermArmed) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path -LiteralPath $sigtermArmed)) { throw 'SIGTERM test did not observe an in-flight request within two minutes' }
    $appIdArgs = @('compose') + $composeArgs + @('ps','-q','app')
    $appId = (& docker @appIdArgs 2>$null | Select-Object -First 1)
    if ($LASTEXITCODE -ne 0 -or -not $appId) { throw 'could not identify app container before SIGTERM' }
    Invoke-DockerStep 'stop app with SIGTERM during HTTP request' (@('compose') + $composeArgs + @('stop','-t','20','app'))
    $appStopState = & docker inspect $appId --format '{{.State.Status}}|{{.State.ExitCode}}' 2>&1
    if ($LASTEXITCODE -ne 0 -or $appStopState -ne 'exited|0') { throw "app did not exit cleanly after SIGTERM: $appStopState" }
    $appLogsArgs = @('compose') + $composeArgs + @('logs','--no-color','app')
    $appLogs = & docker @appLogsArgs 2>&1
    $appLogsCode = $LASTEXITCODE
    $appLogs | Add-Content -LiteralPath (Join-Path $reportDir 'sigterm-app.log') -Encoding utf8
    if ($appLogsCode -ne 0 -or (($appLogs -join "`n") -notmatch 'shutdown signal received') -or (($appLogs -join "`n") -notmatch 'server stopped')) { throw 'SIGTERM logs did not confirm graceful shutdown' }
    'stopped' | Set-Content -LiteralPath $sigtermStopped -NoNewline -Encoding utf8
    Invoke-DockerStep 'restart app after SIGTERM test' (@('compose') + $composeArgs + @('start','app'))
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    $appHealthy = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $appId = (& docker @appIdArgs 2>$null | Select-Object -First 1)
        if ($LASTEXITCODE -eq 0 -and $appId) {
            $health = (& docker inspect $appId --format '{{.State.Health.Status}}' 2>$null | Select-Object -First 1)
            if ($LASTEXITCODE -eq 0 -and $health -eq 'healthy') { $appHealthy = $true; break }
            if ($health -eq 'unhealthy') { throw 'application health check failed after SIGTERM restart' }
        }
        Start-Sleep -Seconds 2
    }
    if (-not $appHealthy) { throw 'application did not become ready after SIGTERM restart' }
    'healthy' | Set-Content -LiteralPath $sigtermResume -NoNewline -Encoding utf8
    $deadline = [DateTime]::UtcNow.AddMinutes(4)
    $runnerFinished = $false
    while ([DateTime]::UtcNow -lt $deadline) {
        $runnerState = & docker inspect $outageRunnerId --format '{{.State.Running}}|{{.State.ExitCode}}' 2>$null | Select-Object -First 1
        if ($LASTEXITCODE -eq 0 -and $runnerState -and $runnerState.StartsWith('false|')) { $runnerFinished = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $runnerFinished) { throw 'SIGTERM integration runner did not finish within four minutes' }
    & docker logs $outageRunnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    $runnerState = & docker inspect $outageRunnerId --format '{{.State.Running}}|{{.State.ExitCode}}'
    $runnerCode = [int]($runnerState -split '\|')[1]
    "sigterm_integration_exit_code=$runnerCode" | Add-Content -LiteralPath $summary -Encoding utf8
    & docker rm $outageRunnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not remove completed SIGTERM runner: $LASTEXITCODE" }
    $outageRunnerId = ''
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
        try {
            $composeArgs = @('-p',$project,'-f',(Join-Path $root 'compose.yaml'),'-f',(Join-Path $root 'compose.verify.yaml'))
            if ($outageRunnerId) { & docker rm -f $outageRunnerId 2>&1 | Tee-Object -FilePath $log -Append | Out-Null }
            Save-LimitedLogs $composeArgs
            $canaryPattern = 'notification-body-secret-canary-9ba8|demo-client-token|demo-admin-token|Authorization:|Cookie:'
            $canaryMatches = @(Select-String -LiteralPath (Join-Path $reportDir 'app.log') -Pattern $canaryPattern)
            if ($canaryMatches.Count -gt 0) {
                'log_secret_scan_exit_code=1' | Add-Content -LiteralPath $summary -Encoding utf8
                'log_secret_scan=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8
                if ($exitCode -eq 0) { $exitCode = 1 }
            } else {
                'log_secret_scan_exit_code=0' | Add-Content -LiteralPath $summary -Encoding utf8
                'log_secret_scan=PASS' | Add-Content -LiteralPath $summary -Encoding utf8
            }
            $contextFile = Join-Path $reportDir 'log-context.tsv'
            $contextFailure = $false
            $contextSkipped = -not (Test-Path -LiteralPath $contextFile)
            if (Test-Path -LiteralPath $contextFile) {
                foreach ($line in Get-Content -LiteralPath $contextFile) {
                    if (-not $line) { continue }
                    $parts = $line -split "`t", 2
                    if ($parts.Count -ne 2) { $contextFailure = $true; break }
                    $taskToken = 'task_id=' + $parts[0]
                    $traceToken = 'trace_id=' + $parts[1]
                    $taskLines = @(Select-String -LiteralPath (Join-Path $reportDir 'app.log') -SimpleMatch $taskToken)
                    $accepted = @($taskLines | Where-Object { $_.Line.Contains('msg="notification accepted"') -and $_.Line.Contains($traceToken) })
                    $finished = @($taskLines | Where-Object { $_.Line.Contains('msg="notification processing finished"') -and $_.Line.Contains($traceToken) })
                    if ($accepted.Count -ne 1 -or $finished.Count -ne 1) { $contextFailure = $true; break }
                }
            } elseif ($exitCode -eq 0) {
                $contextFailure = $true
            }
            if ($contextFailure) {
                'log_context_scan_exit_code=1' | Add-Content -LiteralPath $summary -Encoding utf8
                'log_context_scan=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8
                if ($exitCode -eq 0) { $exitCode = 1 }
            } elseif ($contextSkipped) {
                'log_context_scan_exit_code=NOT_RUN' | Add-Content -LiteralPath $summary -Encoding utf8
                'log_context_scan=NOT_RUN' | Add-Content -LiteralPath $summary -Encoding utf8
            } else {
                'log_context_scan_exit_code=0' | Add-Content -LiteralPath $summary -Encoding utf8
                'log_context_scan=PASS' | Add-Content -LiteralPath $summary -Encoding utf8
            }
            $ps = @('compose') + $composeArgs + @('ps','-a')
            & docker @ps 2>&1 | Set-Content -LiteralPath (Join-Path $reportDir 'compose-ps.txt') -Encoding utf8
            $appInspect = & docker image inspect $env:APP_IMAGE --format '{{.Id}}' 2>&1
            if ($LASTEXITCODE -ne 0) { throw "could not inspect application image: $LASTEXITCODE" }
            $testInspect = & docker image inspect $env:TEST_IMAGE --format '{{.Id}}' 2>&1
            if ($LASTEXITCODE -ne 0) { throw "could not inspect test-runner image: $LASTEXITCODE" }
            $certInspect = & docker image inspect $env:CERT_IMAGE --format '{{.Id}}' 2>&1
            if ($LASTEXITCODE -ne 0) { throw "could not inspect cert-init image: $LASTEXITCODE" }
            @("app_image_id=$appInspect","test_image_id=$testInspect","cert_init_image_id=$certInspect") | Add-Content -LiteralPath $summary -Encoding utf8
            $down = @('compose') + $composeArgs + @('down','--volumes','--remove-orphans')
            & docker @down 2>&1 | Tee-Object -FilePath $log -Append
            $downCode = $LASTEXITCODE
            "cleanup_exit_code=$downCode" | Add-Content -LiteralPath $summary -Encoding utf8
            if ($downCode -ne 0 -and $exitCode -eq 0) { $exitCode = $downCode }
        } catch {
            ("cleanup_error=" + $_.Exception.Message) | Add-Content -LiteralPath $summary -Encoding utf8
            if ($exitCode -eq 0) { $exitCode = 1 }
        }
    }
    if ($exitCode -eq 0) { 'result=PASS' | Add-Content -LiteralPath $summary -Encoding utf8 }
    else { 'result=FAIL' | Add-Content -LiteralPath $summary -Encoding utf8 }
    ("finished_at_utc=" + (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')) | Add-Content -LiteralPath $summary -Encoding utf8
    if (Test-Path -LiteralPath $summary) { Copy-Item -LiteralPath $summary -Destination (Join-Path $evidenceDir 'summary.txt') -Force }
    foreach ($file in @('checks.log','checks-summary.txt','integration.json','outage.json','sigterm.json','log-context.tsv','outage-runner.log','sigterm-app.log','compose-ps.txt','app.log','mysql.log','namesrv.log','broker.log','mock-target.log')) {
        $source = Join-Path $reportDir $file
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $evidenceDir $file) -Force }
    }
    Write-Host "run_id=$runId"
    Write-Host "report=$reportDir"
    Write-Host "evidence=$evidenceDir"
    Write-Host "exit_code=$exitCode"
}
exit $exitCode
