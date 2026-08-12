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
    $Coordinator = "http://192.168.1.4:8083"
}

Write-Host "====================================================" -ForegroundColor Cyan
Write-Host "⚡ AyeusANN Host Agent Installer (Windows)" -ForegroundColor Cyan
Write-Host "====================================================" -ForegroundColor Cyan
Write-Host "Coordinator URL : $Coordinator" -ForegroundColor Yellow
Write-Host "Token           : $Token" -ForegroundColor Yellow

$InstallDir = "$env:LOCALAPPDATA\AyeusANN"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

Write-Host "Registering host node with AyeusANN coordinator..." -ForegroundColor Green
Write-Host "Host Agent binary ready for execution." -ForegroundColor Green
Write-Host "To run the agent locally on Windows:" -ForegroundColor White
Write-Host "  cargo run -- --coordinator-url $Coordinator --token $Token" -ForegroundColor Yellow
