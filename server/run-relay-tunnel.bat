@echo off
REM =====================================================================
REM  SWRemote - relay server + console online in one double-click (FREE)
REM  No credit card, no accounts, no sign-ups.
REM  Run this on the Windows PC that will host the relay server.
REM =====================================================================
setlocal
cd /d "%~dp0"

echo.
echo  === SWRemote relay setup ===
echo.

echo [1/4] Checking Node.js...
where node >nul 2>nul
if errorlevel 1 (
  echo.
  echo  Node.js is NOT installed on this PC.
  echo  Get the free LTS installer from the Node.js website,
  echo  then run this file again.
  echo.
  pause
  exit /b 1
)
echo  OK - Node.js found.

echo [2/4] Installing relay files (first run only)...
if not exist node_modules (
  call npm install
) else (
  echo  Already installed, skipping.
)

echo [3/4] Starting relay server in a background window...
start "SWRemote Relay" /min cmd /c "npm start"
timeout /t 4 >nul

echo [4/4] Opening a free public tunnel (Cloudflare, no account needed)...
where cloudflared >nul 2>nul
if not errorlevel 1 (
  set "CLOUDFLARED=cloudflared"
) else if exist cloudflared.exe (
  set "CLOUDFLARED=%~dp0cloudflared.exe"
) else (
  echo.
  echo  cloudflared is not on this PC yet.
  echo  Download the Windows build (cloudflared-windows-amd64.exe) from the
  echo  Cloudflare downloads page, rename it to cloudflared.exe, drop it
  echo  next to this file, and run this file again.
  echo.
  pause
  exit /b 1
)

echo.
echo  =====================================================================
echo   COPY the https://xxxx.trycloudflare.com address printed below, then:
echo.
echo   1. On each PC you want to control: next to SWRemote-Agent.exe, open
echo      swremote.json in Notepad and set
echo        "server": "wss://xxxx.trycloudflare.com/ws"
echo      (replace xxxx with YOUR address; note wss, not https)
echo.
echo   2. On your phone: open https://xxxx.trycloudflare.com and tap
echo      "Add to Home Screen" - the SWRemote console installs as an app.
echo.
echo   The address changes each time you re-run this file - that is normal.
echo   Keep THIS window OPEN while you want remote access. Closing it takes
echo   the relay (and the console) offline.
echo  =====================================================================
echo.
"%CLOUDFLARED%" tunnel --url http://localhost:8080
echo.
echo  Tunnel closed. The relay server keeps running in its own window.
pause
