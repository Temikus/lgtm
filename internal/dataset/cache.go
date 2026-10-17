package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/Temikus/lgtm/internal/jev"
)

// Cache answers a request it has seen from the store, at no cost, and
// stores new replies. Rescore always asks the model. Put it outside any
// budget guard, so hits don't count as spend.
//
// A short model name can move to a new dated version without notice, so a
// stored reply is used only if it came from the version the provider served
// last in this run. The first call through a new Cache always asks.
type Cache struct {
	Decider jev.Decider
	Store   *Store
	Rescore bool

	mu      sync.Mutex
	serving string
}

var _ jev.Decider = (*Cache)(nil)

func (c *Cache) Decide(ctx context.Context, req jev.Request) (*jev.Response, error) {
	key, err := RequestKey(req)
	if err != nil {
		return c.Decider.Decide(ctx, req)
	}
	if !c.Rescore {
		c.mu.Lock()
		serving := c.serving
		c.mu.Unlock()
		if r, ok := c.Store.Response(key); ok && serving != "" && r.Served == serving {
			if resp, err := r.response(); err == nil {
				return resp, nil
			}
		}
	}
	resp, err := c.Decider.Decide(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	c.mu.Lock()
	c.serving = resp.Model
	c.mu.Unlock()
	if row, err := responseRow(key, req.Model, resp); err == nil {
		c.Store.PutResponse(row)
	}
	return resp, nil
}

// RequestKey is the SHA-256 of the request as JSON: model, state, and
// questions, with options in the order sent.
func RequestKey(req jev.Request) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func responseRow(key, model string, r *jev.Response) (Response, error) {
	b, err := json.Marshal(r.Answers)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Key: key, Model: model, Served: r.Model, ID: r.ID, Answers: string(b), CostUSD: r.CostUSD,
		InputTokens: int64(r.Usage.InputTokens), OutputTokens: int64(r.Usage.OutputTokens), CreatedAt: time.Now().UTC(),
	}, nil
}

// response is the stored reply with no usage or cost, since replaying it
// spent nothing.
func (r Response) response() (*jev.Response, error) {
	out := &jev.Response{ID: r.ID, Model: r.Served}
	if err := json.Unmarshal([]byte(r.Answers), &out.Answers); err != nil {
		return nil, err
	}
	return out, nil
}
