package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Provider base URLs. Config can only pick one of these by name; a custom
// URL comes only from the environment (see BaseURL).
var providerURLs = map[string]string{
	"openrouter": "https://openrouter.ai/api/v1",
	"typesafe":   "https://api.typesafe.ai/v1",
}

const (
	maxAttempts  = 5
	maxBodyBytes = 1 << 20
	maxErrorLen  = 200
	maxBackoff   = 30 * time.Second
)

// ErrValidation is returned for HTTP 422: the provider rejected the request
// shape. Retrying cannot help.
var ErrValidation = errors.New("decision request failed validation")

// APIError is a non-200 reply. Message is the provider's short error text,
// capped at maxErrorLen; it is untrusted and must be escaped before display.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("decision API: HTTP %d: %s", e.Status, e.Message)
}

func (e *APIError) Unwrap() error {
	if e.Status == http.StatusUnprocessableEntity {
		return ErrValidation
	}
	return nil
}

// BaseURL resolves the API base for a provider name. A non-empty override
// (from LGTM_BASE_URL or the Action's base-url input, never from repo config)
// replaces it and must be https.
func BaseURL(provider, override string) (string, error) {
	if override != "" {
		u, err := url.Parse(override)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", fmt.Errorf("base URL %q must be an absolute https:// URL", override)
		}
		return strings.TrimRight(override, "/"), nil
	}
	if provider == "" {
		provider = "openrouter"
	}
	base, ok := providerURLs[provider]
	if !ok {
		return "", fmt.Errorf("unknown provider %q (want openrouter or typesafe)", provider)
	}
	return base, nil
}

// Client calls the System One endpoint.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	sleep   func(context.Context, time.Duration) error
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(h *http.Client) ClientOption { return func(c *Client) { c.http = h } }

// NewClient returns a Client for baseURL (see BaseURL).
func NewClient(baseURL, apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
		sleep:   sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ Decider = (*Client)(nil)

// Decide validates the questions, refuses oversized input, posts the request
// with bounded retries on 429/529, and checks the response answers every
// question with the right shape from the requested model.
func (c *Client) Decide(ctx context.Context, req Request) (*Response, error) {
	if err := ValidateQuestions(req.Questions); err != nil {
		return nil, err
	}
	body, err := json.Marshal(encodeRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshaling decision request: %w", err)
	}
	if est := (len(body) + bytesPerToken - 1) / bytesPerToken; est > MaxRequestTokens {
		return nil, fmt.Errorf("~%d tokens > %d: %w", est, MaxRequestTokens, ErrTooLarge)
	}

	var raw []byte
	for attempt := 1; ; attempt++ {
		var retryAfter time.Duration
		raw, retryAfter, err = c.post(ctx, body)
		if err == nil {
			break
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !retryable(apiErr.Status) || attempt == maxAttempts {
			return nil, err
		}
		if err := c.sleep(ctx, backoff(attempt, retryAfter)); err != nil {
			return nil, err
		}
	}

	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing decision response: %w", err)
	}
	return out.decode(req)
}

func (c *Client) post(ctx context.Context, body []byte) ([]byte, time.Duration, error) {
	// #nosec G704 -- baseURL is a hardcoded provider URL or an https override from the environment; see BaseURL.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("creating decision request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("HTTP-Referer", "https://github.com/Temikus/lgtm")
	httpReq.Header.Set("X-Title", "LGTM")

	resp, err := c.http.Do(httpReq) // #nosec G704 -- see above
	if err != nil {
		return nil, 0, fmt.Errorf("sending decision request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("reading decision response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseRetryAfter(resp.Header.Get("Retry-After")), &APIError{Status: resp.StatusCode, Message: errorMessage(resp.StatusCode, raw)}
	}
	return raw, 0, nil
}

// 529 is the provider's "overloaded" status.
func retryable(status int) bool { return status == http.StatusTooManyRequests || status == 529 }

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// backoff honours Retry-After, else doubles from 1s with full jitter.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, maxBackoff)
	}
	ceiling := min(time.Second<<(attempt-1), maxBackoff)
	return time.Duration(rand.Int64N(int64(ceiling)) + 1) // #nosec G404 -- jitter, not security
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errorMessage keeps only the provider's short error.message, capped. The
// raw body could echo the request state.
func errorMessage(status int, body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = strings.TrimSpace(e.Error.Message)
	}
	if msg == "" {
		return http.StatusText(status)
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen] + "..."
	}
	return msg
}

type wireRequest struct {
	Model     string                  `json:"model"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// encodeRequest maps questions onto the wire shape, where criteria is an
// object for choice and noul but an ordered array for score.
func encodeRequest(req Request) wireRequest {
	out := wireRequest{Model: req.Model, State: req.State, Questions: make(map[string]wireQuestion, len(req.Questions))}
	for id, q := range req.Questions {
		wq := wireQuestion{Type: string(q.Type), Instructions: q.Instructions}
		switch {
		case q.Type == Score && len(q.Levels) > 0:
			wq.Criteria = q.Levels
		case q.Type != Score && len(q.Choices) > 0:
			wq.Criteria = orderedOptions(q.Choices)
		}
		out.Questions[id] = wq
	}
	return out
}

// orderedOptions marshals as a JSON object in slice order. encoding/json
// sorts map keys, which would defeat the option-reversal consistency check.
type orderedOptions []Option

func (o orderedOptions) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, opt := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(opt.Name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(opt.Definition)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type wireResponse struct {
	ID      string                `json:"id"`
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		Usage
		Cost float64 `json:"cost"`
	} `json:"usage"`
}

type wireAnswer struct {
	Type          string          `json:"type"`
	Choice        string          `json:"choice"`
	Probabilities json.RawMessage `json:"probabilities"`
	Confidence    float64         `json:"confidence"`
	Noul          *float64        `json:"noul"`
	Score         float64         `json:"score"`
}

func (r *wireResponse) decode(req Request) (*Response, error) {
	if !sameModel(req.Model, r.Model) {
		return nil, fmt.Errorf("response model %q does not match requested %q", r.Model, req.Model)
	}
	out := &Response{ID: r.ID, Model: r.Model, Usage: r.Usage.Usage, CostUSD: r.Usage.Cost, Answers: make(map[string]Answer, len(req.Questions))}
	for _, id := range SortedIDs(req.Questions) {
		wa, ok := r.Answers[id]
		if !ok {
			return nil, fmt.Errorf("no answer for question %q", id)
		}
		a, err := wa.decode(req.Questions[id])
		if err != nil {
			return nil, fmt.Errorf("answer %q: %w", id, err)
		}
		out.Answers[id] = a
	}
	return out, nil
}

// sameModel accepts the requested ID or a dated snapshot of it
// ("typesafe/jev-1.13" -> "typesafe/jev-1.13-20260917").
func sameModel(requested, got string) bool {
	if got == requested {
		return true
	}
	suffix, ok := strings.CutPrefix(got, requested+"-")
	if !ok || len(suffix) != 8 {
		return false
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}

func (a wireAnswer) decode(q Question) (Answer, error) {
	if QuestionType(a.Type) != q.Type {
		return Answer{}, fmt.Errorf("type %q, want %q", a.Type, q.Type)
	}
	probs, err := decodeProbabilities(a.Probabilities)
	if err != nil {
		return Answer{}, err
	}
	for k, p := range probs {
		if !unit(p) {
			return Answer{}, fmt.Errorf("probability %q = %v outside [0,1]", k, p)
		}
	}
	out := Answer{Type: q.Type, Choice: a.Choice, Probabilities: probs, Confidence: a.Confidence, Score: a.Score}
	switch q.Type {
	case Noul:
		// A missing noul must not read as p=0, a confident "false".
		if a.Noul == nil || !unit(*a.Noul) {
			return Answer{}, errors.New("noul probability missing or outside [0,1]")
		}
		out.Noul = *a.Noul
	case Choice:
		if !q.Has(a.Choice) {
			return Answer{}, fmt.Errorf("choice %q is not one of the options", a.Choice)
		}
	case Score:
		if math.IsNaN(a.Score) || a.Score < 0 || a.Score > float64(len(q.Levels)-1) {
			return Answer{}, fmt.Errorf("score %v outside [0,%d]", a.Score, len(q.Levels)-1)
		}
	}
	return out, nil
}

func unit(p float64) bool { return p >= 0 && p <= 1 }

// decodeProbabilities accepts an object keyed by option or level, or an
// array indexed by level (keyed "0", "1", ...).
func decodeProbabilities(raw json.RawMessage) (map[string]float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err == nil {
		return m, nil
	}
	var arr []float64
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("probabilities: want object or array, got %.40s", raw)
	}
	m = make(map[string]float64, len(arr))
	for i, p := range arr {
		m[strconv.Itoa(i)] = p
	}
	return m, nil
}
