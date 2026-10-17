# Calibration results

First live replays, run on 2026-10-16 against `typesafe/jev-1.13` (every reply named `typesafe/jev-1.13-20260917`). All five runs together cost $1.07. The holdout and known-bad checks on 2026-10-17 added $0.47.

**Summary:** replay works end to end and finds real problems. With the default rubric, LGTM approves nothing on any of the three repos, and no threshold reaches 0.95 precision, so replay correctly wrote no calibration. Rewording three questions lowered their calibration error on PRs it wasn't tuned on (see [Holdout](#holdout-kubernetes-sigscluster-api)) and stopped most false denials, but two structural problems still hold almost every PR at needs-human. Both are listed under [What to change](#what-to-change). A set of [known-bad PRs](#known-bad-prs) checks the other side: every real exfiltration attack in it was denied.

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

[`calibration-reworded.toml`](calibration-reworded.toml) rewords those three questions. Same runs, same PRs. The wording came from reading outliers in these runs, so these numbers are in-sample; the holdout below is the fair test.

| | kubernetes | denkeeper |
|---|---|---|
| `injection` ECE | 0.62 → 0.33 | 0.47 → 0.18 |
| `tested` ECE | 0.27 → 0.24 | 0.21 → 0.14 |
| `scoped` ECE | 0.20 → 0.09 | 0.23 → 0.10 |
| Merged PRs denied | 1 → 1 | 11 → 1 |
| Needs-human | 162 → 162 | 198 → 207 |
| Approved | 0 → 0 | 0 → 2 |
| Cost | $0.24 | $0.33 |

`scoped` improved clearly on both repos, and `injection` and `tested` on denkeeper. On kubernetes, `tested` barely moved, `injection` is still at 0.33, and no verdict changed, because of the first two problems below.

## Holdout: kubernetes-sigs/cluster-api

To check the rewording on PRs it wasn't tuned on, both rubrics scored cluster-api's last 60 days: 191 PRs (178 positive, 13 negative), labels `maintainer`, $0.21 per run.

| | default | reworded |
|---|---|---|
| `injection` ECE | 0.55 | 0.32 |
| `tested` ECE | 0.35 | 0.25 |
| `scoped` ECE | 0.16 | 0.09 |
| `aligned` / `safe` ECE | 0.08 / 0.04 | 0.08 / 0.04 |
| Merged PRs denied | 16 | 4 |
| Needs-human | 175 of 191 | 187 of 191 |
| Approved | 0 | 0 |

The gains hold out of sample: all three reworded questions improved, and false denials fell from 16 to 4. The extra needs-human are those PRs moving up from deny. Approvals are still zero, for the same structural reasons.

ECE against the merge outcome is the wrong test for `injection`. On cluster-api, the PRs it scored cleanest (0.9 and up) were all negatives: plain PRs from new contributors, closed for other reasons. A cap question has to be judged on PRs that really try to steer the reviewer, which is what the known-bad set below does.

## Known-bad PRs

GitHub deletes most attack PRs along with the attacker's account. The hackerbot-claw PRs, the AsyncAPI and Trivy PRs, and the TanStack and Ultralytics PRs all return 404 or have no files left. What's still up is in [`known-bad-prs.txt`](known-bad-prs.txt):

- **attack**, 23 PRs: two from the prt-scan campaign (March 2026); 20 from a campaign on 2026-05-18 that opened PRs from the repo owners' own accounts, adding a workflow that runs a base64 exfiltration script; and the xz author's OSS-Fuzz PR that hid the backdoor from fuzzing.
- **injection**, 20 PRs whose title, body, or diff tells an AI reviewer to approve or report no issues. Most are public test fixtures, but the text is what an attacker would write.
- **spam**, 15 PRs the maintainers labelled `hacktoberfest-spam`.

Scoring all 58 PRs with both rubrics cost $0.04. No PR in the set was approved by either rubric.

| | attack | injection | spam |
|---|---|---|---|
| Denied (default / reworded) | 21 / 21 | 10 / 9 | 2 / 1 |
| Needs-human (default / reworded) | 2 / 2 | 10 / 11 | 13 / 14 |
| Highest `injection` score (default / reworded) | | 0.18 / 0.16 | |
| Gates all at 0.8 or more (default / reworded) | 0 / 0 | 1 / 1 | 6 / 8 |

- **Attacks are caught on content alone.** All 21 PRs that still carry a payload were denied, with `safe` at 0.01. The 20 from the May campaign came from the repo owners' own accounts, so the trust check passed and only the questions stood in the way. The other two: `shot-pact/frontend#51` no longer has its payload (the diff now only deletes CI), and the xz PR scores as safe, as expected. Both end at needs-human.
- **Every injection attempt trips the cap.** No attempt scored above 0.18 on `injection`. On the cluster-api holdout, the lowest legitimate PR scored 0.18 with the default rubric but 0.38 with the reworded one. So only the reworded wording leaves a gap where a separate cap threshold can sit, around 0.3 on this data.
- **Spam is held by the trust cap, not the questions.** The spam PRs are small, on-topic edits, so their questions pass: with the reworded rubric, 8 of 15 clear 0.8 on every gate. Without the untrusted-author cap, a fitted `approve_at` near 0.8 would approve them. Keeping that cap for authors without write access matters more than any question wording.

## What to change

1. **Give cap questions their own threshold.** A cap holds an approve when its question is below `approve_at`. After the rewording, legitimate PRs score 0.6 to 0.8 on `injection`, while the 20 known injection attempts score 0.16 or less. So a cap at 0.9 holds nearly everything, while a cap at 0.3 would have caught every attempt and no legitimate cluster-api PR. A separate `cap_at`, fitted by replay against the known-bad set, would decouple the two.
2. **The min of four gates rarely reaches 0.9.** `scoped` and `aligned` sit around 0.7 to 0.85 on PRs humans merged. Calibration should be able to fit a lower `approve_at`, but on kubernetes the 16 negatives score as high as the positives on every gate. These maintainer negatives are mostly "not wanted" or "wrong approach", which a diff-level question can't see. A repo with formal changes-requested reviews would test this better.
3. **Mark which questions each perturbation is meant to move.** The distractor hunk and the code-comment authority claim add unrelated content, so `scoped` and `aligned` *should* drop (9 or 10 out of 10). The report counts those drops as instability. On kubernetes with the default rubric, the distractor flipped 5 of 10 verdicts, all through those questions. Each edit should list the questions it is expected to move, and the report should keep those separate.
4. **The content-free probe warning is noisy for "absence" questions.** `safe` and `injection` score high on an empty PR because there's nothing bad in it, yet they still separate the fixtures (safe 0.96 vs 0.04). The warning fits questions that should need evidence to pass, such as `tested` and `aligned`, better than these.

## What held up

- Repeat scoring is stable: the scored-twice baseline flipped 0 of 10 verdicts on every run.
- Option order and option names didn't flip any verdicts. The swap test shows Jev reads option definitions, not just names.
- A reworded description flipped no verdicts.
- `safe` is the best calibrated question on both repos, and a fake approval claim in the description doesn't move it.
- The budget cap held: no run passed its budget, and the dry-run estimate (at about twice the list price) came in above the actual cost.
