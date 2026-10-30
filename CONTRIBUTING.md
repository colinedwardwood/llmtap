# Contributing to llmtap

Thanks for considering a contribution. llmtap sits on a production
trust boundary — it sees credentials and prompts — so the bar for
correctness and test coverage is deliberately high, but the codebase
is small and the workflow is simple.

## Prerequisites

- Go 1.26+
- `make` and a POSIX shell
- Optional: `golangci-lint` (the `lint` target falls back to `go vet`
  without it), Docker for the compose demo stack

## Getting started

```bash
git clone https://github.com/colinedwardwood/llmtap
cd llmtap
make test      # go test -race ./...
make run       # builds and starts against config.example.yaml
```

`llmtap check --config <file>` validates a config without starting
the server — useful while iterating.

## Making changes

1. Fork and branch from `main`.
2. Keep changes focused; one logical change per PR.
3. Add or update tests. Bug fixes need a regression test that fails
   without the fix. `-race` is always on in CI.
4. Run the full gate locally before pushing:

   ```bash
   make lint test build
   ```

5. Update docs that your change makes stale: `README.md`,
   `config.example.yaml` comments, and `CHANGELOG.md` (add an entry
   under **Unreleased**).

## Commit messages

Follow the existing convention:

```
type(scope): imperative summary

fix(proxy): release breaker probe slot on passthrough exit
feat(config): add LLMTAP_SAMPLE_RATIO env override
docs: correct PromQL examples for cost metrics
```

Types in use: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`,
`chore`, `ci`.

## What reviewers look for

- **Bounded everything.** Buffers, retries, goroutines, and attribute
  sizes all need explicit ceilings. Unbounded growth on a hostile
  input is a rejected review, not a nit.
- **No content leaks.** Prompt/completion bytes must never reach a
  span, log line, or error message unless content capture is on, and
  then only after redaction. Redact before truncating.
- **Passthrough fidelity.** The proxy forwards bytes it doesn't
  understand rather than failing the request. Enrichment is
  best-effort; the data path is not.
- **Honest failure classification.** Client-caused failures (cancel,
  oversized body) must not be attributed to the upstream.

## Security issues

Do **not** open a public issue or PR for a vulnerability. See
[SECURITY.md](SECURITY.md) for the private reporting path.

## Releases

Maintainers cut releases by pushing a `v*` tag. The release pipeline
produces signed binaries, a signed container image, SLSA-3 provenance,
SBOMs, and a checksums file; nothing is published if provenance
generation fails.
