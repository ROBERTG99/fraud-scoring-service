# Security policy

## Supported versions

The project is pre-release: no version has been tagged. Only the latest `main` branch is supported, and
security fixes land there through pull requests.

| Version | Supported |
|---|---|
| `main` (pre-release) | Yes |
| Any other branch or fork | No |

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability**, which creates a private security advisory visible only to the project's
maintainers. Do not open a public issue, pull request or discussion for a vulnerability.

A useful report includes:

- the component and file (for example `internal/api`, `src/fraud_summarizer/redact.py`)
- the commit you tested
- steps or a test that reproduces the problem
- what an attacker gains

Reports are handled in the advisory. Once a fix is on `main`, the advisory can be published with credit to
the reporter if they want it.

## Data

All data in this repository is synthetic, produced by the generator in `cmd/gen` with a fixed seed. No real
cardholder, account or personal data is used, stored or needed, and none should ever be added to an issue,
advisory, test or fixture. Planted test values such as `4111` test card numbers, `555` phone numbers and
`example.com` e-mail addresses are synthetic too.

## Threat model and controls

[`docs/security.md`](docs/security.md) describes the trust boundaries, a STRIDE analysis per component, the
OWASP Top 10 for LLM applications, the results of `make security`, and the accepted and open findings. The
findings of the security audit and where each was fixed are listed in
[`docs/DECISIONS.md`](docs/DECISIONS.md) (D-041 to D-069). Known gaps are in
[`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md).
