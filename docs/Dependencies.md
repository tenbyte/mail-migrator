# Dependencies

[Documentation](README.md)

Direct dependencies are pinned in `go.mod` and `frontend/package-lock.json`. Generated CycloneDX SBOM files provide the complete resolved dependency graph for a build.

## Go runtime and application

| Dependency | Version | Purpose | License |
| --- | --- | --- | --- |
| Go | 1.27 | Toolchain and standard library | BSD-3-Clause |
| Wails v2 | 2.15.0 | Desktop runtime and bindings | MIT |
| go-imap/v2 | 2.0.0-beta.8 | IMAP protocol | MIT |
| go-webdav | 0.7.0 | CalDAV and CardDAV transport | MIT |
| go-ical | `439c63cef608` | iCalendar processing | MIT |
| go-vcard | 0.1.0 | vCard processing | MIT |
| go-keyring | 0.2.8 | Operating system credential store | MIT |
| modernc.org/sqlite | 1.57.0 | Embedded SQLite database | BSD-3-Clause |
| golang.org/x/text | 0.41.0 | Text processing | BSD-3-Clause |
| golang.org/x/mod | 0.40.0 | Semantic version comparison | BSD-3-Clause |

## Frontend runtime

| Dependency | Version | Purpose | License |
| --- | --- | --- | --- |
| React and React DOM | 19.2.8 | User interface | MIT |
| Sonner | 2.0.8 | Notifications | MIT |

The frontend toolchain uses TypeScript 6.0.3, Vite 8.2.2, Vitest 4.1.11, ESLint 10.9.1, and typescript-eslint 8.69.0. TypeScript remains on 6.0 because TypeScript 7 is outside the supported typescript-eslint parser range.

Wails v2 remains the supported desktop framework. Wails v3 is a separate beta line and requires a deliberate application migration rather than a dependency-only update. See the [Wails v3 status](https://v3.wails.io/blog/).
