package replay

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
)

// model answers every noul 0.97, except `injection`, which drops to 0.2
// when it sees "SEC-123". For choice questions it picks the option whose
// definition starts with "One" (byName false) or the option named
// "single" (byName true).
type model struct {
	mu     sync.Mutex
	byName bool
	calls  int
}

func (m *model) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	st, _ := json.Marshal(req.State)
	out := &jev.Response{Model: req.Model, Answers: map[string]jev.Answer{}, CostUSD: 0.001}
	for id, q := range req.Questions {
		a := jev.Answer{Type: q.Type, Noul: 0.97}
		switch q.Type {
		case jev.Noul:
			if id == "injection" && strings.Contains(string(st), "SEC-123") {
				a.Noul = 0.2
			}
		case jev.Choice:
			for _, o := range q.Choices {
				if (m.byName && o.Name == "single") || (!m.byName && strings.HasPrefix(o.Definition, "One")) {
					a.Choice = o.Name
				}
			}
			a.Probabilities = map[string]float64{a.Choice: 0.9}
		case jev.Score:
			a.Score = float64(len(q.Levels) - 1)
		}
		out.Answers[id] = a
	}
	return out, nil
}

const choiceGate = `
[questions.lang]
type = "choice"
gate = true
instructions = "Is the diff in one language?"
options = [{name = "single", definition = "One language"}, {name = "mixed", definition = "Several languages"}]
pass = ["single"]
`

func snapshot() *prstate.Snapshot {
	return &prstate.Snapshot{
		PR: prstate.PR{Owner: "o", Repo: "r", Number: 1, BaseSHA: "b", HeadSHA: "h", Title: "Add retry", Body: "Retries 429s.\n\n- honours Retry-After"},
		Files: []prstate.File{
			{Path: "retry.go", Status: "modified", Additions: 3, Patch: "@@ -1,2 +1,3 @@\n package x\n+func retry() {}\n@@ -9,1 +10,2 @@\n+// done\n"},
			{Path: "retry_test.go", Status: "added", Additions: 1, Patch: "@@ -0,0 +1,1 @@\n+func TestRetry() {}\n"},
		},
	}
}

func opts(t *testing.T, toml string, d jev.Decider) engine.Options {
	t.Helper()
	cfg, err := config.Load([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	return engine.Options{Config: cfg, Decider: d}
}

func TestShuffleHunks_KeepsHunksAndFiles(t *testing.T) {
	s := snapshot()
	got := shuffleHunks(s)
	if got.PR.Title != "" || got.PR.Body != "" || len(got.Files) != 2 {
		t.Fatalf("shuffled = %+v", got)
	}
	var before, after []string
	for _, f := range s.Files {
		before = append(before, splitHunks(f.Patch)...)
	}
	for _, f := range got.Files {
		after = append(after, splitHunks(f.Patch)...)
	}
	slices.Sort(before)
	slices.Sort(after)
	if len(before) != 3 || !slices.Equal(before, after) {
		t.Errorf("hunks before %q, after %q", before, after)
	}
	if s.Files[0].Patch == "" {
		t.Error("shuffling changed the input")
	}
}

func TestProbe_WarnsOnHighPrior(t *testing.T) {
	o := opts(t, "", &model{})
	got := Probe(context.Background(), nil, o, snapshot(), 0.9)
	if len(got) == 0 {
		t.Fatal("no baselines")
	}
	for _, b := range got {
		if b.Err != nil {
			t.Errorf("%s: %v", b.ID, b.Err)
		}
		if wantWarn := b.Role != "info"; b.Warn != wantWarn {
			t.Errorf("%s (%s): warn = %v at prior %.2f", b.ID, b.Role, b.Warn, b.Empty)
		}
	}
}

func TestPerturb(t *testing.T) {
	m := &model{}
	o := opts(t, choiceGate, m)
	base, edits := Perturb(context.Background(), nil, o, []*prstate.Snapshot{snapshot(), snapshot()}, o.Config.Policy())
	if base.N != 2 || base.VerdictFlips != 0 {
		t.Errorf("baseline = %+v", base)
	}
	byName := map[string]FlipRate{}
	for _, e := range edits {
		byName[e.Name] = e
	}
	if e := byName["fake authority claim"]; e.QuestionFlips["injection"] != 2 || e.VerdictFlips != 2 {
		t.Errorf("authority claim = %+v, want injection to flip on both PRs", e)
	}
	for _, name := range []string{"reworded description", "unrelated distractor hunk", "choice options reversed", "choice names swapped"} {
		if e := byName[name]; e.N != 2 || e.VerdictFlips != 0 {
			t.Errorf("%s = %+v, want no flips from a definition-reading model", name, e)
		}
	}

	// A model that follows option names flips when names and definitions
	// are swapped.
	o = opts(t, choiceGate, &model{byName: true})
	_, edits = Perturb(context.Background(), nil, o, []*prstate.Snapshot{snapshot()}, o.Config.Policy())
	for _, e := range edits {
		if e.Name == "choice names swapped" && e.QuestionFlips["lang"] != 1 {
			t.Errorf("names swapped = %+v, want lang to flip", e)
		}
	}
}

func TestEdits_DontChangeInput(t *testing.T) {
	s := snapshot()
	want, _ := json.Marshal(s)
	for _, e := range Edits {
		if e.PR != nil {
			e.PR(s)
		}
	}
	if got, _ := json.Marshal(s); string(got) != string(want) {
		t.Errorf("edits changed the snapshot:\n%s", got)
	}
}
