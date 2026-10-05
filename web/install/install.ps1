# Host agent installer for Windows (PRD F-11: one-command install).
#
#   & ([scriptblock]::Create((irm <server>/install.ps1))) -Server <server> -Token <token> -Region IN-SOUTH
#
# The agent connects to <server> as well. -Coordinator <url> names a different
# address for it, on installations that have one.
#
# Installs the agent to %USERPROFILE%\.ayeusann\bin and sets it up to run in
# the background and start again at every login. The token is single-use; after
# enrolling, the agent reconnects with its stored credential and remembered
# settings.
#
#   -Foreground   run in this window instead of in the background
#   -NoStart      install only
param(
    [string]$Server = "",
    [string]$Token = "",
    [string]$Coordinator = "",
    [string]$Region = "",
    [string]$Runtime = "",
    [switch]$NoStart,
    [switch]$Foreground
)

$ErrorActionPreference = "Stop"

function Say([string]$Message) { Write-Host "  $Message" }
function Fail([string]$Message) {
    Write-Host ""
    Write-Host "  error: $Message" -ForegroundColor Red
    Write-Host ""
    exit 1
}

# A machine being enrolled connects to the server it was installed from. One
# that is already enrolled keeps the address it remembers.
if (-not $Coordinator -and $Server -and $Token) { $Coordinator = $Server }

if (-not [Environment]::Is64BitOperatingSystem) { Fail "the agent needs 64-bit Windows." }
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { Fail "Windows on ARM is not supported yet." }

$InstallDir = Join-Path $env:USERPROFILE ".ayeusann\bin"
$Bin = Join-Path $InstallDir "ayeusann-agent.exe"
$State = Join-Path $env:USERPROFILE ".ayeusann\agent-state.json"

Write-Host ""
Write-Host "Installing the host agent (windows/amd64)"
Write-Host ""
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

# 1. The agent binary.
$Downloaded = $false
if ($Server) {
    $Tmp = "$Bin.tmp"
    try {
        # The progress bar slows Invoke-WebRequest down by an order of magnitude.
        $ProgressPreference = "SilentlyContinue"
        Invoke-WebRequest -UseBasicParsing -Uri "$Server/downloads/ayeusann-agent-windows-amd64.exe" -OutFile $Tmp
        Move-Item -Force $Tmp $Bin
        $Downloaded = $true
        Say "downloaded agent from $Server"
    } catch {
        Remove-Item -Force -ErrorAction SilentlyContinue $Tmp
    }
}
if (-not $Downloaded) {
    if (Test-Path $Bin) {
        Say "using the agent already installed at $Bin"
    } else {
        Fail "no Windows agent is published at $Server yet. Ask the operator to run 'make dist-agent-windows', then run this command again."
    }
}

# 2. The model runtime the agent drives.
if (-not $Runtime -or $Runtime -eq "ollama") {
    try {
        Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://127.0.0.1:11434/api/version" | Out-Null
        Say "Ollama is running"
    } catch {
        Say "warning: Ollama is not running. Install it from https://ollama.com/download and start it;"
        Say "         the machine connects now but receives jobs only once Ollama is up."
    }
}

# 3. NVIDIA driver floor (PRD F-11). The agent enforces this too.
$Smi = Get-Command nvidia-smi -ErrorAction SilentlyContinue
if ($Smi) {
    $Driver = (& nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>$null | Select-Object -First 1)
    if ($Driver) {
        $Major = [int]($Driver.Trim().Split(".")[0])
        if ($Major -lt 535) { Fail "NVIDIA driver $Driver is too old; install 535 or newer first." }
    }
} else {
    Say "warning: nvidia-smi was not found. The agent needs an NVIDIA GPU with its driver installed."
}

Say "installed to $Bin"
Write-Host ""

# Only what was asked for is passed on: anything left out keeps the value the
# agent remembered from its last enrolment.
$AgentArgs = @()
if ($Token) { $AgentArgs += @("--token", $Token) }
if ($Coordinator) { $AgentArgs += @("--coordinator", $Coordinator) }
if ($Region) { $AgentArgs += @("--region", $Region) }
if ($Runtime) { $AgentArgs += @("--runtime", $Runtime) }

if ($NoStart) {
    Write-Host "Run it in the background with:"
    Write-Host "  & `"$Bin`" service install $($AgentArgs -join ' ')"
    Write-Host ""
    exit 0
}
if (-not $Token -and -not (Test-Path $State)) {
    Fail "-Token is required for the first run; create one in the console under Hosts > Add a machine."
}

if (-not $Foreground) {
    & $Bin service install @AgentArgs
    if ($LASTEXITCODE -eq 0) { Write-Host ""; exit 0 }
    if ($LASTEXITCODE -ne 3) { exit $LASTEXITCODE }
    Write-Host ""
    Write-Host "Running it in this window instead."
}

Write-Host "Starting the agent. Keep this window open; press Ctrl+C to stop."
Write-Host "Start it again later with: & `"$Bin`""
Write-Host ""
& $Bin @AgentArgs
exit $LASTEXITCODE
