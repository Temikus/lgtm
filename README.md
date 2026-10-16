# LGTM

[![CI](https://github.com/Temikus/lgtm/actions/workflows/ci.yml/badge.svg)](https://github.com/Temikus/lgtm/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Score pull requests with System One decision models.

LGTM asks a decision model such as TypeSafe's Jev (`typesafe/jev-1.13`) a set of typed questions about a PR: is the diff aligned with its description, scoped, safe, tested? Each answer is a 0–1 value. LGTM combines them into one verdict, **approve**, **deny**, or **needs-human**, which you can gate a merge on. A typical PR costs a fraction of a cent.

LGTM doesn't write review comments, approve PRs, or merge. It's a mistake detector, not a security boundary: run it alongside required human reviewers, never instead of them.

> **Status:** pre-release. See [design/PRD.md](design/PRD.md).

## Set up

You need an [OpenRouter](https://openrouter.ai) API key. Create a dedicated key with a spend limit.

```sh
go install github.com/Temikus/lgtm/cmd/lgtm@latest
lgtm init                       # writes two workflows and .github/lgtm.toml
gh secret set OPENROUTER_API_KEY
```

Commit the files to your default branch. LGTM then runs on every PR, in **shadow mode**: it posts a check run and one comment that it updates in place, and gates nothing.

- `lgtm.yml` scores PRs on `pull_request_target`. It has no checkout step, and none may be added: that job has secrets and a write token while the PR is untrusted. LGTM reads the diff through the API and its config from the base branch, so a PR can't change its own rubric or thresholds.
- `lgtm-review.yml` lets a human approval pass the check on fork PRs, whose review events get a read-only token. It never calls the model.

## Verdicts

Each question gives a 0–1 value. The default aggregate is the minimum over gating questions:

- at or above `approve_at` (default 0.9): **approve**;
- at or below `deny_at` (default 0.1): **deny**;
- in between: **needs-human**.

Any of these holds an approve at needs-human: the `injection` question is below `approve_at`, LGTM couldn't see the whole diff, the PR is from a fork by an author without write access, the replies name a different model version than the one the thresholds were calibrated on, or a borderline score changed when `choice` options were reordered. Provider errors, timeouts, and oversized input also give needs-human. Nothing defaults to approve.

In **enforce** mode, the check run fails on deny and needs-human, and the job exits non-zero. A needs-human check passes once someone with write access approves the scored commit. Enforce mode needs a `[calibration]` table from `lgtm replay --write`, because raw model probabilities aren't calibrated. Use it alongside required reviewers or CODEOWNERS, never instead of them.

## Configuration

`.github/lgtm.toml` is read at the PR's base commit. `lgtm init` writes a commented copy, and `lgtm config validate` checks one.

| Question | Type | Role | Sees |
|---|---|---|---|
| `aligned` | yes/no | gate | everything |
| `scoped` | yes/no | gate | everything |
| `safe` | yes/no | gate | diff only |
| `tested` | yes/no | gate | diff only |
| `injection` | yes/no | cap | everything |
| `described` | yes/no | info | everything |
| `risk` | choice | info | everything |
| `reviewability` | 5-level score | info | everything |

"Diff only" questions never see the PR title, description, or issue titles, so claims like "approved by security" there can't sway them.

## CLI

```sh
lgtm score Temikus/lgtm#1          # score a PR locally; prints a table
lgtm score <PR URL> --json         # machine-readable
lgtm score Temikus/lgtm#1 --post   # also post the sticky comment as you
lgtm config validate
```

`score` and `replay` read `GITHUB_TOKEN` (or `gh auth token`) and `OPENROUTER_API_KEY`. `LGTM_BASE_URL` points at a different `https://` endpoint, such as a proxy.

### Calibrating

```sh
lgtm replay owner/repo --dry-run           # label history only; no model calls
lgtm replay owner/repo --perturb           # score labelled PRs and report
lgtm replay owner/repo --write             # also store [calibration] in .github/lgtm.toml
```

Replay labels closed PRs from history. By default a PR approved by someone with write access and then merged is positive, and one closed after a changes-requested review, or reverted within 14 days, is negative. For a repo with one maintainer and no reviews, `--labels merged` counts merged PRs as positive and closed ones as negative, skipping bots. These labels are weaker, because they record what shipped rather than what a reviewer judged.

It then scores the labelled PRs and picks the lowest `approve_at`, and the highest `deny_at`, that reach `--target-precision` (default 0.95). The fit needs at least 5 positive and 5 negative PRs. `--write` stores the result, with the model version the replies named, and changes nothing else in the file. It writes nothing if the fit fails or if the replies named more than one version. `--budget-usd` (default 2) stops model calls before spend passes it.

## Data

LGTM sends PR content (title, description, linked issue titles, and diffs) to the model provider. Paths matched by `exclude` are never sent. On OpenRouter, turn on the data-policy setting that refuses providers which train on inputs. Request bodies are never logged unless `LGTM_DEBUG=1`, and Action logs on public repos are public.

## Development

Uses [mise](https://mise.jdx.dev) for tools and [just](https://just.systems) for tasks.

```sh
mise install
just check                 # lint + unit tests
just test integration      # live Jev API; needs OPENROUTER_API_KEY
```

## License

[Apache-2.0](LICENSE)
