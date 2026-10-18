# LGTM

[![CI](https://github.com/Temikus/lgtm/actions/workflows/ci.yml/badge.svg)](https://github.com/Temikus/lgtm/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Score pull requests with System One decision models.

LGTM asks a decision model such as TypeSafe's Jev (`typesafe/jev-1.13`) a set of typed questions about a PR: is the diff aligned with its description, scoped, free of malice and security mistakes, tested? Each answer is a 0–1 value. LGTM combines them into one verdict, **approve**, **deny**, or **needs-human**, which you can gate a merge on. A typical PR costs a fraction of a cent.

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
- `lgtm-review.yml` lets a human approval pass the check on fork PRs, whose review events get a read-only token. It never calls the model. For a first-time contributor, GitHub may hold the review event's run until you re-run it from the Actions tab.

## Verdicts

Each question gives a 0–1 value. The default aggregate is the minimum over gating questions:

- at or above `approve_at` (default 0.9): **approve**;
- at or below `deny_at` (default 0.1): **deny**;
- in between: **needs-human**.

A **veto** question denies on its own: `malicious` below `veto_at` (default 0.4) denies whatever the aggregate says. It asks about deliberate harm, such as exfiltration, backdoors, hidden payloads, and weakened CI safeguards. `veto_at` is global, not fitted per repo; it is set from real attacks and hard legitimate PRs ([docs/calibration.md](docs/calibration.md#safety)).

Any of these holds an approve at needs-human: a cap question is below its `cap_at` (`injection` at the global default 0.3, `hygiene` at 0.8, `breaks` at 0.7), LGTM couldn't see the whole diff, a lockfile pulls a package from outside the usual registries (npm, PyPI, crates.io, RubyGems, pub.dev; `lockfile_registries` adds your own), the diff changes files that run commands on their own on developers' machines (VS Code tasks, dev containers, git hooks, or Claude, Gemini, or Cursor agent settings), a required check failed on the commit (the base branch's ruleset checks plus `ci_required`; checks still running don't count, so this mostly catches re-scores; GitHub still blocks the merge on a failed required check, and a re-check after CI is [#17](https://github.com/Temikus/lgtm/issues/17)), the diff changes a sensitive path (CI workflows, actions, and CODEOWNERS by default; `sensitive_paths`), the diff is over `max_changed_lines` (default 1,000), the PR is from a fork by an author without write access, the replies name a different model version than the one the thresholds were calibrated on, or a borderline score changed when `choice` options were reordered. Provider errors, timeouts, and oversized input also give needs-human. Nothing defaults to approve.

In **enforce** mode, the check run fails on deny and needs-human, and the job exits non-zero. A needs-human check passes once someone with write access approves the scored commit. Enforce mode needs a `[calibration]` table from `lgtm replay --write`, because raw model probabilities aren't calibrated. Use it alongside required reviewers or CODEOWNERS, never instead of them.

## Configuration

`.github/lgtm.toml` is read at the PR's base commit. `lgtm init` writes a commented copy, and `lgtm config validate` checks one.

| Question | Type | Role | Sees |
|---|---|---|---|
| `aligned` | yes/no | gate | everything |
| `scoped` | yes/no | gate | everything |
| `malicious` | yes/no | veto | diff only |
| `hygiene` | yes/no | cap | diff only |
| `breaks` | yes/no | cap | diff only |
| `tested` | yes/no | info | diff only |
| `injection` | yes/no | cap | everything |
| `described` | yes/no | info | everything |
| `risk` | choice | info | everything |
| `reviewability` | 5-level score | info | everything |

"Diff only" questions never see the PR title, description, or issue titles, so claims like "approved by security" there can't sway them. `hygiene` covers honest security mistakes (committed secrets, injectable code, disabled TLS or auth): they need a human, not a deny. `[questions.hygiene] cap_at = 0.7` sets its own hold point. `tested` is informational because on reviewed history rejected PRs scored higher on it than merged ones ([docs/calibration.md](docs/calibration.md)); `[questions.tested] gate = true` makes it a gate again.

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
lgtm replay owner/repo --dry-run           # label history and estimate cost; no model calls
lgtm replay owner/repo --perturb           # score labelled PRs and report
lgtm replay owner/repo --write             # also store [calibration] in .github/lgtm.toml
lgtm replay owner/repo --offline           # rerun from the local dataset: no GitHub calls
lgtm data pull --list docs/known-bad-prs.txt && lgtm replay owner/repo --lists
lgtm data pull --list docs/hard-good-prs.txt --label positive
```

Replay labels closed PRs from history. By default a PR approved by someone with write access and then merged is positive, and one closed after a changes-requested review, or reverted within 14 days, is negative. Few repos close PRs after a formal changes-requested review, so strict negatives are rare. `--labels maintainer` adds Prow's `lgtm` label as an approval and counts a PR closed unmerged by someone other than its author (directly, or through a bot after their `/close`) as negative; stale closures and authors closing their own PRs stay unlabelled. It works on repos like kubernetes. For a repo with one maintainer and no reviews, `--labels merged` counts merged PRs as positive and closed ones as negative, skipping bots. These labels are weaker, because they record what shipped rather than what a reviewer judged.

It then scores the labelled PRs and picks the lowest `approve_at` that approves at most `--max-false-approve` (default 5%) of rejected PRs, and the highest `deny_at` below it that denies at most `--max-false-deny` (default 1%) of merged ones, known-good listed PRs included. Rates per class are used rather than precision, because most repos merge 95% of what they close, so approving nearly everything would already reach 95% precision. The fit needs at least 20 rejected PRs, since one false approval in fewer than 20 is already more than 5%. `--write` stores the result, with the model version the replies named, and changes nothing else in the file. It writes nothing if the fit fails or if the replies named more than one version. `--budget-usd` (default 2) caps the run: before each model call, replay reserves that request's estimated cost and stops if the total could pass the budget. `--dry-run` shows the estimate first.

Replay keeps PRs, labels, diffs, and model replies in Parquet files under your cache directory (`lgtm data dir` prints it), so a rerun reads only the PR list and asks the model only about calls it hasn't seen. `--offline` reads nothing from GitHub, and `--rescore` asks the model again. The first model call of each run always goes to the model, and stored replies are reused only if they came from the version it serves now, so a silent model update can't hide behind the cache. To start again, delete the directory. Every run also adds per-question scores to a `scores` table you can query, for example with DuckDB: `select * from read_parquet('<dir>/scores/*.parquet', union_by_name=true)`.

## Data

LGTM sends PR content (title, description, linked issue titles, and diffs) to the model provider. Paths matched by `exclude` are never sent. On OpenRouter, turn on the data-policy setting that refuses providers which train on inputs. Request bodies are never logged unless `LGTM_DEBUG=1`, and Action logs on public repos are public. `lgtm replay` and `lgtm data` keep PR content and model replies on your machine, in the dataset directory; delete it, or pass `--no-data`, to keep nothing.

## Development

Uses [mise](https://mise.jdx.dev) for tools and [just](https://just.systems) for tasks.

```sh
mise install
just check                 # lint + unit tests
just test integration      # live Jev API; needs OPENROUTER_API_KEY
```

## License

[Apache-2.0](LICENSE)
