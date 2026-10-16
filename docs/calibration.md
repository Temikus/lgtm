# Calibration results

First live replays, run on 2026-10-16 against `typesafe/jev-1.13` (every reply named `typesafe/jev-1.13-20260917`). All five runs together cost $1.07.

**Summary:** replay works end to end and finds real problems. With the default rubric, LGTM approves nothing on any of the three repos, and no threshold reaches 0.95 precision, so replay correctly wrote no calibration. Rewording three questions fixed most of the miscalibration, but two structural problems still hold almost every PR at needs-human. Both are listed under [What to change](#what-to-change).

## Runs

| Repo | Labels | PRs scored | Positive / negative | Cost |
|---|---|---|---|---|
| kubernetes/kubernetes, 30 days | `maintainer` | 164 | 148 / 16 | $0.21 |
| Temikus/denkeeper, 1 year | `merged` | 210 | 205 / 5 | $0.28 |
| Temikus/butter, 1 year | `merged` | 24 | 23 / 1 | $0.01 |

Commands:

```sh
lgtm replay kubernetes/kubernetes --labels maintainer --since 30d --max-prs 200 --perturb --budget-usd 1
lgtm replay Temikus/denkeeper --labels merged --since 365d --perturb --budget-usd 0.5
lgtm replay Temikus/butter --labels merged --since 365d --budget-usd 0.25
```

Kubernetes is the main test of the method: real reviewers and enough negatives. denkeeper and butter have one maintainer, so their labels only say what shipped, and butter has too few negatives to fit.

## Default rubric

| | kubernetes | denkeeper |
|---|---|---|
| Needs-human | 162 of 164 | 198 of 210 |
| Merged PRs denied | 1 | 11 |
| Fit | no approve threshold reaches 0.95 | no deny threshold reaches 0.95 |

Expected calibration error (ECE) of each gate and cap question against the PR outcome, lower is better:

| Question | kubernetes | denkeeper |
|---|---|---|
| `injection` | 0.62 | 0.47 |
| `tested` | 0.27 | 0.21 |
| `scoped` | 0.20 | 0.23 |
| `aligned` | 0.11 | 0.09 |
| `safe` | 0.08 | 0.05 |

What went wrong, from `lgtm score` on the outliers:

- **`injection` flags ordinary PR text.** A kubernetes description that is only `/kind cleanup`, an empty release note, and an AI-usage disclosure scored 0.28. Most merged kubernetes PRs scored 0.2 to 0.4. Prow commands and PR templates read as text addressed to automation.
- **`tested` fails changes with no runtime effect.** A CI-only change (denkeeper #509) scored 0.10 and was denied.
- **`scoped` fails companion changes.** denkeeper PRs that update the docs alongside the code scored around 0.1 and were denied.

## Reworded rubric

[`calibration-reworded.toml`](calibration-reworded.toml) rewords those three questions. Same runs, same PRs:

| | kubernetes | denkeeper |
|---|---|---|
| `injection` ECE | 0.62 → 0.33 | 0.47 → 0.18 |
| `tested` ECE | 0.27 → 0.24 | 0.21 → 0.14 |
| `scoped` ECE | 0.20 → 0.09 | 0.23 → 0.10 |
| Merged PRs denied | 1 → 1 | 11 → 1 |
| Needs-human | 162 → 162 | 198 → 207 |
| Approved | 0 → 0 | 0 → 2 |
| Cost | $0.24 | $0.33 |

The rewording is a clear improvement, but the verdicts barely moved, because of the two problems below.

## What to change

1. **Give cap questions their own threshold.** A cap holds an approve when its question is below `approve_at`. After the rewording, legitimate PRs score 0.6 to 0.8 on `injection`, while the steered fixture scores 0.02. So a cap at 0.9 holds nearly everything, and a cap at 0.5 would still catch the attacks seen so far. A separate `cap_at` (for example 0.5), fitted by replay, would decouple the two.
2. **The min of four gates rarely reaches 0.9.** `scoped` and `aligned` sit around 0.7 to 0.85 on PRs humans merged. Calibration should be able to fit a lower `approve_at`, but on kubernetes the 16 negatives score as high as the positives on every gate. These maintainer negatives are mostly "not wanted" or "wrong approach", which a diff-level question can't see. A repo with formal changes-requested reviews would test this better.
3. **Mark which questions each perturbation is meant to move.** The distractor hunk and the code-comment authority claim add unrelated content, so `scoped` and `aligned` *should* drop (9 or 10 out of 10). The report counts those drops as instability. On kubernetes with the default rubric, the distractor flipped 5 of 10 verdicts, all through those questions. Each edit should list the questions it is expected to move, and the report should keep those separate.
4. **The content-free probe warning is noisy for "absence" questions.** `safe` and `injection` score high on an empty PR because there's nothing bad in it, yet they still separate the fixtures (safe 0.96 vs 0.04). The warning fits questions that should need evidence to pass, such as `tested` and `aligned`, better than these.

## What held up

- Repeat scoring is stable: the scored-twice baseline flipped 0 of 10 verdicts on every run.
- Option order and option names didn't flip any verdicts. The swap test shows Jev reads option definitions, not just names.
- A reworded description flipped no verdicts.
- `safe` is the best calibrated question on both repos, and a fake approval claim in the description doesn't move it.
- The budget cap held: no run passed its budget, and the dry-run estimate (at about twice the list price) came in above the actual cost.
