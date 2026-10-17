package dataset

import (
	"context"
	"errors"
	"testing"

	"github.com/Temikus/lgtm/internal/jev"
)

type counter struct {
	calls int
	err   error
}

func (c *counter) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &jev.Response{
		ID: "resp", Model: req.Model + "-20260917", CostUSD: 0.002, Usage: jev.Usage{InputTokens: 100},
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

	// A reopened store still has the answer, and a hit costs nothing.
	s = reopen(t, s)
	c = &Cache{Decider: inner, Store: s}
	again, err := c.Decide(context.Background(), request("Is it safe?"))
	if err != nil || inner.calls != 1 {
		t.Fatalf("repeat called the model: calls = %d, err = %v", inner.calls, err)
	}
	if again.CostUSD != 0 || again.Usage.InputTokens != 0 || again.Model != "typesafe/jev-1.13-20260917" || again.Answers["safe"].Noul != 0.93 {
		t.Errorf("cached = %+v", again)
	}

	if _, err := c.Decide(context.Background(), request("Is it safe to merge?")); err != nil || inner.calls != 2 {
		t.Errorf("a changed question should miss: calls = %d, err = %v", inner.calls, err)
	}
	c.Rescore = true
	if _, err := c.Decide(context.Background(), request("Is it safe?")); err != nil || inner.calls != 3 {
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
