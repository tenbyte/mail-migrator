# Changelog

## 0.7.0 - 2026-09-10

### Fixed

- Message verification now accepts byte-identical mail whose destination server only normalizes CRLF and LF line endings after APPEND. Other content changes still fail SHA-256 verification.
- Rechecking a previously failed verification recalculates and validates the current source content before approving the destination copy.
- Choosing **Leave skipped** now removes the resolved message from the actionable issue list instead of returning a legacy `null` action list that could crash the interface.
- The frontend also normalizes legacy `null` action lists defensively when loading existing migration data.

### Changed

- Automated GitHub release titles now put the version first, for example `v0.7.0 Tenbyte Mail Migrator`.

## 0.6.0 - 2026-09-09

### Fixed

- Legacy IMAP servers whose `RFC822.SIZE` differs from the `BODY[]` literal can now transfer messages safely using the literal size. The mismatch is recorded as a warning, while the migrated message is verified with SHA-256.
- APPENDLIMIT, quota checks, and duplicate protection now use the actual raw literal size instead of potentially incorrect source metadata.
- Incomplete raw literals continue to be quarantined.

## 0.4.0 - 2026-09-08

### Added

- Separate Mailbox notices settings page with independent source and destination opt-ins, plaintext custom text, a 10,000-character counter, live previews, explicit saving, and unsaved-change protection.
- Immutable notice settings per new IMAP migration and manual finalization after `COMPLETED` or confirmed `COMPLETED_WITH_ERRORS`.
- Safe plaintext/HTML cutover emails, independent delivery status and retries, persistent Message-IDs, and duplicate prevention across connection failures and restarts.
- Notice actions for completed migrations in Recent migrations and notice delivery details in exported reports.
- Delta-sync filtering so the generated source notice is never copied or counted.

### Changed

- Updated `actions/download-artifact` to 8, `modernc.org/sqlite` to 1.58.0, Echo to 4.15.4, Vitest to 5.0.0, `@types/react-dom` to 19.2.7, and ESLint to 10.10.0.
- Kept TypeScript at 6.0.3 until the supported typescript-eslint peer range includes TypeScript 7.
- Advanced reset documentation now distinguishes persistent notice settings from Factory Reset.

### Security

- Notice custom text is never accepted as HTML: special characters are escaped, line breaks are safely rendered, and generated HTML has no external images or tracking resources.
- Source mailboxes remain read-only during migration and delta sync. Only an explicitly confirmed notice cutover writes to the source `INBOX`.
