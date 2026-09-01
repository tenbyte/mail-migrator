<p align="center">
  <img src="branding/readme-banner.webp" alt="Tenbyte Mail Migrator with the courier mascot and application interface" width="960">
</p>

# Tenbyte Mail Migrator

[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8.svg)](https://go.dev/doc/go1.27)

Tenbyte Mail Migrator is a local desktop application for controlled, verifiable mailbox migrations. Data is transferred directly between the configured accounts; there is no hosted migration backend.

## Feature status

| Service | Protocol | Status |
| --- | --- | --- |
| Mail | IMAP | Supported |
| Calendar | CalDAV | Experimental Alpha |
| Contacts | CardDAV | Experimental Alpha |

CalDAV and CardDAV appear only after a session-only opt-in under Advanced settings. Do not use the Alpha services for critical migrations.

## Highlights

- Streams mail directly from source to destination without staging mailbox files.
- Verifies transferred mail with full SHA-256 comparisons.
- Stores progress locally for interruption recovery and manual delta syncs.
- Keeps passwords in memory unless the operating system credential store is explicitly enabled.
- Exports migration reports and privacy-conscious support bundles on demand.
- Provides separate migration-data and full factory resets under Advanced settings.

Version 0.3.0 supports macOS 13 or later and Windows 10 or later. The project uses Go 1.27, Node.js 22, React, and Wails 2.15.

## Responsible use

Migration and cleanup actions can change data on connected accounts. You are responsible for the accounts, permissions, settings, backups, and actions you choose in this application. Check your configuration, keep an independent backup, and test with non-critical data first.

The software is provided without warranties. Use it at your own risk. The [Apache License 2.0](LICENSE) contains the full warranty and liability terms.

## Build and development

Install Go 1.27 and Node.js 22. macOS builds require Xcode Command Line Tools. Windows builds require WebView2 and a supported C/C++ toolchain.

```bash
make frontend
make test
make test-race
make lint
make audit
make sbom
make build-macos
make build-windows
```

`make dev` starts the Wails development environment. Desktop binaries are written to `build/bin`. CycloneDX SBOM files are generated under `build/compliance` and are not committed.

Pushing a matching version tag such as `v0.3.0` runs the release workflow. It builds a Windows executable and a universal macOS app, packages them with SHA-256 checksums, and publishes the files on the corresponding GitHub release. Generated binaries and release packages remain ignored by Git.

## Security and local data

Migration state and reports are stored in a local SQLite database. At startup, the application makes one unauthenticated request to GitHub's public release API to check for a newer stable version. The request contains no account credentials or device identifier, and updates are never downloaded or installed automatically.

See [Data and security](docs/Data-and-security.md) for storage locations, network destinations, recovery, exports, and reset behavior. Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/tenbyte/mail-migrator/security/advisories/new).

## Documentation

- [Documentation overview](docs/README.md)
- [Architecture](docs/Architecture.md)
- [Data and security](docs/Data-and-security.md)
- [Development and operations](docs/Development-and-operations.md)
- [Dependencies](docs/Dependencies.md)

## License

Copyright 2026 Tenbyte Technologies GmbH. The source code and documentation are licensed under the [Apache License 2.0](LICENSE).

### Artwork

The AI-assisted courier artwork is an adaptation inspired by the Go Gopher by Renee French and is distributed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). It is unofficial and is not endorsed by the Go project or Google. The asset list, source, attribution, and modifications are recorded in [NOTICE](NOTICE).
