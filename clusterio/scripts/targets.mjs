// The platforms the binary packages are built for: npm's platform and arch, and Go's.
export const targets = [
	{ platform: "win32", arch: "x64", goos: "windows", goarch: "amd64" },
	{ platform: "linux", arch: "x64", goos: "linux", goarch: "amd64" },
	{ platform: "linux", arch: "arm64", goos: "linux", goarch: "arm64" },
	{ platform: "darwin", arch: "x64", goos: "darwin", goarch: "amd64" },
	{ platform: "darwin", arch: "arm64", goos: "darwin", goarch: "arm64" },
];

export const packageName = (t) => `@danielv123/timeshade-${t.platform}-${t.arch}`;
export const binaryName = (t) => (t.platform === "win32" ? "timeshadedb.exe" : "timeshadedb");
