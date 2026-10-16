package replay

import (
	"context"
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

// Budget stops calls before spend passes LimitUSD. Each call reserves the
// average cost so far, so concurrent calls can't all slip under the limit.
type Budget struct {
	Decider  jev.Decider
	LimitUSD float64

	mu       sync.Mutex
	spent    float64
	reserved float64
	calls    int
}

var _ jev.Decider = (*Budget)(nil)

func (b *Budget) Decide(ctx context.Context, req jev.Request) (*jev.Response, error) {
	b.mu.Lock()
	est := 0.0
	if b.calls > 0 {
		est = b.spent / float64(b.calls)
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
	}
	return resp, err
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
