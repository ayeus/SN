# Windows hosts run the agent inside WSL2 (PRD F-11: Linux at launch,
# Windows/WSL2 in Phase 2). This script only explains how.
Write-Host ""
Write-Host "The host agent runs inside WSL2 on Windows." -ForegroundColor Cyan
Write-Host ""
Write-Host "  1. Install WSL2 and Ubuntu:   wsl --install -d Ubuntu"
Write-Host "  2. Install the NVIDIA driver for WSL (R535 or newer) on Windows."
Write-Host "  3. In the console, open Hosts > Add a machine and copy the 'Windows (WSL)' command."
Write-Host ""
