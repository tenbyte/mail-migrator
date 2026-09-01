# Data and security

[Documentation](README.md)

## Responsible use

Migration and cleanup actions can change data on connected accounts. You are responsible for the accounts, access rights, settings, backups, and actions you choose in the application. Review the selected source and destination and also this code repository carefully, keep an independent backup, and test with non-critical data first.

Tenbyte Mail Migrator is provided without warranties and must be used at your own risk. See the repository's [Apache License 2.0](../LICENSE) for the full warranty and liability terms.

## Local storage

Migration state is stored in `migrations.db` with owner-only permissions where supported.

- macOS: `~/Library/Application Support/Tenbyte Mail Migrator/migrations.db`
- Windows: `%LOCALAPPDATA%\Tenbyte\Mail Migrator\migrations.db`
- Other development platforms: the system configuration directory under `Tenbyte/Mail Migrator/migrations.db`

The database contains server names, account usernames, folder and collection mappings, message metadata, hashes, progress, warnings, conflicts, and report data. It does not store account passwords. Historical messages already stored by an older version are not translated or rewritten.

Before a schema upgrade, the application creates a one-time backup beside the database using the form `migrations.db.vN.bak`. WAL and shared-memory files may exist while the application is running.

## Credentials

Passwords are held in process memory for the active operation. If the user selects credential storage, the password is stored through the operating system credential store under service name `com.tenbyte.mail-migrator`. SQLite keeps only the credential reference.

Closing the application clears session-only destination credentials and DAV Alpha consent. Removing the database does not remove credentials from the operating system credential store.

## Reset and recovery controls

Advanced settings provides two explicit, confirmed reset operations:

- **Reset migration data** removes migration history, progress, mappings, delta-sync state, conflicts, warnings, and recovery records. Saved passwords and the current connection form remain available.
- **Reset entire app** removes the same migration data and every password stored by this application in the operating system credential store. The application reloads into its first-run state after a successful reset.

Both operations close SQLite before removing `migrations.db`, its `-wal` and `-shm` sidecars, and schema backups matching `migrations.db.vN.bak`. A fresh database with the current schema is created immediately. Resets are rejected while a transfer is active.

A reset never connects to source or destination servers and therefore cannot remove or modify remote mail, calendars, or contacts. Reports and support bundles previously exported to user-selected locations are outside the application data directory and are not deleted.

If a file cannot be removed, the application attempts to reopen a usable database and reports the failed step. If credential deletion fails after the migration database has already been reset, the error explicitly describes that partial result; running the full reset again retries removal of all credentials by application service name, even without the old database.

## Network destinations

The application connects only to:

- source and destination IMAP servers entered by the user;
- source and destination CalDAV or CardDAV servers entered by the user or found through the relevant `/.well-known/` endpoint;
- `https://api.github.com/repos/tenbyte/mail-migrator/releases/latest` once per process start for the stable release check.

The GitHub request has a five-second timeout, uses no token, and includes only a version-specific user agent. It contains no mailbox credentials or device identifier. A failed or invalid response is ignored. The release page button opens the fixed address `https://github.com/tenbyte/mail-migrator/releases/latest`. No update is downloaded or installed automatically.

## Reports and support bundles

Exports are written only after the user selects a destination. Reports and support bundles can contain server names, account identifiers, folder or collection names, message metadata, errors, and migration timing. They do not contain passwords or raw message bodies. Review exports before sharing them.

## Recovery

At startup, interrupted work is marked for safe recovery. A message with a known destination UID and hash is verified again. An uncertain APPEND is inventoried at the destination before any retry, which avoids creating a blind duplicate.

For backup or support work, close the application first and copy the database together with any `-wal` and `-shm` files that still exist. Restore the complete set to the same path while the application is closed.
