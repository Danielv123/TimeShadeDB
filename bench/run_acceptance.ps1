# Runs the acceptance suite (or, with -mode baseline, the baselines) and
# writes bench/REPORT.md. All options are flags of bench/cmd/acceptance; see
# bench/README.md. Example:
#   $env:TSDB_TESTDATA = "G:\data\minimap"
#   .\bench\run_acceptance.ps1
#   $env:TSDB_MASTER_BIN = "C:\tmp\tsdb-master.exe"; .\bench\run_acceptance.ps1 -mode baseline
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    & go run ./bench/cmd/acceptance -repo $repo @args
    exit $LASTEXITCODE
} finally {
    Pop-Location
}
