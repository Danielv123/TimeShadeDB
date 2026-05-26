param(
    [string]$OutputDir = "build",
    [string]$PackageName = "",
    [switch]$SkipPackage
)

$ErrorActionPreference = "Stop"

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$outputRoot = Join-Path $repoRoot $OutputDir
$packageDir = Join-Path $outputRoot "packages"

$npm = Get-Command npm.cmd -ErrorAction SilentlyContinue
if (-not $npm) {
    $npm = Get-Command npm -ErrorAction Stop
}

$goos = (& go env GOOS).Trim()
$goarch = (& go env GOARCH).Trim()
$exeSuffix = ""
if ($goos -eq "windows") {
    $exeSuffix = ".exe"
}

$binaryName = "timeshadedb$exeSuffix"
$binaryPath = Join-Path $repoRoot $binaryName

if ($PackageName -eq "") {
    $PackageName = "timeshadedb-$goos-$goarch"
}
$packagePath = Join-Path $packageDir "$PackageName.zip"

New-Item -ItemType Directory -Force -Path $packageDir | Out-Null

Write-Host "Building web assets..."
Push-Location (Join-Path $repoRoot "web")
try {
    & $npm.Source run build
}
finally {
    Pop-Location
}

Write-Host "Building Go binary..."
Push-Location $repoRoot
try {
    & go build -trimpath -o $binaryPath ./cmd/timeshadedb
}
finally {
    Pop-Location
}

if (-not $SkipPackage) {
    if (Test-Path $packagePath) {
        Remove-Item -LiteralPath $packagePath -Force
    }
    Write-Host "Packaging $packagePath..."
    Compress-Archive -LiteralPath $binaryPath -DestinationPath $packagePath
}

Write-Host "Built $binaryPath"
if (-not $SkipPackage) {
    Write-Host "Packaged $packagePath"
}
