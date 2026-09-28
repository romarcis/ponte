#!/bin/sh
# Builds Ponte for Windows and Linux into dist/.
# VERSION (e.g. 1.2.0) is shown in the app; it defaults to "dev".
set -e
cd "$(dirname "$0")"
mkdir -p dist
V="-X main.version=${VERSION:-dev}"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui $V" -o dist/Ponte.exe .
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-s -w -H windowsgui $V" -o dist/Ponte-arm64.exe .
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w $V" -o dist/ponte-linux-x64 .
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w $V" -o dist/ponte-linux-arm64 .
ls -lh dist
