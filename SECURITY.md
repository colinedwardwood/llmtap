# Security Policy

llmtap sits between an application and its LLM provider's API. Every
request that flows through it carries production credentials and the
prompts (often sensitive) the application sends. We take vulnerability
reports seriously and aim to keep llmtap a credible part of any
production trust boundary.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security findings.

Use GitHub's private vulnerability reporting:
**https://github.com/colinedwardwood/llmtap/security/advisories/new**

If that path isn't available, email `colin.wood@grafana.com` with
`[llmtap-security]` in the subject. Encrypt with the GitHub-published
SSH key on the maintainer's profile if the report contains exploit
detail.

We acknowledge every report within **72 hours** and aim for an
initial triage assessment within **7 days**.

## Coordinated disclosure window

Default window: **90 days** from acknowledgement, or sooner if a
fix ships and is broadly available. Reporters may request a shorter
window for active in-the-wild exploitation; we'll always negotiate
in good faith.

If we cannot ship a fix within 90 days we'll communicate the reason
and request an extension explicitly — never silently.

## In scope

- The `llmtap` binary and the container image at
  `ghcr.io/colinedwardwood/llmtap`.
- The release pipeline (`.github/workflows/release.yml`) and the
  CI pipeline (`.github/workflows/ci.yml`) including the signed
  artifacts they produce.
- The Go modules in this repo: `internal/*`, `cmd/llmtap`.

## Out of scope

- Vulnerabilities in third-party dependencies we transitively import
  — please report those to the upstream project first. We'll
  prioritize the dep bump on our side as soon as a fix is available.
- Configuration foot-guns that the proxy explicitly warns about
  (e.g. `LLMTAP_ALLOW_INSECURE=true` on a non-loopback bind, or
  `telemetry.acknowledge_insecure: true` — both opt-in declarations
  that the operator is accepting the trade-off).
- Denial-of-service from a configured upstream returning malicious
  bytes. The proxy has bounded buffers, body deadlines, concurrency
  caps, and circuit breakers (see `PLAN.md` A11–A13, A24), but a
  determined upstream can still consume operator-side resources;
  that's an upstream-trust question, not a vulnerability.

## What we'll do

1. Acknowledge the report (≤ 72h).
2. Triage and assign a severity (≤ 7d).
3. Develop a fix on a private branch.
4. Coordinate disclosure timing with the reporter.
5. Release a patched version, sign + attest it per the standard
   release pipeline (SLSA-3 provenance, CycloneDX SBOM, cosign).
6. Publish a security advisory on the GitHub repo crediting the
   reporter (unless they prefer anonymity).
7. Update `CHANGELOG.md` with the CVE reference and the upgrade
   path.

## Verification

Every release artifact is signed with cosign keyless via GitHub
OIDC. Verify a downloaded binary:

```bash
cosign verify-blob \
  --certificate llmtap-linux-amd64.pem \
  --signature   llmtap-linux-amd64.sig \
  --certificate-identity-regexp "^https://github\.com/colinedwardwood/llmtap/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  llmtap-linux-amd64
```

The container image at `ghcr.io/colinedwardwood/llmtap` is signed
the same way:

```bash
cosign verify \
  --certificate-identity-regexp "^https://github\.com/colinedwardwood/llmtap/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/colinedwardwood/llmtap:latest
```

SLSA-3 provenance and CycloneDX SBOMs are attached to each release.

## Thanks

We appreciate every responsible report and will credit reporters in
the advisory unless they prefer anonymity.
