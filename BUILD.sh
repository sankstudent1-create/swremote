#!/bin/bash
# Canonical SWRemote Windows build. ALWAYS use -H=windowsgui or a console
# window opens next to the GUI (regression seen in v3.2.0).
set -e
export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$PATH"
VER=$(node -p "require('./version.json').version")
echo "building v$VER ..."
cd agent-go
go-winres make --in winres/winres.json --out rsrc_windows_amd64.syso >/dev/null
rm -f rsrc_windows_amd64.syso_windows_386.syso
mv -f rsrc_windows_amd64.syso_windows_amd64.syso rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui -X main.appVersion=$VER" -o SWRemote-Agent.exe .
cd ../service
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" -o SWRemote-Service.exe .
# v8.5: the installer embeds both exes — always pack the fresh builds
cp -f ../agent-go/SWRemote-Agent.exe ../installer/SWRemote-Agent.exe
cp -f SWRemote-Service.exe ../installer/SWRemote-Service.exe
cd ../installer
go-winres make --in winres/winres.json --out rsrc_windows_amd64.syso >/dev/null
rm -f rsrc_windows_amd64.syso_windows_386.syso
mv -f rsrc_windows_amd64.syso_windows_amd64.syso rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui -X main.appVersion=$VER" -o SWRemote-Setup.exe .
cd ..
echo "done: agent-go/SWRemote-Agent.exe installer/SWRemote-Setup.exe service/SWRemote-Service.exe"
