package replay

import (
	"fmt"
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
func (r *Report) Text(target float64) string {
	var b textWriter
	r.textHeader(&b)
	r.textFit(&b, target)
	r.textVerdicts(&b)
	r.textReliability(&b)
	r.textProbe(&b)
	r.textPerturb(&b)
	r.textShadow(&b)
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
	if r.Labels == Merged {
		b.w("Labels come from merges, not reviews, so they say what shipped, not what a reviewer judged.")
	}
}

func (r *Report) textFit(b *textWriter, target float64) {
	b.w("\nThreshold fit (target precision %.2f)", target)
	if c := r.Calibration; c != nil {
		b.w("  approve_at %s (precision %.2f) · deny_at %s (precision %.2f) · from %d PRs · %s",
			num(c.ApproveAt), r.Fit.ApprovePrecision, num(c.DenyAt), r.Fit.DenyPrecision, r.Fit.NLabelled, c.Snapshot)
		return
	}
	b.w("  Nothing to write: %s.", r.WhyNot)
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
	if pos < minSupport || neg < minSupport {
		b.w("A fit needs at least %d positive and %d negative PRs, so replay would write nothing.", minSupport, minSupport)
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
