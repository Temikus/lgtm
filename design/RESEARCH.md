# Research: pitfalls and borrowable findings for LGTM

Deep-research run of 2026-10-16. 24 sources fetched, 118 claims extracted, 25 verified by 3-vote adversarial check (19 confirmed, 6 refuted).

## Summary

The biggest risk the PRD may have missed comes from Jev's model family. A typed decision head can answer from the option name rather than the rubric text bound to it. For Jev itself, swapping which definition is bound to yes/no raised decision flips by 30.4 pp over neutral 0/1 names and cut AUC from 81.5% to 58.1% (arXiv 2609.26758). Raw option probabilities and 2-10 scores also carry label, position and score-level priors that change by task and scale. So LGTM should use neutral option names, calibrate each parameter and scale separately on PR data, and run perturbation and label-swap checks (the CALM method) during replay before any threshold is trusted. Industry deployments at Mozilla/Ubisoft, Beko, WirelessCar, ByteDance and Atlassian agree on three lessons: noise and false positives destroy trust, so favour precision and short ranked output; automated output anchors human reviewers (automation bias); and the effect on review time is mixed, so shadow mode should measure time-to-merge as a cost. No verified claims survived on prompt injection, diff truncation, Goodhart gaming, PR-signal predictors of defects or reverts, or conformal and selective-prediction thresholds. Those areas are still open.

## Findings

### 1. (medium) [Rubric] Jev and similar typed decision heads follow the option name, not the definition bound to it. Rubric options named yes/no, approve/deny or similar may be answered from the name. PRD change: give every noul/choice option a neutral name (A/B, 0/1 or random strings) and put the meaning only in the bound definition text. Add label-swapped and counterbalanced variants of each gating question to the calibration and replay suite. High accuracy under the default mapping can hide a model that follows the label.

**Evidence:** For Jev specifically: 32.50% decision flips with yes/no names vs 2.08% with 0/1 names, a 30.4 pp gap over a test-retest floor of at most 1.33%. Balanced accuracy fell from 71.3% to 51.6% and AUC from 81.5% to 58.1% (Sec. 4.5). The headline 70.4 pp gap and the AUC drop from 93.8% to 23.2% (below chance) are for Laya, an open-weight Jev-like model, not Jev, so cite 30.4 pp for Jev. Random-string names behaved like the neutral control. approve/deny was not tested; rejected/accepted is the closest pair tested. Type-error rate stayed at 0%, so type safety does not prevent this. A follow-up paper (arXiv 2610.02586) reportedly also found reliance on option labels and gains from renaming options to A/B; that paper was not verified here. Votes were 2-1 on both claims. The source is one non-peer-reviewed preprint, about 3 weeks old, and Jev is closed-source and may change.

**Sources:** <https://arxiv.org/abs/2609.26758>

### 2. (medium) [Rubric / Calibration] Ordinal 2-10 score questions are exposed to scoring bias: judges favour particular score values whatever the input. The bias differs by model, task and score range. PRD change: before setting thresholds, measure each score question's marginal distribution on content-free or shuffled PRs and on historical replays. Calibrate per parameter and per scale, not with one global correction. Pooling across parameters, for example with a hierarchical model, is acceptable when human anchors are sparse.

**Evidence:** 2608.05726 states that LLM evaluators tend to generate particular scores regardless of the context and that scoring bias varies across LLMs, tasks, and score ranges. 2506.22316 independently finds substantial scoring biases even in strong LLMs. 2605.09227 reports leniency, strictness and mid-scale compression, and pools calibration across rubrics. 2602.02219 finds that rubric scoring behaves like multiple choice, with positional bias across score options. Caveats: all evidence comes from generative LLMs emitting number tokens. The 2-10 range was never tested. Non-transfer across tasks is inferred from task dependence, not measured directly. Votes were 3-0 and 2-1.

**Sources:** <https://arxiv.org/abs/2608.05726>, <https://arxiv.org/abs/2506.22316>, <https://arxiv.org/abs/2605.09227>, <https://arxiv.org/abs/2602.02219>

### 3. (medium) [Verdict aggregation / Calibration] Raw option probabilities from a constrained head contain a label prior and should not be read as calibrated confidences. Comparing raw noul P(yes) or MIN-of-probabilities against fixed approve/deny thresholds is unsafe. PRD change: put a calibration map, fitted on historical human decisions, between raw probabilities and the MIN aggregator. Report reliability (for example ECE or reliability diagrams) per parameter during shadow mode.

**Evidence:** CalibraEval (2410.15393) models the observed probability as a function of a content-independent prior and a debiased probability, then fits a calibration map. Zheng et al. (ICLR 2024, 2309.03882) find across 20 LLMs that models a priori assign more probability mass to specific option ID tokens; their PriDe method removes that prior. Caveats: content independence is a modelling assumption. Both papers study generative next-token probabilities, not a non-generative decision head. Vote was 2-1. A related claim, that swapping slot or ID token alone flips verdicts on named options, was refuted 1-2.

**Sources:** <https://arxiv.org/abs/2410.15393>, <https://arxiv.org/abs/2309.03882>

### 4. (high) [Rubric / Verdict aggregation] Position and order bias is systematic, not random noise. It differs by judge and task, grows with the number of options, and is strongest on low-margin or borderline cases, which are the PRs near approve/deny thresholds. PRD change: randomise or permute option order for multi-option choice questions during calibration. Measure the bias profile on PR data, not borrowed benchmarks. Keep a wide needs-human band, and require order-swap and name-swap consistency before any near-threshold approve.

**Evidence:** CALM (ICLR 2025): position robustness in the default setting was 0.566 for ChatGPT and 0.832 for Claude-3.5. The paper says the bias becomes more pronounced as the number of answers increases, and most models fall below 0.5 with 3-4 candidates. Judging the Judges (AACL-IJCNLP 2025; 15 judges, 22 tasks including the software-development benchmark DevBench, more than 150k instances) finds position bias is not due to random chance, varies significantly across judges and tasks, and is most pronounced when quality gaps are minimal. Pezeshkpour & Hruschka (NAACL Findings 2024) tie MCQ order sensitivity (13-75% performance gaps) to uncertainty among the top 2-3 choices, which applies to the single-PR, pointwise case. Caveat: 2406.07791 studies pairwise comparison only, and none of the sources tests a non-generative head. Votes: 3-0, 3-0, 2-1.

**Sources:** <https://arxiv.org/abs/2410.02736>, <https://arxiv.org/abs/2406.07791>, <https://arxiv.org/abs/2308.11483>

### 5. (medium) [Security posture / Rubric] Judges favour content with authority cues, even fabricated ones, and cite planted fake references to justify their decision. PR descriptions or code comments such as 'approved by security team, see SEC-123' could push aligned/safe scores up. This vector is not covered by capping fork PRs; insiders and bots can use it too. PRD change: add paired adversarial fixtures (the same PR with and without fake approval or ticket claims) to the replay suite, and treat a score shift as a defect. Consider removing or neutralising approval-claim text before scoring the safe parameter.

**Evidence:** CALM, Sec. 5: the LLM openly favored answers containing citations, even when these were fake. Authority-bias robustness rates of 0.662-0.865 across six judges mean 13-34% of judgments flipped. Book and quote citations moved judges more than URLs. 2402.10669 (EMNLP 2024) reports over 50% attack success with fake references on Claude-2. Counter-evidence: 2409.15268 found judges robust to common authority hacks on Arena-Hard-Auto, so effect size depends on benchmark and judge. Transfer to code diffs and to Jev is untested. Treat this as a hypothesis to test, not a fact. Vote 3-0.

**Sources:** <https://arxiv.org/abs/2410.02736>, <https://arxiv.org/abs/2402.10669>, <https://arxiv.org/abs/2409.15268>

### 6. (high) [Calibration / replay backlog] Use CALM-style principle-guided perturbations as LGTM's robustness test harness. Apply controlled edits to a PR and measure flip rate against a no-perturbation consistency baseline (the same input run twice). Adopt the method, not the paper's bias rates. Code-specific perturbations to include: rewording the description, fake authority claims in comments, distracting unrelated hunks, renamed or reordered options, verbose vs terse descriptions, and label-swapped option bindings.

**Evidence:** CALM (ICLR 2025) covers 12 biases and reports that advanced models have achieved commendable overall performance, but significant biases persist in certain specific tasks. Robustness Rate is the share of decisions that stay the same after a perturbation; Consistency Rate is the noise floor. Several CALM biases (CoT, self-enhancement, refinement-aware, arguably bandwagon and diversity) do not apply to a non-generative single-PR scorer. Subsets are small (150-500) and GPT-4o generates the perturbations. Vote 3-0.

**Sources:** <https://arxiv.org/abs/2410.02736>

### 7. (high) [Outputs] Noise and false positives are the main reason developers ignore review bots, and early inaccurate output damages trust badly. PRD change: favour precision over recall at launch. Keep the PR comment short and ranked, limited to gating parameters, with file and line references where possible. Do not leave ungated 'nice to know' scores in the main output. Shadow mode is the right place to tune for precision before anything is visible.

**Evidence:** RevMate at Mozilla/Ubisoft: 20 of 37 reviewers said comment relevance must improve (comments were 'superficial', 'generic', sometimes 'wrong'), and trivial or incorrect comments were the main reasons for ignoring them. WirelessCar (2505.16339): 'you get flooded with false positives, and then you miss the real issues'. The assistant surfaced low-priority findings that were hard to tell apart from important ones in long summaries, and participants asked for short, file- and line-specific output. ByteDance BitsAI-CR (FSE 2025 industry track): developers tend to ignore review comments entirely when faced with numerous comments; early inaccurate comments significantly damage trust; the team chose precision over recall. 10.9% and 12.4% of 137 survey respondents flagged incorrect or unnecessary comments; these are not error rates. All votes 3-0. All three studies tested generative comment bots, so they carry over to a verdict/score bot by analogy.

**Sources:** <https://arxiv.org/html/2411.07091v1>, <https://arxiv.org/pdf/2505.16339>, <https://arxiv.org/pdf/2501.15134>

### 8. (medium) [Verdict aggregation / Outputs] Automated review output anchors human reviewers: they focus on what the bot flagged and may assume it caught everything. An automated approve probably increases this risk. PRD change: keep needs-human prominent. Never let an automated approve replace required human review on protected branches. Word the output so that a pass makes no claim that no other issues exist. In shadow mode, track whether human review depth (comments per PR, time in review) drops on PRs the bot approves.

**Evidence:** Beko (ICSE 2025 SEIP), one survey respondent: 'reviewers may ignore by saying that if any other issue exists, the bot would have written it.' WirelessCar participant P3: 'I could miss something else, because I would focus on those improvements.' Tufano et al. (ICSE 2025, 2411.11401) is a controlled experiment: automated reviews anchor reviewer focus on tool-raised issues, with no improvement in finding high-severity issues. Atlassian RovoDev (ICSE 2026 SEIP): PRs with bot comments had 35.6% fewer human-written comments (2.87 vs 4.45, p<.001). This is observational; the paper calls it reduced workload, and reduced scrutiny is an inference. The evidence is mostly qualitative or anecdotal, plus one experiment. All studies cover comment bots, not approve verdicts. Votes 3-0.

**Sources:** <https://arxiv.org/abs/2412.18531v2>, <https://arxiv.org/pdf/2505.16339>, <https://arxiv.org/abs/2411.11401>, <https://arxiv.org/pdf/2601.01129v1>

### 9. (medium) [Shadow mode / Calibration] Industrial effects on review speed are mixed. Do not assume LGTM speeds up review; measure time-to-merge as a possible cost. Comment acceptance rates from comment bots (about 74% resolved at Beko) are only a loose outside reference. Score LGTM's shadow mode by agreement with human verdicts (precision and recall per approve/deny/needs-human) and break results down by project or repo.

**Evidence:** Beko (Qodo PR-Agent on GPT-4): average PR closure time rose from 5h52m to 8h20m (p<<0.001). Per-project results ranged from 2h48m to 4h38m (up), 6h06m to 3h07m (down) and 20h22m to 30h51m (up). The conclusion section gives contradictory numbers, and the before/after design is confounded by seasonality. 73.8% of 1,408 comments were labelled Resolved, 26.2% Won't Fix or Closed, with 55%-90% Resolved by project. Labels were chosen by PR authors, so 26% is closer to a lower bound on noise. Atlassian RovoDev: median cycle time was 31% lower (14.35h vs 20.73h) on PRs with bot comments. This is observational and not randomised. A refuted claim (1-2) had used Atlassian's 38.70% vs 44.45% code-resolution rate as an acceptance baseline, so do not cite that figure as a baseline.

**Sources:** <https://arxiv.org/abs/2412.18531v2>, <https://arxiv.org/pdf/2601.01129v1>

### 10. (medium) [Calibration / replay backlog] Historical human review output is incomplete, noisy ground truth. Matching it should not be the only success metric. When replaying LGTM against past human decisions, treat disagreements as cases to adjudicate, not automatic errors. Sample them for expert relabelling, and track rubber-stamp approvals in the history as label noise.

**Evidence:** Atlassian RovoDev, Sec. 5.1: only 7% of PRs had at least one bot comment aligned with a human comment. About 40% of comments matched no human comment. The authors write that 'the similarity metric should not be used as the sole measure of the practical usefulness'. Online code resolution (38.70%) was far above offline alignment (4-7%), but the paper never measured whether the non-aligned comments were useful, so 'badly underestimates' goes beyond the paper. The study covers free-text comment similarity; LGTM's categorical verdict labels are better defined, so only the caution about noisy labels carries over. Vote 3-0.

**Sources:** <https://arxiv.org/pdf/2601.01129v1>

## Caveats

Coverage gaps: no claims survived verification on several topics the question asked about. These are prompt injection via PR content against AI reviewers, long-context and diff-truncation effects, Goodhart effects or developers gaming scores, PR or code-change signals that predict review outcomes, defects or reverts (size, tests, tangled changes, description quality), Google, Meta or Microsoft deployment studies, conformal prediction or selective prediction for thresholds, and methods for aggregating multiple rubric judgments. The PRD's current choices in those areas (MIN aggregation, a fork-PR cap at needs-human, partial view never approves) are neither confirmed nor challenged by this evidence.

Evidence on Jev specifically: the most direct finding (2609.26758) is a single, non-peer-reviewed preprint about 3 weeks old, and both claims from it passed only 2-1. Its headline numbers (70.4 pp, AUC dropping to 23.2%) describe Laya, not Jev; the Jev figures are 30.4 pp and AUC from 81.5% to 58.1%. Jev is closed-source and non-deterministic, and was tested at one point in time, so its served behaviour may change.

Transfer to Jev: almost all bias evidence (scoring, token, position, authority) comes from generative LLM judges, mostly in pairwise comparisons. Carrying it over to a non-generative typed decision head that scores one PR is plausible but untested, except for the option-name paper.

Industrial studies (Beko, Atlassian, ByteDance, Mozilla/Ubisoft, WirelessCar) evaluate generative comment bots, not approve/deny verdict bots. Their lessons on noise, trust and automation bias carry over by analogy. Several rely on small qualitative samples (n=7-37), single respondents, or observational before/after designs with confounders.

Refuted claims to avoid citing: the 7-8% RevMate acceptance rate as a baseline, Atlassian's 38.70% acceptance as a baseline, the ByteDance ReviewFilter precision gain (54.50% to 67.12%) as support for a two-stage gate, and the perplexity/familiarity explanation of self-preference bias.

Time sensitivity: most sources are 2024-2026 preprints, and model behaviour shifts between versions, so bias profiles should be measured again whenever Jev is updated.

## Open questions

- Does Jev's option-name and score-level bias also show up on approve/deny, safe/unsafe and 2-10 ordinal options on real PR diffs? How large is it there, and do neutral or random-string names fully remove it without hurting accuracy?
- What is the evidence on prompt injection and authority-claim text in diffs, descriptions and comments against non-generative classifiers? Should non-fork PRs from insiders or bots also face a cap or sanitisation step, not only fork PRs?
- Which calibration and abstention method best fits MIN-aggregated gating: per-parameter Platt or isotonic scaling, label-swap debiasing such as PriDe, or conformal and selective prediction with a guaranteed approve error rate? How many labelled historical PRs per repo does each need?
- Which PR signals (size, test changes, tangled changes, description quality) actually predict reverts or post-merge defects strongly enough to check or replace the 'tested', 'scoped' and 'risk' rubric parameters? Does chunking large diffs change verdict accuracy measurably?

## Refuted (do not cite)

- (1-2) When an LLM judge picks between named options, its answer can depend on which slot an option occupies (position bias) and on the option's ID token (token bias). Swapping either one can flip the verdict. This applies directly to LGTM's 'choice over named options' questions and its approve/deny labels. <https://arxiv.org/abs/2410.15393>
- (1-2) LLM judges score low-perplexity (familiar) text significantly higher than human evaluators do, whether or not the judge wrote that text. For LGTM, this means the classifier's scores may track fluent or boilerplate-like PR descriptions and diffs (for example, AI-generated PRs) rather than true rubric compliance, so the 'described' and 'aligned' parameters need calibration against human decisions. <https://arxiv.org/abs/2410.21819>
- (0-3) The authors trace self-preference bias to familiarity, measured as low perplexity, rather than to recognising their own output. This suggests that a non-generative judge can still have the bias whenever its training distribution resembles the text it is judging. <https://arxiv.org/abs/2410.21819>
- (1-2) In a field study of RevMate (GPT-4o with RAG and LLM-as-a-Judge filtering) covering 587 patch reviews at Mozilla and Ubisoft, reviewers accepted only about 7-8% of LLM-generated review comments. Industrial acceptance of automated review output is low, which argues for shadow mode and strict approve thresholds. <https://arxiv.org/html/2411.07091v1>
- (1-2) In a 12-month industrial deployment at Atlassian (June 2024 to June 2025, over 2,000 repos, over 54,000 comments, about 2.1 comments per PR), 38.70% of LLM-generated review comments led to a code change. Human-written comments did so 44.45% of the time. This gives a realistic acceptance baseline for AI review output. <https://arxiv.org/pdf/2601.01129v1>
- (1-2) At ByteDance, adding a second-stage yes/no verifier (ReviewFilter) after the detector (RuleChecker) raised the precision of review comments from 54.50% to 67.12% in the taxonomy-guided model. This supports a separate gate or filter stage before anything is posted or gates a PR. <https://arxiv.org/pdf/2501.15134>

## All sources

- <https://arxiv.org/abs/2609.26758> (LLM-as-judge reliability and biases)
- <https://arxiv.org/abs/2608.05726> (LLM-as-judge reliability and biases)
- <https://arxiv.org/abs/2410.15393> (LLM-as-judge reliability and biases)
- <https://arxiv.org/abs/2410.02736> (LLM-as-judge reliability and biases)
- <https://arxiv.org/abs/2406.07791> (LLM-as-judge reliability and biases)
- <https://arxiv.org/abs/2410.21819> (LLM-as-judge reliability and biases)
- <https://arxiv.org/html/2411.07091v1> (Industry AI code review deployments)
- <https://arxiv.org/abs/2412.18531v2> (Industry AI code review deployments)
- <https://arxiv.org/pdf/2505.16339> (Industry AI code review deployments)
- <https://arxiv.org/pdf/2601.01129v1> (Industry AI code review deployments)
- <https://arxiv.org/pdf/2501.15134> (Industry AI code review deployments)
- <https://arxiv.org/pdf/2512.17540v1> (Industry AI code review deployments)
- <https://arxiv.org/abs/2410.06351> (PR signals that predict review outcome and defects)
- <https://arxiv.org/pdf/2609.22610> (PR signals that predict review outcome and defects)
- <https://arxiv.org/abs/2602.14611> (PR signals that predict review outcome and defects)
- <https://arxiv.org/abs/2601.18749> (PR signals that predict review outcome and defects)
- <https://arxiv.org/abs/2603.18740> (Prompt injection against AI code reviewers)
- <https://arxiv.org/abs/2606.09935> (Prompt injection against AI code reviewers)
- <https://arxiv.org/abs/2602.16741> (Prompt injection against AI code reviewers)
- <https://arxiv.org/html/2602.00305> (Prompt injection against AI code reviewers)
- <https://arxiv.org/pdf/2602.13110> (Selective prediction and threshold calibration)
- <https://arxiv.org/pdf/2512.24587> (Selective prediction and threshold calibration)
- <https://arxiv.org/pdf/2607.27143> (Selective prediction and threshold calibration)
- <https://arxiv.org/pdf/2110.01052v4> (Selective prediction and threshold calibration)
