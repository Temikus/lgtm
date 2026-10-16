package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const model = "typesafe/jev-1.13"

// fullResponse mirrors the published response shapes: a dated model
// snapshot, probabilities as an object and as an array.
const fullResponse = `{
	"id": "gen-dec-1",
	"model": "typesafe/jev-1.13-20260917",
	"answers": {
		"safe": {"type": "noul", "noul": 0.98},
		"risk": {"type": "choice", "choice": "contained", "probabilities": {"contained": 0.9, "wide": 0.1}, "confidence": 0.8},
		"size": {"type": "score", "score": 0.4, "probabilities": {"0": 0.7, "1": 0.2, "2": 0.1}, "confidence": 0.6, "legend": {"0": "small"}},
		"arr":  {"type": "score", "score": 1.0, "probabilities": [0.1, 0.8, 0.1], "confidence": 0.7}
	},
	"usage": {"input_tokens": 275, "output_tokens": 20, "cost": 0.00003}
}`

func fullQuestions() map[string]Question {
	return map[string]Question{
		"safe": {Type: Noul, Instructions: "Is it safe?"},
		"risk": {Type: Choice, Instructions: "Blast radius?", Choices: map[string]string{"contained": "one module", "wide": "many modules"}},
		"size": {Type: Score, Instructions: "How big?", Levels: []string{"small", "medium", "large"}},
		"arr":  {Type: Score, Instructions: "How big?", Levels: []string{"small", "medium", "large"}},
	}
}

type recorded struct {
	body   map[string]any
	header http.Header
	path   string
}

// server replies with the given status/body pairs in order, repeating the last.
func server(t *testing.T, rec *recorded, replies ...func(w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if rec != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &rec.body)
			rec.header = r.Header.Clone()
			rec.path = r.URL.Path
		}
		replies[min(n, len(replies)-1)](w)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func reply(status int, body string, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func testClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL+"/v1", "test-key", WithHTTPClient(srv.Client()))
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestDecide_EncodesRequest(t *testing.T) {
	var rec recorded
	srv, _ := server(t, &rec, reply(200, fullResponse))

	_, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: map[string]any{"pr": "x"}, Questions: fullQuestions()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if rec.path != "/v1/systemone" {
		t.Errorf("path = %q", rec.path)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
	var wire struct {
		Questions map[string]struct {
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	raw, _ := json.Marshal(rec.body)
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if c := string(wire.Questions["risk"].Criteria); !strings.HasPrefix(c, "{") {
		t.Errorf("choice criteria should be an object: %s", c)
	}
	if c := string(wire.Questions["size"].Criteria); c != `["small","medium","large"]` {
		t.Errorf("score criteria should be an ordered array: %s", c)
	}
	if c := wire.Questions["safe"].Criteria; c != nil {
		t.Errorf("noul without criteria should omit the field: %s", c)
	}
}

func TestDecide_DecodesAnswers(t *testing.T) {
	srv, _ := server(t, nil, reply(200, fullResponse))

	resp, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: fullQuestions()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if resp.Usage.InputTokens != 275 || resp.CostUSD != 0.00003 {
		t.Errorf("usage = %+v cost = %v", resp.Usage, resp.CostUSD)
	}
	if a := resp.Answers["safe"]; a.Noul != 0.98 {
		t.Errorf("safe = %+v", a)
	}
	if a := resp.Answers["risk"]; a.Choice != "contained" || a.Probabilities["contained"] != 0.9 {
		t.Errorf("risk = %+v", a)
	}
	if a := resp.Answers["size"]; a.Score != 0.4 || a.Probabilities["0"] != 0.7 {
		t.Errorf("size = %+v", a)
	}
	if a := resp.Answers["arr"]; a.Probabilities["1"] != 0.8 || len(a.Probabilities) != 3 {
		t.Errorf("array probabilities should be keyed by index: %+v", a)
	}
}

func TestDecide_RetriesOverloadThenSucceeds(t *testing.T) {
	for _, status := range []int{429, 529} {
		srv, calls := server(t, nil, reply(status, `{"error":{"message":"slow down"}}`, "Retry-After", "1"), reply(200, fullResponse))

		if _, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: fullQuestions()}); err != nil {
			t.Fatalf("status %d: Decide: %v", status, err)
		}
		if calls.Load() != 2 {
			t.Errorf("status %d: calls = %d, want 2", status, calls.Load())
		}
	}
}

func TestDecide_GivesUpAfterMaxAttempts(t *testing.T) {
	srv, calls := server(t, nil, reply(429, `{"error":{"message":"rate limited"}}`))

	_, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: fullQuestions()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		t.Fatalf("err = %v, want APIError 429", err)
	}
	if calls.Load() != maxAttempts {
		t.Errorf("calls = %d, want %d", calls.Load(), maxAttempts)
	}
}

func TestDecide_DoesNotRetryValidationError(t *testing.T) {
	srv, calls := server(t, nil, reply(422, `{"error":{"message":"bad question"}}`))

	_, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: fullQuestions()})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestDecide_RejectsBadResponses(t *testing.T) {
	tests := map[string]string{
		"different model":   `{"model":"other/model","answers":{"safe":{"type":"noul","noul":0.9}}}`,
		"prefix not a date": `{"model":"typesafe/jev-1.13x","answers":{"safe":{"type":"noul","noul":0.9}}}`,
		"missing answer":    `{"model":"typesafe/jev-1.13","answers":{}}`,
		"missing noul":      `{"model":"typesafe/jev-1.13","answers":{"safe":{"type":"noul"}}}`,
		"noul out of range": `{"model":"typesafe/jev-1.13","answers":{"safe":{"type":"noul","noul":1.5}}}`,
		"wrong type":        `{"model":"typesafe/jev-1.13","answers":{"safe":{"type":"choice","choice":"a"}}}`,
		"not json":          `<html>`,
	}
	qs := map[string]Question{"safe": {Type: Noul, Instructions: "Is it safe?"}}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv, _ := server(t, nil, reply(200, body))
			if _, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: qs}); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestDecide_RejectsUnknownChoice(t *testing.T) {
	body := `{"model":"typesafe/jev-1.13","answers":{"risk":{"type":"choice","choice":"huge","probabilities":{"contained":0.5,"wide":0.5}}}}`
	srv, _ := server(t, nil, reply(200, body))
	qs := map[string]Question{"risk": fullQuestions()["risk"]}

	if _, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: qs}); err == nil {
		t.Fatal("want error for a choice outside the options")
	}
}

func TestDecide_CapsErrorMessage(t *testing.T) {
	srv, _ := server(t, nil, reply(400, `{"error":{"message":"`+strings.Repeat("x", 1000)+`"}}`))

	_, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: "s", Questions: fullQuestions()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if len(apiErr.Message) > maxErrorLen+3 {
		t.Errorf("message length = %d", len(apiErr.Message))
	}
}

func TestDecide_RefusesOversizedInput(t *testing.T) {
	srv, calls := server(t, nil, reply(200, fullResponse))
	state := strings.Repeat("x", MaxRequestTokens*bytesPerToken)

	_, err := testClient(srv).Decide(context.Background(), Request{Model: model, State: state, Questions: fullQuestions()})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if calls.Load() != 0 {
		t.Error("oversized input must not reach the provider")
	}
}

func TestBaseURL(t *testing.T) {
	tests := []struct {
		provider, override, want string
		wantErr                  bool
	}{
		{provider: "openrouter", want: "https://openrouter.ai/api/v1"},
		{provider: "typesafe", want: "https://api.typesafe.ai/v1"},
		{provider: "", want: "https://openrouter.ai/api/v1"},
		{provider: "evil", wantErr: true},
		{provider: "openrouter", override: "https://proxy.example/v1", want: "https://proxy.example/v1"},
		{provider: "openrouter", override: "https://proxy.example/v1/", want: "https://proxy.example/v1"},
		{provider: "openrouter", override: "http://proxy.example/v1", wantErr: true},
		{provider: "openrouter", override: "proxy.example", wantErr: true},
	}
	for _, tt := range tests {
		got, err := BaseURL(tt.provider, tt.override)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("BaseURL(%q, %q) = %q, %v", tt.provider, tt.override, got, err)
		}
	}
}
