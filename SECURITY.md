# Security policy

## Supported code and releases

Security fixes are developed on `main` and supported in the latest release. For binary installs, use a release that publishes `attestations.jsonl`; the installer verifies the exact repository, release workflow, tag, and hosted-runner provenance before it installs or executes bytes. Releases v0.3.2 and earlier predate this bundle and fail closed. This project does not claim that a currently published release is signed; inspect the release assets and verify them before use. The `main` branch is source under development, not a substitute for a verified release artifact.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting route for confidential reports:

<https://github.com/olafurns7/codator/security/advisories/new>

If that private route is unavailable, open a public issue only to request private contact:

<https://github.com/olafurns7/codator/issues/new?title=Please%20provide%20a%20private%20security%20contact>

Do not include vulnerability details, exploit code, credentials, tokens, or other secrets in that fallback issue. Wait for a private channel before sharing sensitive information.

## Trust limits

The installer checks GitHub artifact attestations against this repository and its release workflow, exact tag, source ref, GitHub issuer, and hosted-runner policy. That establishes release-workflow provenance for the downloaded bytes; it does not prove that the source, repository, maintainer account, GitHub, native CLIs, plugins, PATH, or the resulting code are uncompromised or safe. Profile separation is not OS isolation, and Codator does not replace provider authentication, authorization, or spending controls.
