@echo off
REM SWRemote Windows Agent installer
REM Run this file on the Windows PC you want to control remotely.
title SWRemote Agent Setup
echo ============================================
echo   SWRemote Agent Setup  (by SWInfoSystems)
echo ============================================
echo.

where python >nul 2>nul
if %errorlevel% neq 0 (
  echo [1/3] Python not found. Opening python.org download page...
  echo       Install Python 3.10+ and TICK "Add python.exe to PATH", then re-run this file.
  start https://www.python.org/downloads/
  pause
  exit /b 1
)

echo [1/3] Installing required libraries (one-time, needs internet)...
python -m pip install --upgrade pip >nul
python -m pip install -r "%~dp0requirements.txt"
if %errorlevel% neq 0 (
  echo Failed to install libraries. Check your internet and try again.
  pause
  exit /b 1
)

echo.
echo [2/3] Configuration
set /p SERVER="Relay server address (ENTER for test default http://localhost:8080): "
if "%SERVER%"=="" set SERVER=http://localhost:8080
set /p UPIN="Set an access PIN for this PC (min 6 digits): "
set "SRV=%SERVER%"

powershell -NoProfile -Command "$s=$env:SRV -replace '^http','ws'; if($s -notmatch '/ws$'){$s+='/ws'}; @{server=$s; name=$env:COMPUTERNAME; pin=$env:UPIN} | ConvertTo-Json | Out-File -Encoding utf8 '%~dp0config.json'"

echo       Saved. Your device ID is generated on first run.
echo.
echo [3/3] Starting SWRemote agent...
echo       Keep this window open while you want the PC to be reachable.
echo       Your SWRemote ID and PIN will be printed below.
echo.
python "%~dp0agent.py"
pause
