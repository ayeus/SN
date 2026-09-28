# AyeusANN Host Agent Windows Installer (PowerShell)
Param(
    [string]$Token = $env:SN_REGISTRATION_TOKEN,
    [string]$Coordinator = $env:SN_COORDINATOR_URL
)

if (-not $Token) {
    Write-Host "ERROR: Registration token is required. Set `$env:SN_REGISTRATION_TOKEN or pass -Token parameter." -ForegroundColor Red
    exit 1
}

if (-not $Coordinator) {
    $Coordinator = "http://127.0.0.1:50051"
}

Write-Host "====================================================" -ForegroundColor Cyan
Write-Host "⚡ AyeusANN Host Agent Installer (Windows)" -ForegroundColor Cyan
Write-Host "====================================================" -ForegroundColor Cyan
Write-Host "Coordinator URL : $Coordinator" -ForegroundColor Yellow
Write-Host "Token           : $($Token.Substring(0, [Math]::Min(16, $Token.Length)))..." -ForegroundColor Yellow

$InstallDir = "$env:LOCALAPPDATA\AyeusANN\bin"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

Write-Host "[1/3] Verifying network connectivity to coordinator..." -ForegroundColor White
try {
    $resp = Invoke-WebRequest -Uri "$Coordinator/healthz" -UseBasicParsing -TimeoutSec 5 -ErrorAction SilentlyContinue
    Write-Host "  ✓ Coordinator reachable at $Coordinator" -ForegroundColor Green
} catch {
    Write-Host "  ! Warning: Unable to connect to coordinator at $Coordinator" -ForegroundColor Yellow
}

Write-Host "[2/3] Checking AyeusANN agent binary..." -ForegroundColor White
$agentBin = Join-Path $InstallDir "ayeusann-agent.exe"

if (Test-Path ".\agent\Cargo.toml") {
    Write-Host "  Building agent binary from source with cargo..." -ForegroundColor White
    cargo build --release --manifest-path .\agent\Cargo.toml
    if (Test-Path ".\agent\target\release\AyeusANN-agent.exe") {
        Copy-Item ".\agent\target\release\AyeusANN-agent.exe" -Destination $agentBin -Force
    }
}

Write-Host "[3/3] Installation completed successfully." -ForegroundColor Green
Write-Host ""
if (Test-Path $agentBin) {
    Write-Host "To launch your host agent now, run:" -ForegroundColor White
    Write-Host "  & `"$agentBin`" --coordinator-url $Coordinator --token $Token" -ForegroundColor Yellow
} else {
    Write-Host "To run the agent with cargo:" -ForegroundColor White
    Write-Host "  cargo run --manifest-path agent\Cargo.toml --release -- --coordinator-url $Coordinator --token $Token" -ForegroundColor Yellow
}
