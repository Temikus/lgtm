# Security Policy

## Supported versions

LGTM is pre-1.0. Only the latest release gets security fixes.

| Version  | Supported          |
|----------|--------------------|
| latest   | :white_check_mark: |
| < latest | Best-effort        |

## Reporting a vulnerability

**Please don't open a public issue for security vulnerabilities.**

Use [GitHub Private Vulnerability Reporting](https://github.com/Temikus/lgtm/security/advisories/new). This keeps the details private until a fix is out.

Please include:

- what the vulnerability is and what it lets an attacker do;
- steps to reproduce, or a proof of concept;
- the affected version and component (CLI, Action, website);
- a suggested fix, if you have one.

You'll get an acknowledgement within 48 hours and a status update within 7 days. Disclosure is coordinated after the fix ships, with credit unless you'd rather stay anonymous.

## Scope

LGTM is a mistake detector, not a security boundary. Its inputs are text the PR author controls, so a determined author can steer its scores. These are **not** vulnerabilities on their own:

- prompt injection that moves a score, when the verdict still follows the documented rules (fork cap, `injection` cap, partial view never approves);
- a bad score from the model.

These **are** in scope:

- anything that lets a PR change its own rubric, thresholds, mode, or provider;
- anything that makes LGTM approve on a failure, a partial view, or an untrusted fork PR;
- leaking the model API key or `GITHUB_TOKEN`, or sending repo content anywhere except the configured provider;
- flipping the check to success without a qualifying human approval on the scored commit;
- markdown or HTML injection through the sticky comment or check run;
- the getlgtm.dev waitlist leaking or enumerating emails.

## Supply chain

- Release binaries are signed with [cosign](https://github.com/sigstore/cosign) (keyless OIDC), and each release has an SBOM.
- The Action runs a GHCR image pinned by digest, with build provenance attestations.
- Renovate pins workflow actions by SHA.
