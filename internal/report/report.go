// Package report renders a scoring result as a PR comment, check-run
// output, JSON, or terminal text. Every string from the PR, GitHub, or the
// provider goes through Escape (markdown) or clean (terminal).
package report

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

// Marker identifies LGTM's sticky comment. ghout also checks the author,
// since anyone can post this marker.
const Marker = "<!-- lgtm -->"

// PassText never says "looks good": bot output anchors reviewers.
const PassText = "No blocking issues found by LGTM's checks. This doesn't replace human review."

const (
	maxListed   = 20
	maxErrorLen = 200
)

var mdReplacer = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", "@", "&#64;",
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"(", `\(`, ")", `\)`, "#", `\#`, "|", `\|`, "!", `\!`, "~", `\~`,
	"\n", " ", "\r", " ",
)

// Escape makes an untrusted string inert in GitHub markdown: no HTML, links,
// mentions, formatting, or table breaks.
func Escape(s string) string { return mdReplacer.Replace(s) }

// Headline is the verdict in words.
func Headline(v verdict.Verdict) string {
	switch v {
	case verdict.Approve:
		return "no blocking issues"
	case verdict.Deny:
		return "deny"
	}
	return "needs human review"
}

// Title is the check-run title, e.g. "0.94 · approve".
func Title(r *engine.Result) string {
	v := r.Verdict
	if v.Failure != nil {
		return "needs human review (scoring failed)"
	}
	word := string(v.Verdict)
	if v.Verdict == verdict.NeedsHuman {
		word = "needs human review"
	}
	if r.Mode == config.Shadow {
		word = "would have: " + word
	}
	return fmt.Sprintf("%.2f · %s", v.Aggregate, word)
}

// Conclusion maps the verdict to a check-run conclusion. Needs-human is a
// failure because GitHub counts neutral as passing for required checks.
func Conclusion(r *engine.Result) string {
	if r.Mode == config.Shadow {
		return "neutral"
	}
	if r.Verdict.Verdict == verdict.Approve {
		return "success"
	}
	return "failure"
}

// Comment renders the sticky PR comment: the verdict and gate/cap rows up
// front, everything else collapsed.
func Comment(r *engine.Result) string {
	return Marker + "\n" + body(r, false) + "\n<!-- lgtm:sha=" + Escape(r.PR.HeadSHA) + " -->\n"
}

// Summary renders the check-run summary: the same content, nothing collapsed.
func Summary(r *engine.Result) string { return body(r, true) }

func body(r *engine.Result, expanded bool) string {
	v := r.Verdict
	var sections []string
	if r.Mode == config.Shadow {
		sections = append(sections, "### LGTM (shadow) · would have: "+Headline(v.Verdict))
	} else {
		sections = append(sections, "### LGTM · "+Headline(v.Verdict))
	}
	if v.Failure != nil {
		sections = append(sections, "Scoring failed, so a human needs to review this: "+Escape(capLen(v.Failure.Error(), maxErrorLen)))
	} else {
		sections = append(sections,
			fmt.Sprintf("Score **%.2f** (approve at ≥ %.2f, deny at ≤ %.2f)", v.Aggregate, r.ApproveAt, r.DenyAt),
			table(blocking(r)))
	}
	if id := v.Vetoed; id != "" && v.Failure == nil {
		sections = append(sections, fmt.Sprintf("Denied because `%s` is below `veto_at` (%.2f), whatever the score.", Escape(id), r.VetoAt))
	}
	if c := caps(r); c != "" {
		sections = append(sections, c)
	}
	if v.Verdict == verdict.Approve {
		sections = append(sections, PassText)
	}
	if r.Mode == config.Shadow {
		sections = append(sections, "Shadow mode: nothing is gated.")
	}
	if expanded {
		sections = append(sections, "#### Details", details(r))
	} else {
		sections = append(sections, "<details><summary>Details</summary>\n\n"+details(r)+"\n\n</details>")
	}
	return strings.Join(sections, "\n\n") + "\n"
}

// blocking returns gate, veto, and cap questions, lowest value first.
func blocking(r *engine.Result) []engine.QuestionResult {
	var out []engine.QuestionResult
	for _, q := range r.Questions {
		if q.Role != rubric.Info {
			out = append(out, q)
		}
	}
	slices.SortStableFunc(out, func(a, b engine.QuestionResult) int {
		switch {
		case a.Value < b.Value:
			return -1
		case a.Value > b.Value:
			return 1
		}
		return 0
	})
	return out
}

func table(qs []engine.QuestionResult) string {
	var b strings.Builder
	b.WriteString("| Check | | Value |\n|---|---|---|")
	for _, q := range qs {
		name := Escape(q.ID)
		switch q.Role {
		case rubric.Cap:
			name += " (cap)"
		case rubric.Veto:
			name += " (veto)"
		}
		if q.Err != nil {
			fmt.Fprintf(&b, "\n| %s | | n/a |", name)
			continue
		}
		fmt.Fprintf(&b, "\n| %s | `%s` | %.2f |", name, bar(q.Value), q.Value)
	}
	return b.String()
}

func bar(v float64) string {
	n := int(v*10 + 0.5)
	return strings.Repeat("█", n) + strings.Repeat("░", 10-n)
}

var capText = map[verdict.CapKind]string{
	verdict.CapQuestion:     "a cap question is below `cap_at`",
	verdict.PartialView:     "LGTM couldn't see the whole diff",
	verdict.UntrustedAuthor: "the PR is from a fork by an author without write access",
	verdict.ModelChanged:    "the model version changed since calibration; re-run `lgtm replay --write`",
	verdict.Borderline:      "the score is borderline and an answer changed when the options were reordered",
}

func caps(r *engine.Result) string {
	if len(r.Verdict.Caps) == 0 {
		return ""
	}
	lines := []string{"Held at needs-human because:"}
	for _, c := range r.Verdict.Caps {
		lines = append(lines, "- "+capText[c])
	}
	return strings.Join(lines, "\n")
}

func details(r *engine.Result) string {
	var b strings.Builder
	for _, q := range r.Questions {
		if q.Role == rubric.Info {
			fmt.Fprintf(&b, "- %s: %s\n", Escape(q.ID), infoValue(q))
		}
	}
	model := r.Model
	if len(r.Served) > 0 {
		model = strings.Join(r.Served, ", ")
	}
	fmt.Fprintf(&b, "- Model: %s · %d input tokens · $%.5f · %d calls · scored %s\n",
		Escape(model), r.Usage.InputTokens, r.CostUSD, r.Calls, Escape(short(r.PR.HeadSHA)))
	for _, p := range r.Partial {
		fmt.Fprintf(&b, "- Partial view: %s\n", Escape(p))
	}
	if r.TrustErr != nil {
		fmt.Fprintf(&b, "- Author permission check failed: %s\n", Escape(capLen(r.TrustErr.Error(), maxErrorLen)))
	}
	writeFiles(&b, "Not seen", len(r.Unseen), func(i int) string {
		return fmt.Sprintf("%s (%s)", Escape(r.Unseen[i].Path), Escape(r.Unseen[i].Reason))
	})
	writeFiles(&b, "Excluded", len(r.Excluded), func(i int) string {
		f := r.Excluded[i]
		return fmt.Sprintf("%s (+%d −%d)", Escape(f.Path), f.Additions, f.Deletions)
	})
	return strings.TrimSuffix(b.String(), "\n")
}

func writeFiles(b *strings.Builder, label string, n int, item func(int) string) {
	if n == 0 {
		return
	}
	fmt.Fprintf(b, "- %s (%d):", label, n)
	for i := range min(n, maxListed) {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + item(i))
	}
	if n > maxListed {
		fmt.Fprintf(b, ", and %d more", n-maxListed)
	}
	b.WriteString("\n")
}

func infoValue(q engine.QuestionResult) string {
	if q.Err != nil || q.Answer == nil {
		return "n/a"
	}
	switch q.Type {
	case jev.Choice:
		return fmt.Sprintf("%s (%.2f)", Escape(q.Answer.Choice), q.Answer.Probabilities[q.Answer.Choice])
	case jev.Score:
		lvl := int(q.Answer.Score + 0.5)
		if lvl >= 0 && lvl < len(q.Levels) {
			return fmt.Sprintf("%s (%.2f)", Escape(q.Levels[lvl]), q.Value)
		}
	}
	return fmt.Sprintf("%.2f", q.Value)
}

// JSONResult is the --json output.
type JSONResult struct {
	Repo      string         `json:"repo"`
	Number    int            `json:"number"`
	HeadSHA   string         `json:"head_sha"`
	Mode      string         `json:"mode"`
	Model     string         `json:"model"`
	Served    []string       `json:"served,omitempty"`
	Verdict   string         `json:"verdict"`
	Raw       string         `json:"raw_verdict"`
	Aggregate float64        `json:"aggregate"`
	ApproveAt float64        `json:"approve_at"`
	DenyAt    float64        `json:"deny_at"`
	Caps      []string       `json:"caps,omitempty"`
	Failure   string         `json:"failure,omitempty"`
	Questions []JSONQuestion `json:"questions"`
	Partial   []string       `json:"partial,omitempty"`
	Unseen    []string       `json:"unseen,omitempty"`
	Excluded  []string       `json:"excluded,omitempty"`
	Untrusted bool           `json:"untrusted"`
	Tokens    int            `json:"input_tokens"`
	CostUSD   float64        `json:"cost_usd"`
	Calls     int            `json:"calls"`
}

// JSONQuestion is one question in JSONResult.
type JSONQuestion struct {
	ID     string      `json:"id"`
	Role   string      `json:"role"`
	Type   string      `json:"type"`
	Value  *float64    `json:"value"`
	Answer *jev.Answer `json:"answer,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// JSON renders the result for scripts.
func JSON(r *engine.Result) ([]byte, error) {
	out := JSONResult{
		Repo: r.PR.Owner + "/" + r.PR.Repo, Number: r.PR.Number, HeadSHA: r.PR.HeadSHA,
		Mode: string(r.Mode), Model: r.Model, Served: r.Served, Verdict: string(r.Verdict.Verdict), Raw: string(r.Verdict.Raw),
		Aggregate: r.Verdict.Aggregate, ApproveAt: r.ApproveAt, DenyAt: r.DenyAt,
		Partial: r.Partial, Untrusted: r.Untrusted, Tokens: r.Usage.InputTokens, CostUSD: r.CostUSD, Calls: r.Calls,
	}
	for _, c := range r.Verdict.Caps {
		out.Caps = append(out.Caps, string(c))
	}
	if r.Verdict.Failure != nil {
		out.Failure = r.Verdict.Failure.Error()
	}
	for _, q := range r.Questions {
		jq := JSONQuestion{ID: q.ID, Role: string(q.Role), Type: string(q.Type), Answer: q.Answer}
		if q.Err != nil {
			jq.Error = q.Err.Error()
		} else {
			v := q.Value
			jq.Value = &v
		}
		out.Questions = append(out.Questions, jq)
	}
	for _, u := range r.Unseen {
		out.Unseen = append(out.Unseen, u.Path)
	}
	for _, f := range r.Excluded {
		out.Excluded = append(out.Excluded, f.Path)
	}
	return json.MarshalIndent(out, "", "  ")
}

// Text renders a terminal table. Untrusted strings lose control characters
// so they can't drive the terminal.
func Text(r *engine.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s#%d @ %s\n", clean(r.PR.Owner), clean(r.PR.Repo), r.PR.Number, short(r.PR.HeadSHA))
	fmt.Fprintf(&b, "verdict: %s (%s mode)\n", r.Verdict.Verdict, r.Mode)
	if r.Verdict.Failure != nil {
		fmt.Fprintf(&b, "failure: %s\n", clean(r.Verdict.Failure.Error()))
	} else {
		fmt.Fprintf(&b, "score:   %.2f (approve >= %.2f, deny <= %.2f)\n", r.Verdict.Aggregate, r.ApproveAt, r.DenyAt)
	}
	for _, c := range r.Verdict.Caps {
		fmt.Fprintf(&b, "capped:  %s\n", capText[c])
	}
	b.WriteString("\n")
	for _, q := range r.Questions {
		val := "n/a"
		if q.Err == nil {
			val = fmt.Sprintf("%.2f", q.Value)
		}
		fmt.Fprintf(&b, "  %-16s %-5s %s %s\n", clean(q.ID), q.Role, bar(q.Value), val)
	}
	fmt.Fprintf(&b, "\nmodel %s · %d input tokens · $%.5f · %d calls\n", clean(r.Model), r.Usage.InputTokens, r.CostUSD, r.Calls)
	for _, p := range r.Partial {
		fmt.Fprintf(&b, "partial: %s\n", clean(p))
	}
	for _, u := range r.Unseen {
		fmt.Fprintf(&b, "not seen: %s (%s)\n", clean(u.Path), clean(u.Reason))
	}
	for _, f := range r.Excluded {
		fmt.Fprintf(&b, "excluded: %s\n", clean(f.Path))
	}
	return b.String()
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func capLen(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
