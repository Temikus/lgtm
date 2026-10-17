package replay

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Temikus/lgtm/internal/verdict"
)

type textWriter struct{ strings.Builder }

func (b *textWriter) w(format string, args ...any) { _, _ = fmt.Fprintf(b, format+"\n", args...) }

// Text renders the report for a terminal. PR titles are untrusted, so they
// lose control characters.
func (r *Report) Text() string {
	var b textWriter
	r.textHeader(&b)
	r.textFit(&b)
	r.textVerdicts(&b)
	r.textReliability(&b)
	r.textProbe(&b)
	r.textPerturb(&b)
	r.textShadow(&b)
	r.textListed(&b)
	r.textCapSweep(&b)
	r.textSafety(&b)
	r.textReview(&b)
	return b.String()
}

func (r *Report) textHeader(b *textWriter) {
	served := ""
	if len(r.Served) > 0 {
		served = " (replies named " + strings.Join(r.Served, ", ") + ")"
	}
	b.w("LGTM replay: %s · labels %s · model %s%s", r.Repo, r.Labels, r.Model, served)
	pos, neg, low := countLabels(r.Samples)
	b.w("PRs: %d closed, %d labelled (%d positive, %d negative, %d low-confidence), %d scored, %d failed · cost $%.4f",
		len(r.Samples), pos+neg+low, pos, neg, low, len(r.Points), len(r.Failed), r.CostUSD)
	if r.BudgetHit {
		b.w("Stopped at the budget: the figures cover the PRs scored before it.")
	}
	switch r.Labels {
	case Merged:
		b.w("Labels come from merges, not reviews, so they say what shipped, not what a reviewer judged.")
	case Maintainer:
		b.w("Negative labels include PRs closed by a maintainer for reasons other than quality, such as duplicates.")
	}
}

func (r *Report) textFit(b *textWriter) {
	b.w("\nThreshold fit (at most %s of rejected PRs approved, %s of merged PRs denied)", pct(r.Rates.MaxFalseApprove), pct(r.Rates.MaxFalseDeny))
	c, f := r.Calibration, r.Fit
	if c == nil {
		b.w("  Nothing to write: %s.", r.WhyNot)
		return
	}
	b.w("  approve_at %s approves %s of merged PRs and %d of %d rejected ones (%s) · precision %s",
		num(c.ApproveAt), pct(f.ApproveRecall), f.FalseApproved, f.NNegative, pct(f.FalseApprove), pct(f.ApprovePrecision))
	denied := fmt.Sprintf("%d of %d merged PRs", f.FalseDenied, f.NPositive)
	if f.NListedPositive > 0 {
		denied = fmt.Sprintf("%d of %d merged and known-good PRs", f.FalseDenied, f.NPositive+f.NListedPositive)
	}
	b.w("  deny_at %s denies %s (%s) · from %d history PRs · %s", num(c.DenyAt), denied, pct(f.FalseDeny), f.NLabelled, c.Snapshot)
	b.w("  The false-approve rate rests on %d rejected PRs: each one is %s.", f.NNegative, pct(share(1, f.NNegative)))
}

func (r *Report) textVerdicts(b *textWriter) {
	b.w("\nVerdicts on labelled PRs")
	cols := []Metrics{r.Current}
	head := fmt.Sprintf("  %-20s %-21s", "", fmt.Sprintf("current (%s/%s)", num(r.CurrentPol.ApproveAt), num(r.CurrentPol.DenyAt)))
	if r.Fitted != nil {
		cols = append(cols, *r.Fitted)
		head += fmt.Sprintf(" fitted (%s/%s)", num(r.Calibration.ApproveAt), num(r.Calibration.DenyAt))
	}
	b.w("%s", strings.TrimRight(head, " "))
	row := func(name string, f func(Metrics) string) {
		line := fmt.Sprintf("  %-20s", name)
		for _, m := range cols {
			line += fmt.Sprintf(" %-21s", f(m))
		}
		b.w("%s", strings.TrimRight(line, " "))
	}
	row("approve precision", func(m Metrics) string { return pct(m.ApprovePrecision) })
	row("approve recall", func(m Metrics) string { return pct(m.ApproveRecall) })
	row("deny precision", func(m Metrics) string { return pct(m.DenyPrecision) })
	row("deny recall", func(m Metrics) string { return pct(m.DenyRecall) })
	row("needs-human", func(m Metrics) string {
		return fmt.Sprintf("%d of %d (%s)", m.NeedsHuman, m.N, pct(ratio(m.NeedsHuman, m.N)))
	})
}

func (r *Report) textReliability(b *textWriter) {
	if len(r.Reliability) == 0 {
		return
	}
	b.w("\nReliability: each gate and cap question against the PR outcome (ECE, lower is better)")
	for _, q := range r.Reliability {
		b.w("  %-16s ECE %.3f over %d PRs", q.ID, q.ECE, q.N)
		for _, bin := range q.Bins {
			b.w("    %.1f-%.1f  %3d PRs  mean score %.2f  positive %s", bin.Lo, bin.Hi, bin.N, bin.MeanScore, pct(bin.PositiveRate))
		}
	}
}

func (r *Report) textProbe(b *textWriter) {
	if len(r.Probe) == 0 {
		return
	}
	b.w("\nContent-free probe: each question's answer to an empty PR / shuffled hunks")
	for _, p := range r.Probe {
		switch {
		case p.Err != nil:
			b.w("  %-16s failed: %s", p.ID, clean(p.Err.Error()))
		case p.Warn:
			b.w("  %-16s %.2f / %.2f  passes with no content, so it can't tell PRs apart", p.ID, p.Empty, p.Shuffled)
		default:
			b.w("  %-16s %.2f / %.2f", p.ID, p.Empty, p.Shuffled)
		}
	}
}

func (r *Report) textPerturb(b *textWriter) {
	if r.Baseline == nil {
		return
	}
	b.w("\nPerturbation: verdict flips per PR, with the questions that flipped")
	for _, f := range append([]FlipRate{*r.Baseline}, r.Edits...) {
		b.w("  %-28s %d/%d%s", f.Name, f.VerdictFlips, f.N, flips(f.QuestionFlips))
	}
}

func (r *Report) textShadow(b *textWriter) {
	s := r.Shadow
	if s == nil {
		return
	}
	b.w("\nShadow mode: %d labelled PRs had an earlier LGTM check run", s.N)
	if s.Decisive > 0 {
		b.w("  agreed with humans on %d of %d approve or deny verdicts (%s)", s.Agreed, s.Decisive, pct(ratio(s.Agreed, s.Decisive)))
	}
	for _, v := range []verdict.Verdict{verdict.Approve, verdict.NeedsHuman, verdict.Deny} {
		if g, ok := s.ByVerdict[v]; ok {
			b.w("  %-12s %3d PRs · median time to merge %s · %.1f comments per PR", v, g.N, dur(g.MedianTimeToMerge), g.MeanComments)
		}
	}
}

// textListed counts verdicts per label and kind. It names every approved
// known-bad PR, the failure that matters most, and counts known-good PRs a
// cap question held.
func (r *Report) textListed(b *textWriter) {
	if len(r.Listed) == 0 {
		return
	}
	type tally struct{ approve, deny, human, failed, capped int }
	byGroup := map[string]*tally{}
	var groups, approved []string
	for _, l := range r.Listed {
		g := string(l.Label) + " " + l.Kind
		t := byGroup[g]
		if t == nil {
			t = &tally{}
			byGroup[g] = t
			groups = append(groups, g)
		}
		if slices.Contains(l.Caps, string(verdict.CapQuestion)) {
			t.capped++
		}
		switch {
		case l.Err != nil:
			t.failed++
		case l.Verdict == string(verdict.Approve):
			t.approve++
			if l.Label == Negative {
				approved = append(approved, l.Ref)
			}
		case l.Verdict == string(verdict.Deny):
			t.deny++
		default:
			t.human++
		}
	}
	slices.Sort(groups)
	b.w("\nListed PRs: %d scored, %d known-bad approved (known-good ones count towards false denials)", len(r.Listed), len(approved))
	for _, g := range groups {
		t := byGroup[g]
		b.w("  %-22s %d approve · %d needs-human · %d deny · %d not scored · %d held by a cap question", g, t.approve, t.human, t.deny, t.failed, t.capped)
	}
	for _, ref := range approved {
		b.w("  approved known-bad: %s", clean(ref))
	}
}

// textCapSweep shows what each cap_at would hold. The configured one is
// starred.
func (r *Report) textCapSweep(b *textWriter) {
	if len(r.CapSweeps) == 0 {
		return
	}
	b.w("\nCap sweep: PRs each cap_at would hold (* is the configured one)")
	for _, sw := range r.CapSweeps {
		b.w("  %s", sw.Question)
		for _, row := range sw.Rows {
			mark := " "
			if row.At == sw.At {
				mark = "*"
			}
			line := fmt.Sprintf("  %s %-5s merged held %d of %d", mark, num(row.At), row.MergedHeld, row.Merged)
			if row.Good > 0 {
				line += fmt.Sprintf(" · known-good held %d of %d", row.GoodHeld, row.Good)
			}
			kinds := slices.Sorted(maps.Keys(row.Caught))
			if len(kinds) > 0 {
				var parts []string
				for _, k := range kinds {
					c := row.Caught[k]
					parts = append(parts, fmt.Sprintf("%s %d of %d", clean(k), c[0], c[1]))
				}
				line += " · known-bad caught: " + strings.Join(parts, ", ")
			}
			b.w("%s", line)
		}
	}
}

// textSafety shows what each veto_at would deny. The configured one is
// starred and carries a 95% upper bound on each known-bad kind's miss rate.
func (r *Report) textSafety(b *textWriter) {
	if len(r.VetoSweeps) == 0 {
		return
	}
	b.w("\nSafety: PRs each veto_at would deny (* is the configured one; every history PR counts as legitimate)")
	for _, sw := range r.VetoSweeps {
		b.w("  %s", sw.Question)
		for _, row := range sw.Rows {
			star := row.At == r.CurrentPol.VetoAt
			mark := " "
			if star {
				mark = "*"
			}
			b.w("  %s %-5s legitimate denied: %s", mark, num(row.At), counts(row.Legit, nil))
			if len(row.Bad) > 0 {
				bound := func(c [2]int) string { return "" }
				if star {
					bound = func(c [2]int) string { return fmt.Sprintf(" (miss ≤ %s)", pct(missBound(c[1]-c[0], c[1]))) }
				}
				b.w("          known-bad caught: %s", counts(row.Bad, bound))
			}
		}
		var low []string
		for _, l := range sw.Lowest {
			low = append(low, fmt.Sprintf("%s %.2f", clean(l.Ref), l.Value))
		}
		if len(low) > 0 {
			b.w("    lowest legitimate: %s", strings.Join(low, ", "))
		}
	}
}

// counts renders "group n of total" for each group, with an optional suffix.
func counts(m map[string][2]int, suffix func([2]int) string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		c := m[k]
		part := fmt.Sprintf("%s %d of %d", clean(k), c[0], c[1])
		if suffix != nil {
			part += suffix(c)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func (r *Report) textReview(b *textWriter) {
	m := r.Current
	if r.Fitted != nil {
		m = *r.Fitted
	}
	if len(m.Disagreements) == 0 && len(r.Failed) == 0 {
		return
	}
	b.w("\nTo review by hand (human labels are noisy; a disagreement isn't always LGTM's mistake)")
	for _, p := range m.Disagreements {
		b.w("  #%d %s: aggregate %.2f, human %s (%s)", p.Number, clean(p.Title), p.Agg, p.Label, clean(p.Reason))
	}
	for _, f := range r.Failed {
		b.w("  #%d not scored: %s", f.Number, clean(f.Err.Error()))
	}
}

// LabelText summarises history labels, for --dry-run.
func LabelText(repo string, mode LabelMode, ss []Sample) string {
	var b textWriter
	pos, neg, low := countLabels(ss)
	b.w("%s · labels %s · %d closed PRs: %d positive, %d negative, %d low-confidence, %d unlabelled",
		repo, mode, len(ss), pos, neg, low, len(ss)-pos-neg-low)
	if pos < minSupport || neg < minNegatives {
		b.w("A fit needs at least %d positive and %d negative PRs, so replay would write nothing.", minSupport, minNegatives)
		if mode == Strict {
			b.w("Try a longer --since, or --labels maintainer if the repo approves outside GitHub reviews (such as Prow) or closes PRs it rejects.")
		}
	}
	for _, s := range ss {
		if s.Label == Unlabelled {
			continue
		}
		label := string(s.Label)
		if s.Low {
			label += ", low-confidence"
		}
		b.w("  #%d %s: %s (%s)", s.PR.Number, clean(s.PR.Title), label, clean(s.Reason))
	}
	return b.String()
}

func countLabels(ss []Sample) (pos, neg, low int) {
	for _, s := range ss {
		switch {
		case s.Label == Unlabelled:
		case s.Low:
			low++
		case s.Label == Positive:
			pos++
		default:
			neg++
		}
	}
	return pos, neg, low
}

func flips(m map[string]int) string {
	var ids []string
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s %d", id, m[id]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  (" + strings.Join(parts, ", ") + ")"
}

func pct(f float64) string {
	if math.IsNaN(f) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

func dur(d time.Duration) string {
	if d == 0 {
		return "n/a"
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}
