#!/usr/bin/env bash
set -euo pipefail

# generate-baseline.sh — Create baseline JSON files for integration tests.
#
# Usage: ./testdata/generate-baseline.sh <YEAR>
#
# Runs the integration test which saves current AniList output to
# internal/scheduler/testdata/ if no baseline exists yet. Delete the
# baseline files first to force regeneration.

YEAR="${1:?usage: $0 <YEAR>}"

cd "$(dirname "$0")/.." || exit 1

export INTEGRATION=1
export INTEGRATION_YEAR="$YEAR"
export UPDATE_BASELINE=1

# Clean existing baselines to force regeneration
BASEDIR="internal/scheduler/testdata"
rm -f "$BASEDIR"/baseline-*.json

go test -run TestIntegration_DataPipeline ./internal/scheduler/ -v

echo ""
echo "Baselines generated in $BASEDIR/"
ls -la "$BASEDIR"/baseline-*.json 2>/dev/null || echo "(no baseline files found)"
