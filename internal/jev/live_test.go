//go:build integration

package jev

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestDecide_Live is the drift check the httptest fixtures can't be: the
// decisions API is alpha. Run with `just test integration`.
func TestDecide_Live(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if os.Getenv("LGTM_INTEGRATION") != "1" || key == "" {
		t.Skip("needs LGTM_INTEGRATION=1 and OPENROUTER_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base, err := BaseURL("openrouter", "")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := NewClient(base, key).Decide(ctx, Request{
		Model: model,
		State: map[string]any{"message": "I was charged twice for my subscription."},
		Questions: map[string]Question{
			"refund": {Type: Noul, Instructions: "Is `message` asking for money back?"},
			"team":   {Type: Choice, Instructions: "Which team should handle `message`?", Choices: []Option{{"billing", "Charges and refunds"}, {"technical", "Bugs and outages"}}},
			"size":   {Type: Score, Instructions: "How urgent is `message`?", Levels: []string{"low", "medium", "high"}},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	t.Logf("model=%s usage=%+v cost=%v", resp.Model, resp.Usage, resp.CostUSD)
	if a := resp.Answers["team"]; a.Choice != "billing" || len(a.Probabilities) != 2 {
		t.Errorf("team = %+v, want billing with 2 probabilities", a)
	}
	if a := resp.Answers["size"]; len(a.Probabilities) != 3 {
		t.Errorf("size = %+v, want 3 level probabilities", a)
	}
	if resp.Usage.InputTokens == 0 {
		t.Errorf("usage not decoded: %+v", resp.Usage)
	}
}

// TestDecide_LiveDatedSnapshot checks a config can pin the exact version the
// short name points at, as the PRD suggests for an exact pin.
func TestDecide_LiveDatedSnapshot(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if os.Getenv("LGTM_INTEGRATION") != "1" || key == "" {
		t.Skip("needs LGTM_INTEGRATION=1 and OPENROUTER_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base, err := BaseURL("openrouter", "")
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(base, key)
	req := Request{Model: model, State: map[string]any{"message": "hi"}, Questions: map[string]Question{"greeting": {Type: Noul, Instructions: "Is `message` a greeting?"}}}
	resp, err := c.Decide(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Model = resp.Model
	pinned, err := c.Decide(ctx, req)
	t.Logf("short name served %s; requesting it gave %v, err %v", resp.Model, modelOf(pinned), err)
	if err != nil {
		t.Fatalf("dated versions can't be requested: %v", err)
	}
	if pinned.Model != resp.Model {
		t.Errorf("asked for %s, served %s", resp.Model, pinned.Model)
	}
}

func modelOf(r *Response) string {
	if r == nil {
		return ""
	}
	return r.Model
}
