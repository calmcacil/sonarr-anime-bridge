#!/usr/bin/env bash
# Compare native release/work binaries using shared historical inputs.
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 scripts/native_regression.py "$@"
