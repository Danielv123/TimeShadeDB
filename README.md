<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./web/images/logo_dark_transparent.png">
  <img alt="TimeShadeDB" src="./web/images/logo_light_transparent.png">
</picture>

# TimeShadeDB

TimeShadeDB stores timestamped canvas history and serves an embedded web interface for querying and replaying the canvas over time.

## Build

```powershell
powershell.exe -ExecutionPolicy Bypass -File scripts\build.ps1
```

The build script rebuilds the web bundle, compiles the Go binary with the current embedded assets, and writes a package archive under `build/packages/`.
