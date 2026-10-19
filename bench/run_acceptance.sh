#!/usr/bin/env sh
# Runs the WP11 acceptance suite (or, with -mode baseline, the Wave 1 baselines) and
# writes bench/REPORT.md. All options are flags of bench/cmd/acceptance; see
# bench/README.md. Example:
#   TSDB_TESTDATA=/data/minimap TSDB_STREAM=/data/stream60 bench/run_acceptance.sh
#   TSDB_MASTER_BIN=/tmp/tsdb-master bench/run_acceptance.sh -mode baseline
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(dirname "$here")
cd "$repo"
exec go run ./bench/cmd/acceptance -repo "$repo" "$@"
