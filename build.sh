#!/bin/sh
# Builds tclaude and tcodex into bin/ next to this script.
set -eu
cd "$(dirname "$0")"
go build -o bin/tclaude ./cmd/tclaude
go build -o bin/tcodex ./cmd/tcodex
