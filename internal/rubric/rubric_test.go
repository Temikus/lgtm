package rubric

import (
	"math"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/jev"
)

func ptr[T any](v T) *T { return &v }

func byID(t *testing.T, qs []Question, id string) Question {
	t.Helper()
	for _, q := range qs {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("question %q not found", id)
	return Question{}
}

func TestDefaults_MatchPRDTable(t *testing.T) {
	want := map[string]struct {
		typ  jev.QuestionType
		role Role
		ctx  Context
	}{
		"aligned":       {jev.Noul, Gate, Full},
		"scoped":        {jev.Noul, Gate, Full},
		"malicious":     {jev.Noul, Veto, Diff},
		"hygiene":       {jev.Noul, Cap, Diff},
		"breaks":        {jev.Noul, Cap, Diff},
		"tested":        {jev.Noul, Info, Diff},
		"injection":     {jev.Noul, Cap, Full},
		"described":     {jev.Noul, Info, Full},
		"risk":          {jev.Choice, Info, Full},
		"reviewability": {jev.Score, Info, Full},
	}
	qs := Defaults()
	if len(qs) != len(want) {
		t.Fatalf("got %d defaults, want %d", len(qs), len(want))
	}
	for _, q := range qs {
		w, ok := want[q.ID]
		if !ok {
			t.Errorf("unexpected default %q", q.ID)
			continue
		}
		if q.Type != w.typ || q.Role != w.role || q.Context != w.ctx {
			t.Errorf("%s = %s/%s/%s, want %s/%s/%s", q.ID, q.Type, q.Role, q.Context, w.typ, w.role, w.ctx)
		}
	}
	if err := Validate(qs); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

// tested points the wrong way on reviewed history, so it doesn't gate by
// default; a repo can make it a gate again.
func TestMerge_TestedCanGateAgain(t *testing.T) {
	qs, err := Merge(Defaults(), map[string]Override{"tested": {Gate: ptr(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if q := byID(t, qs, "tested"); q.Role != Gate {
		t.Errorf("tested role = %s, want gate", q.Role)
	}
}

func TestMerge_Veto(t *testing.T) {
	qs, err := Merge(Defaults(), map[string]Override{"described": {Veto: ptr(true)}, "malicious": {Veto: ptr(false)}})
	if err != nil {
		t.Fatal(err)
	}
	if q := byID(t, qs, "described"); q.Role != Veto || !q.Role.Gating() {
		t.Errorf("described role = %s, want veto", q.Role)
	}
	if q := byID(t, qs, "malicious"); q.Role != Info {
		t.Errorf("malicious with veto = false: role %s, want info", q.Role)
	}
}

func TestDefaults_DiffLocalSet(t *testing.T) {
	for _, q := range Defaults() {
		want := q.ID == "malicious" || q.ID == "hygiene" || q.ID == "breaks" || q.ID == "scoped" || q.ID == "injection"
		if q.DiffLocal != want {
			t.Errorf("%s DiffLocal = %v, want %v", q.ID, q.DiffLocal, want)
		}
	}
}

func TestMerge(t *testing.T) {
	qs, err := Merge(Defaults(), map[string]Override{
		"described": {Gate: ptr(true)},
		"tested":    {Enabled: ptr(false)},
		"malicious": {Instructions: ptr("Custom text.")},
		"hygiene":   {CapAt: ptr(0.6)},
		"migration_safe": {
			Type:         ptr("noul"),
			Gate:         ptr(true),
			Instructions: ptr("If the diff adds a database migration, can it run without locking large tables?"),
		},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if q := byID(t, qs, "described"); q.Role != Gate {
		t.Errorf("described role = %s, want gate", q.Role)
	}
	for _, q := range qs {
		if q.ID == "tested" {
			t.Error("disabled question still present")
		}
	}
	if q := byID(t, qs, "malicious"); q.Instructions != "Custom text." || q.Context != Diff {
		t.Errorf("malicious = %+v; override should keep other fields", q)
	}
	if q := byID(t, qs, "hygiene"); q.CapAt != 0.6 {
		t.Errorf("hygiene cap_at = %v, want 0.6", q.CapAt)
	}
	if q := byID(t, qs, "migration_safe"); q.Role != Gate || q.Context != Full || q.Type != jev.Noul {
		t.Errorf("migration_safe = %+v", q)
	}
	if qs[len(qs)-1].ID != "migration_safe" {
		t.Error("added questions should follow the defaults")
	}
}

func TestMerge_Errors(t *testing.T) {
	tests := map[string]map[string]Override{
		"new without type":     {"x": {Instructions: ptr("x?")}},
		"new without text":     {"x": {Type: ptr("noul")}},
		"gate and cap":         {"described": {Gate: ptr(true), Cap: ptr(true)}},
		"veto and gate":        {"described": {Veto: ptr(true), Gate: ptr(true)}},
		"cap_at at one":        {"hygiene": {CapAt: ptr(1.0)}},
		"bad context":          {"described": {Context: ptr("everything")}},
		"bad type":             {"x": {Type: ptr("maybe"), Instructions: ptr("x?")}},
		"choice gate no pass":  {"risk": {Gate: ptr(true), Pass: []string{}}},
		"pass not an option":   {"risk": {Pass: []string{"tiny"}}},
		"options on noul":      {"described": {Options: []jev.Option{{Name: "a"}, {Name: "b"}}}},
		"retype without shape": {"described": {Type: ptr("score")}},
	}
	for name, ov := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Merge(Defaults(), ov); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestNormalise(t *testing.T) {
	noul := Question{ID: "n", Question: jev.Question{Type: jev.Noul}}
	score := Question{ID: "s", Question: jev.Question{Type: jev.Score, Levels: []string{"a", "b", "c", "d", "e"}}}
	inv := score
	inv.Invert = true
	choice := Question{ID: "c", Question: jev.Question{Type: jev.Choice, Choices: []jev.Option{{Name: "contained"}, {Name: "moderate"}, {Name: "wide"}}}, Pass: []string{"contained", "moderate"}}

	tests := []struct {
		name string
		q    Question
		a    jev.Answer
		want float64
	}{
		{"noul is p", noul, jev.Answer{Noul: 0.83}, 0.83},
		{"score top", score, jev.Answer{Score: 4}, 1},
		{"score mid", score, jev.Answer{Score: 2}, 0.5},
		{"score inverted", inv, jev.Answer{Score: 1}, 0.75},
		{"choice pass mass", choice, jev.Answer{Choice: "wide", Probabilities: map[string]float64{"contained": 0.3, "moderate": 0.25, "wide": 0.45}}, 0.55},
		{"choice no probs pass", choice, jev.Answer{Choice: "moderate"}, 1},
		{"choice no probs fail", choice, jev.Answer{Choice: "wide"}, 0},
	}
	for _, tt := range tests {
		got, err := tt.q.Normalise(tt.a)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tt.name, got, tt.want)
		}
	}

	noPass := choice
	noPass.Pass = nil
	if _, err := noPass.Normalise(jev.Answer{Choice: "wide"}); err == nil || !strings.Contains(err.Error(), "pass") {
		t.Errorf("choice without pass should error, got %v", err)
	}
}

func TestReversed(t *testing.T) {
	q := byID(t, Defaults(), "risk")
	r := q.Reversed()
	if r.Choices[0].Name != "wide" || r.Choices[2].Name != "contained" {
		t.Errorf("reversed = %v", r.Choices)
	}
	if q.Choices[0].Name != "contained" {
		t.Error("Reversed must not mutate the original")
	}
}
