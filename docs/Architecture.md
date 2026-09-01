# Architecture

[Documentation](README.md)

## Components

The Wails desktop process owns the application lifecycle and exposes a narrow set of Go bindings to the React interface. The React frontend collects account settings, runs preflight checks, displays mappings and progress, and requests migrations or manual delta syncs.

The Go application separates protocol handling from migration state:

- IMAP and DAV clients communicate with source and destination servers.
- Mail and DAV migration services perform preflight checks, transfers, verification, retries, and reconciliation.
- SQLite stores migration state, mappings, verification results, conflicts, warnings, and report data.
- The operating system credential store is optional and holds passwords only when requested by the user.

## Main data flow

1. The user enters source and destination settings.
2. Preflight inspects both accounts and builds folder or collection mappings without migrating content.
3. The selected data is read from the source and written to the destination.
4. Mail is streamed rather than staged as mailbox files. Full verification compares source and destination SHA-256 hashes.
5. Progress and recovery state are committed locally. Reports are derived from that stored state.

Source accounts are read-only during normal migration. A manual delta sync inventories both sides and presents source deletions as explicit destination actions. Unsupported safe deletion operations stop without a broad fallback.

## DAV Alpha gate

CalDAV and CardDAV code, database tables, and migration logic remain available in the backend. The frontend hides both services until the user accepts the Alpha warning for the current process session. Removing that consent disables both services, restores Mail as active, clears DAV connection and preflight state, and excludes DAV settings and mappings from new analysis and start requests.

Existing DAV history and reports remain visible. A DAV or mixed delta sync requires renewed Alpha consent after each app start. Mail-only delta syncs do not.

## Update check

The frontend asks the Go binding for update information once during startup. The Go process caches the result, so repeated frontend calls do not repeat the HTTP request. The checker accepts `vX.Y.Z` and `X.Y.Z` tags, compares them as semantic versions, and only reports a newer published stable release.

## Reset lifecycle

Reset operations are owned by the Go process rather than the webview. The backend refuses a reset while a mail or DAV transfer is active, closes SQLite before removing state files, creates a fresh database with the current schema, and then replaces the migration services with instances bound to that database.

A migration-data reset keeps operating-system credentials. A factory reset additionally removes every credential registered under the application's keyring service and reloads the Wails application. If file removal fails, the backend attempts to reopen a usable database and returns a detailed error instead of leaving the reset silent.
