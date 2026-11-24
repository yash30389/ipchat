$ErrorActionPreference = 'Stop'

if (-not (Test-Path '.\\go.mod')) {
    throw 'Run this script from the IPChat project root (the folder containing go.mod).'
}

Write-Host 'Generating go.sum and verifying pinned Go dependencies...' -ForegroundColor Cyan
docker run --rm -v "${PWD}:/app" -w /app golang:1.23-alpine sh -c 'go mod tidy && go mod verify'
if ($LASTEXITCODE -ne 0) {
    throw 'Dependency resolution failed. Check Docker network access to proxy.golang.org and try again.'
}

if (-not (Test-Path '.\\go.sum') -or (Get-Item '.\\go.sum').Length -eq 0) {
    throw 'go.sum was not generated or is empty.'
}

Write-Host 'go.sum generated successfully. Keep go.mod and go.sum committed together.' -ForegroundColor Green
