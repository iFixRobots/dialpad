#!/bin/sh
cd cmd/dialpad-bridge
go build -ldflags "-X main.Tag=$(git describe --tags --exact-match 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=$(date -Iseconds)'" -o ../../dialpad-bridge "$@"
