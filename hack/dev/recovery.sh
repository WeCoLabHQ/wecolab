#!/usr/bin/env bash
# Real-stack disposable recovery drills; all mutations are limited to the labelled fabric.
set -euo pipefail
cd "$(dirname "$0")/../.."
exec python3 hack/dev/recovery_runner.py "$@"
