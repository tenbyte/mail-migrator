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
make frontend
make test
make test-race
make lint
make audit
make sbom
make build-macos
make build-windows
```

`npm ci` is used for reproducible frontend installs. The frontend is built before root Go tests because the compiled assets are embedded by `main.go`. Vulnerability scanning is limited to the root package and `internal/...`; it does not traverse `frontend/node_modules` as Go source.

## Continuous integration

CI runs on pushes to `main`, pull requests, and manual dispatch. It covers Go tests, race tests, vet, frontend tests, ESLint, the production frontend build, npm audit, govulncheck, CycloneDX SBOM generation, and macOS and Windows desktop builds.

Dependabot checks Go modules, frontend npm packages, and GitHub Actions weekly.

## Release procedure

1. Update `appVersion`, Wails product metadata, `frontend/package.json`, and `frontend/package-lock.json` to the same semantic version.
2. Update release notes, `README.md`, and any affected pages under `docs/`.
3. Run all verification and build commands listed above from a clean checkout.
4. Confirm that generated SBOM files and binaries are not staged.
5. Tag the verified commit as `vX.Y.Z` and push the tag. The release workflow verifies the version, builds Windows and macOS packages, creates SHA-256 checksums, includes `LICENSE` and `NOTICE`, and publishes a non-draft GitHub release.

The application reads only GitHub's latest stable release endpoint. Drafts and prereleases do not produce an update notice.

The release packages are unsigned unless platform signing credentials are added to the workflow. Windows SmartScreen and macOS Gatekeeper may therefore warn users even when the checksum is correct. Do not describe a build as signed or notarized until the corresponding signing step is configured and verified.
