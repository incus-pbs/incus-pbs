#!/bin/sh
# Generate shell completion scripts for packaging (goreleaser before-hook).
set -eu
cd "$(dirname "$0")/.."
mkdir -p completions
go run ./cmd/incus-pbs completion bash > completions/incus-pbs.bash
go run ./cmd/incus-pbs completion zsh > completions/_incus-pbs
