package dataset

import (
	"context"
	"errors"
	"testing"

	"github.com/Temikus/lgtm/internal/jev"
)

type counter struct {
	calls  int
	err    error
	served string // dated suffix; empty means -20260917
}

func (c *counter) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	served := c.served
	if served == "" {
		served = "-20260917"
	}
	return &jev.Response{
		ID: "resp", Model: req.Model + served, CostUSD: 0.002, Usage: jev.Usage{InputTokens: 100},
		Answers: map[string]jev.Answer{"safe": {Type: jev.Noul, Noul: 0.93}},
	}, nil
}

func request(instructions string) jev.Request {
	return jev.Request{
		Model: "typesafe/jev-1.13", State: map[string]any{"files": []string{"a.go"}},
		Questions: map[string]jev.Question{"safe": {Type: jev.Noul, Instructions: instructions}},
	}
}

func TestCache_AnswersRepeatsFromStore(t *testing.T) {
	s, _ := Open(t.TempDir())
	inner := &counter{}
	c := &Cache{Decider: inner, Store: s}
	first, err := c.Decide(context.Background(), request("Is it safe?"))
	if err != nil || first.CostUSD != 0.002 {
		t.Fatalf("first = %+v, %v", first, err)
	}

	// A reopened store still has the answer, and a hit costs nothing. A new
	// Cache asks once first, to learn which version is being served.
	s = reopen(t, s)
	c = &Cache{Decider: inner, Store: s}
	if _, err := c.Decide(context.Background(), request("Is it risky?")); err != nil || inner.calls != 2 {
		t.Fatalf("first call of a run should ask: calls = %d, err = %v", inner.calls, err)
	}
	again, err := c.Decide(context.Background(), request("Is it safe?"))
	if err != nil || inner.calls != 2 {
		t.Fatalf("repeat called the model: calls = %d, err = %v", inner.calls, err)
	}
	if again.CostUSD != 0 || again.Usage.InputTokens != 0 || again.Model != "typesafe/jev-1.13-20260917" || again.Answers["safe"].Noul != 0.93 {
		t.Errorf("cached = %+v", again)
	}

	if _, err := c.Decide(context.Background(), request("Is it safe to merge?")); err != nil || inner.calls != 3 {
		t.Errorf("a changed question should miss: calls = %d, err = %v", inner.calls, err)
	}
	c.Rescore = true
	if _, err := c.Decide(context.Background(), request("Is it safe?")); err != nil || inner.calls != 4 {
		t.Errorf("rescore should call the model: calls = %d", inner.calls)
	}
}

func TestCache_DoesNotStoreErrors(t *testing.T) {
	s, _ := Open(t.TempDir())
	inner := &counter{err: errors.New("429")}
	c := &Cache{Decider: inner, Store: s}
	for range 2 {
		if _, err := c.Decide(context.Background(), request("Is it safe?")); err == nil {
			t.Fatal("want the inner error")
		}
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want the failure retried", inner.calls)
	}
}

// After the provider moves the short name to a new version, replies from the
// old one are asked again, so replay sees the new version and its fit
// checks can catch the change.
func TestCache_IgnoresRepliesFromAnotherVersion(t *testing.T) {
	s, _ := Open(t.TempDir())
	old := &counter{}
	if _, err := (&Cache{Decider: old, Store: s}).Decide(context.Background(), request("Is it safe?")); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	moved := &counter{served: "-20261101"}
	c := &Cache{Decider: moved, Store: s}
	for i, q := range []string{"Is it risky?", "Is it safe?", "Is it safe?"} {
		resp, err := c.Decide(context.Background(), request(q))
		if err != nil || resp.Model != "typesafe/jev-1.13-20261101" {
			t.Fatalf("call %d: %+v, %v", i, resp, err)
		}
	}
	if moved.calls != 2 {
		t.Errorf("calls = %d, want the old-version reply asked again once, then reused", moved.calls)
	}
}
