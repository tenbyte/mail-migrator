# Development and operations

[Documentation](README.md)

## Requirements

- Go 1.27
- Node.js 22
- npm with lockfile support
- Wails 2.15 build prerequisites for the target platform
- Xcode Command Line Tools for macOS builds
- WebView2 and a supported C/C++ toolchain for Windows builds

## Commands

```bash
make dev
make dev-debug
make frontend
make test
make test-race
make lint
make audit
make sbom
make build-macos
make build-windows
```

`make dev-debug` enables detailed application diagnostics and Wails debug logging without recording account names, server names, folder names, subjects, or message content. On Windows, the equivalent PowerShell command is:

```powershell
$env:TENBYTE_LOG_LEVEL = "debug"
$env:GOTOOLCHAIN = "go1.27.0"
go run github.com/wailsapp/wails/v2/cmd/wails@v2.15.0 dev -devserver localhost:34116 -loglevel Debug -v 2 -nocolour
```

Diagnostic files are stored under `~/Library/Application Support/Tenbyte Mail Migrator/logs` on macOS and `%LOCALAPPDATA%\Tenbyte\Mail Migrator\logs` on Windows. Press `F12` in a Windows development build to open the WebView developer tools. The application also exposes **Copy diagnostics** and **Open log folder** under Advanced settings; a frontend crash shows the copy action directly in its fallback screen.

`npm ci` is used for reproducible frontend installs. The frontend is built before root Go tests because the compiled assets are embedded by `main.go`. Vulnerability scanning is limited to the root package and `internal/...`; it does not traverse `frontend/node_modules` as Go source.

## Continuous integration

CI runs on pushes to `main`, pull requests, and manual dispatch. It covers Go tests, race tests, vet, frontend tests, ESLint, the production frontend build, npm audit, govulncheck, CycloneDX SBOM generation, and macOS and Windows desktop builds.

## Dependency alerts

The repository intentionally uses Dependabot in alert-only mode. The dependency graph and Dependabot alerts remain enabled under GitHub's Security and quality settings, while Dependabot security updates are disabled. There is no `.github/dependabot.yml`, because version-update configuration would allow Dependabot to open pull requests.

Dependency changes are made manually. CI still runs `npm audit` and `govulncheck`, and GitHub lists vulnerable dependencies under Security and quality without creating branches, commits, or pull requests.

## Release procedure

1. Update `appVersion`, Wails product metadata, `frontend/package.json`, and `frontend/package-lock.json` to the same semantic version.
2. Update release notes, `README.md`, and any affected pages under `docs/`.
3. Run all verification and build commands listed above from a clean checkout.
4. Confirm that generated SBOM files and binaries are not staged.
5. Tag the verified commit as `vX.Y.Z` and push the tag. The release workflow verifies the version, builds Windows and macOS packages, creates SHA-256 checksums, includes `LICENSE` and `NOTICE`, and publishes a non-draft GitHub release.

For mailbox-notice releases, also verify with two disposable IMAP accounts before tagging: both switch combinations, plaintext and HTML rendering, custom-text escaping, the completed-with-errors warning, a retry after an uncertain APPEND without duplicates, and a later delta sync that neither transfers nor counts the source notice.

The application reads only GitHub's latest stable release endpoint. Drafts and prereleases do not produce an update notice.

The release packages are unsigned unless platform signing credentials are added to the workflow. Windows SmartScreen and macOS Gatekeeper may therefore warn users even when the checksum is correct. Do not describe a build as signed or notarized until the corresponding signing step is configured and verified.
