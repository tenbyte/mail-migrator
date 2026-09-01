# Security Policy

## Reporting a vulnerability

Report suspected vulnerabilities through [GitHub private vulnerability reporting](https://github.com/tenbyte/mail-migrator/security/advisories/new). Do not include credentials, live mailbox content, exported reports, support bundles, or migration databases in a public issue.

Include the affected version, operating system, a concise reproduction, and the expected impact. Use test accounts and redacted data where possible.

## Supported versions

Security fixes are provided for the latest published release. Older releases may be asked to upgrade before a report is investigated further.

## Update checks

The application contacts `https://api.github.com/repos/tenbyte/mail-migrator/releases/latest` once per process start with a five-second timeout. This request checks for a newer stable release and contains no mail credentials or device identifier. Failures are ignored, and the application never downloads or installs an update automatically.
