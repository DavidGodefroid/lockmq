# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub security advisories:
open <https://github.com/DavidGodefroid/lockmq/security/advisories/new>
(the "Report a vulnerability" button on the repository's Security tab).

Do not open a public issue, pull request or discussion for a suspected
vulnerability.

Please include:

- the lockmq version (`lockmq version`) and your OS;
- the RabbitMQ version, if relevant;
- the steps or input needed to reproduce the issue, and its impact.

Never include real credentials or real personal data in a report.

You will get an acknowledgement within a week. Fixes are released as soon as
possible, and the advisory is published with credit to the reporter unless
you ask otherwise.

## Scope

The threat model is in [docs/security-model.md](docs/security-model.md).
Issues of interest include, among others:

- a way for a client (CLI or MCP) to read message content without the
  human's approval, to get a queue peek auto-approved, or to loosen the
  policy without confirmation;
- a broker call outside the operation allowlist, or any call that changes
  broker state beyond the documented requeue of a peek;
- a stored stream offset, or a message dead-lettered by a peek the console
  should have refused;
- a secret or a URI password reaching argv, the environment, a file, a log,
  the socket or an error message;
- unmasked personal data in a result where a rule or detector should apply;
- client- or broker-controlled text that can spoof the console's approval
  screen.

Attacks that require running code as the same OS user as the console are out
of scope, as documented in the threat model.

## Supported versions

Only the latest release receives security fixes.
