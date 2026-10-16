package replay

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/Temikus/lgtm/internal/jev"
)

// ErrBudget means the next call could take spend past --budget-usd.
var ErrBudget = errors.New("replay budget reached")

// fallbackUSDPerMTok prices input tokens when a reply reports no cost. It is
// about twice Jev 1.13's listed $0.042 per million, so the guard stops early
// rather than late.
const fallbackUSDPerMTok = 0.10

// Budget stops calls before spend passes LimitUSD. Before each call it
// reserves that request's estimated cost: its size in tokens at the dearest
// price seen so far (or the fallback price), and never less than the average
// call so far. So neither the first call nor calls running side by side can
// take spend past the limit.
type Budget struct {
	Decider  jev.Decider
	LimitUSD float64

	mu       sync.Mutex
	spent    float64
	tokens   int
	calls    int
	reserved float64
}

var _ jev.Decider = (*Budget)(nil)

func (b *Budget) Decide(ctx context.Context, req jev.Request) (*jev.Response, error) {
	b.mu.Lock()
	est := float64(requestTokens(req)) * b.price()
	if b.calls > 0 {
		est = max(est, b.spent/float64(b.calls))
	}
	if b.spent+b.reserved+est > b.LimitUSD {
		b.mu.Unlock()
		return nil, ErrBudget
	}
	b.reserved += est
	b.mu.Unlock()

	resp, err := b.Decider.Decide(ctx, req)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= est
	b.calls++
	if resp != nil {
		b.spent += cost(resp)
		b.tokens += resp.Usage.InputTokens
	}
	return resp, err
}

// price is USD per input token. Callers hold mu.
func (b *Budget) price() float64 {
	p := fallbackUSDPerMTok / 1e6
	if b.tokens > 0 {
		p = max(p, b.spent/float64(b.tokens))
	}
	return p
}

// Spent is the cost of the calls made so far.
func (b *Budget) Spent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

func cost(r *jev.Response) float64 {
	if r.CostUSD > 0 {
		return r.CostUSD
	}
	return float64(r.Usage.InputTokens) * fallbackUSDPerMTok / 1e6
}

// requestTokens estimates a request's input tokens at 3 bytes per token,
// the same conservative rate the client uses for its size limit.
func requestTokens(req jev.Request) int {
	n := 0
	if b, err := json.Marshal(req.State); err == nil {
		n += len(b)
	}
	for id, q := range req.Questions {
		n += len(id) + len(q.Instructions)
		for _, o := range q.Choices {
			n += len(o.Name) + len(o.Definition)
		}
		for _, l := range q.Levels {
			n += len(l)
		}
	}
	return (n + 2) / 3
}
