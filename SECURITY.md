# Security policy

pg-dac connects to databases with real credentials and can change their schema, so security
reports are taken seriously.

## Supported versions

Fixes are made on `main` and shipped in the next release. Only the latest release receives
security fixes; the desktop app updates itself to it.

## Reporting a vulnerability

Please **do not open a public issue**. Report it privately through GitHub instead:
[Report a vulnerability](https://github.com/rmnunes/pg-dac/security/advisories/new) (the
repository's *Security* tab → *Report a vulnerability*).

Include what you found, how to reproduce it, the pg-dac version and what an attacker could do with
it. You can expect an acknowledgement within a week. Once a fix is released, the advisory is
published with credit to you, unless you prefer to stay anonymous.

## What is in scope

For example:

- A password or other secret written to output, logs, errors, saved files or an artifact.
- Bypassing the signature check of a `.pgdac` artifact or `.pgbac` archive, or `--require-key` /
  `trusted_keys`.
- Generated SQL that runs something the source schema did not define (injection through object
  names, comments or definitions).
- Reaching the local `pg-dac serve` daemon without its token, or from outside the machine.
- Installing a desktop update that was not signed with the project's key.

Problems that need the attacker to already control your source schema, project files or the
target database are generally not vulnerabilities: pg-dac deploys what you give it. Data loss that
pg-dac reported as a hazard and you acknowledged is also expected behavior, but a destructive
change that was **not** flagged is a bug worth reporting.
