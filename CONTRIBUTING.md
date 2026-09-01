# Contributing

Contributions are welcome through GitHub issues and pull requests.

Before opening a pull request:

1. Keep changes focused and explain any user-visible behavior.
2. Add or update tests for changed behavior.
3. Run `make test`, `make test-race`, `make lint`, and `make audit`.
4. Do not commit credentials, migration databases, exported reports, support bundles, generated binaries, or generated SBOM files.

Use clear commit messages and keep machine-readable status values, error codes, JSON fields, and the database schema stable unless the change explicitly requires a compatibility update.

Security reports do not belong in public issues. Follow [SECURITY.md](https://github.com/tenbyte/mail-migrator/blob/main/SECURITY.md) instead.
