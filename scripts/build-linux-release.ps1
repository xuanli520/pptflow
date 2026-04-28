[CmdletBinding()]
param(
    [string]$Version = $env:VERSION,
    [string]$TargetArch = "amd64",
    [string]$DistDir = "dist",
    [string]$MainPackage = "./cmd/p2r",
    [string]$Commit = $env:COMMIT,
    [string]$BuildDate = $env:BUILD_DATE
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = "dev"
}
if ([string]::IsNullOrWhiteSpace($Commit)) {
    try {
        $Commit = (git rev-parse --short HEAD).Trim()
    } catch {
        $Commit = "unknown"
    }
}
if ([string]::IsNullOrWhiteSpace($BuildDate)) {
    $BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

$TargetOS = "linux"
$Package = "p2r_${Version}_${TargetOS}_${TargetArch}"
$RepoRoot = (Get-Location).Path
$DistAbs = [System.IO.Path]::GetFullPath((Join-Path $RepoRoot $DistDir))
$WorkDir = [System.IO.Path]::GetFullPath((Join-Path $DistAbs $Package))
$DistPrefix = $DistAbs.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
$Archive = Join-Path $DistAbs "$Package.tar.gz"
$Checksum = Join-Path $DistAbs "$Package.tar.gz.sha256"

if (-not $WorkDir.StartsWith($DistPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to clean unsafe release path: $WorkDir"
}

if (Test-Path -LiteralPath $WorkDir) {
    Remove-Item -LiteralPath $WorkDir -Recurse -Force
}
if (Test-Path -LiteralPath $Archive) {
    Remove-Item -LiteralPath $Archive -Force
}
if (Test-Path -LiteralPath $Checksum) {
    Remove-Item -LiteralPath $Checksum -Force
}
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null

$oldCgo = $env:CGO_ENABLED
$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldGoCache = $env:GOCACHE
$oldGoModCache = $env:GOMODCACHE
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = $TargetOS
    $env:GOARCH = $TargetArch
    if ([string]::IsNullOrWhiteSpace($env:GOCACHE)) {
        $env:GOCACHE = Join-Path $RepoRoot ".go-cache"
    }
    if ([string]::IsNullOrWhiteSpace($env:GOMODCACHE)) {
        $env:GOMODCACHE = Join-Path $RepoRoot ".gomodcache"
    }

    $ldflags = @(
        "-s -w",
        "-X github.com/xuanli520/p2r_tui/cmd.version=$Version",
        "-X github.com/xuanli520/p2r_tui/cmd.commit=$Commit",
        "-X github.com/xuanli520/p2r_tui/cmd.buildDate=$BuildDate"
    ) -join " "

    go build -trimpath -ldflags $ldflags -o (Join-Path $WorkDir "p2r") $MainPackage
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
} finally {
    $env:CGO_ENABLED = $oldCgo
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:GOCACHE = $oldGoCache
    $env:GOMODCACHE = $oldGoModCache
}

Copy-Item -LiteralPath "README.md" -Destination (Join-Path $WorkDir "README.md")
Copy-Item -LiteralPath "docs\linux-release.md" -Destination (Join-Path $WorkDir "INSTALL.md")
Copy-Item -LiteralPath ".p2r.yaml" -Destination (Join-Path $WorkDir "p2r.example.yaml")

tar -czf $Archive -C $DistAbs $Package
if ($LASTEXITCODE -ne 0) {
    throw "tar failed with exit code $LASTEXITCODE"
}

$Hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $Archive).Hash.ToLowerInvariant()
Set-Content -LiteralPath $Checksum -Value "$Hash  $Package.tar.gz" -NoNewline

Write-Output $Archive
