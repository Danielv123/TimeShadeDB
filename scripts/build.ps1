param(
    [string]$OutputDir = "build",
    [string]$PackageName = "",
    [switch]$SkipPackage
)

$ErrorActionPreference = "Stop"

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$outputRoot = Join-Path $repoRoot $OutputDir
$packageDir = Join-Path $outputRoot "packages"
$modBuildDir = Join-Path $outputRoot "mods"
$modSourceDir = Join-Path $repoRoot "timeshadedb_exporter"

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
$modInfo = Get-Content (Join-Path $modSourceDir "info.json") -Raw | ConvertFrom-Json
$modFolderName = "$($modInfo.name)_$($modInfo.version)"
$modStageDir = Join-Path $modBuildDir $modFolderName
$modPackagePath = Join-Path $packageDir "$modFolderName.zip"

if ($PackageName -eq "") {
    $PackageName = "timeshadedb-$goos-$goarch"
}
$packagePath = Join-Path $packageDir "$PackageName.zip"

New-Item -ItemType Directory -Force -Path $packageDir | Out-Null
New-Item -ItemType Directory -Force -Path $modBuildDir | Out-Null

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

Write-Host "Packaging Factorio mod..."
if (Test-Path $modStageDir) {
    Remove-Item -LiteralPath $modStageDir -Recurse -Force
}
if (Test-Path $modPackagePath) {
    Remove-Item -LiteralPath $modPackagePath -Force
}
New-Item -ItemType Directory -Force -Path $modStageDir | Out-Null
Copy-Item -Path (Join-Path $modSourceDir "*") -Destination $modStageDir -Recurse
Compress-Archive -LiteralPath $modStageDir -DestinationPath $modPackagePath

if (-not $SkipPackage) {
    if (Test-Path $packagePath) {
        Remove-Item -LiteralPath $packagePath -Force
    }
    Write-Host "Packaging $packagePath..."
    Compress-Archive -LiteralPath $binaryPath, $modPackagePath -DestinationPath $packagePath
}

Write-Host "Built $binaryPath"
Write-Host "Packaged Factorio mod $modPackagePath"
if (-not $SkipPackage) {
    Write-Host "Packaged $packagePath"
}
