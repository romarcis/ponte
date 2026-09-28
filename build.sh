#!/bin/sh
# Builds Ponte for Windows and Linux into dist/.
set -e
cd "$(dirname "$0")"
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/Ponte.exe .
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/Ponte-arm64.exe .
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/ponte-linux-x64 .
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/ponte-linux-arm64 .
ls -lh dist
