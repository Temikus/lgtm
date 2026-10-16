// Package jev is a client for System One decision models such as TypeSafe's
// Jev, served at POST {base}/systemone by OpenRouter and TypeSafe.
//
// Types, validation and wire encoding are adapted from Denkeeper
// (github.com/Temikus/denkeeper, internal/llm), Apache-2.0.
package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// QuestionType is the answer shape a decision model is asked for.
type QuestionType string

const (
	Choice QuestionType = "choice" // one of 2-255 named options
	Noul   QuestionType = "noul"   // probability the statement is true
	Score  QuestionType = "score"  // 2-10 ordered levels
)

const (
	maxChoiceOptions = 255
	minScoreLevels   = 2
	maxScoreLevels   = 10
)

// Upstream limits: state plus any one question, and the whole request.
const (
	MaxQuestionTokens = 32_000
	MaxRequestTokens  = 64_000
)

// bytesPerToken is deliberately low: JSON is punctuation-heavy, and an
// estimate under the limit should not be rejected upstream.
const bytesPerToken = 3

// ErrTooLarge is returned before any call when the input would exceed
// MaxRequestTokens. Input is never truncated.
var ErrTooLarge = errors.New("decision input exceeds the request token limit")

// Question is one typed question put to a decision model.
type Question struct {
	Type         QuestionType
	Instructions string
	Choices      map[string]string // choice options; for noul, optional "true"/"false" definitions
	Levels       []string          // score levels, worst to best
}

// Answer is the model's answer to one Question. Probabilities are keyed by
// option name for choice and by level index ("0", "1", ...) for score. Score
// is the expected level index, so it lies in [0, len(Levels)-1].
type Answer struct {
	Type          QuestionType       `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
}

// Request asks Model the Questions about State, which is any JSON value.
type Request struct {
	Model     string
	State     any
	Questions map[string]Question
}

// Usage is the token usage the provider reported.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response carries the answers keyed by question ID.
type Response struct {
	ID      string
	Model   string
	Answers map[string]Answer
	Usage   Usage
	CostUSD float64
}

// Decider answers decision requests. Client implements it; tests may
// substitute a scripted fake.
type Decider interface {
	Decide(ctx context.Context, req Request) (*Response, error)
}

// ValidateQuestions checks question shapes against the API limits so a
// malformed question fails before any call is made.
func ValidateQuestions(qs map[string]Question) error {
	if len(qs) == 0 {
		return errors.New("at least one question is required")
	}
	for _, id := range SortedIDs(qs) {
		if strings.TrimSpace(id) == "" {
			return errors.New("question id must not be empty")
		}
		q := qs[id]
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("question %q: instructions must not be empty", id)
		}
		if err := validateShape(id, q); err != nil {
			return err
		}
	}
	return nil
}

func validateShape(id string, q Question) error {
	switch q.Type {
	case Choice:
		if len(q.Levels) > 0 {
			return fmt.Errorf("question %q: choice questions take choices, not levels", id)
		}
		if n := len(q.Choices); n < 2 || n > maxChoiceOptions {
			return fmt.Errorf("question %q: choice needs 2-%d options, got %d", id, maxChoiceOptions, n)
		}
	case Score:
		if len(q.Choices) > 0 {
			return fmt.Errorf("question %q: score questions take levels, not choices", id)
		}
		if n := len(q.Levels); n < minScoreLevels || n > maxScoreLevels {
			return fmt.Errorf("question %q: score needs %d-%d levels, got %d", id, minScoreLevels, maxScoreLevels, n)
		}
	case Noul:
		if len(q.Levels) > 0 {
			return fmt.Errorf("question %q: noul questions take no levels", id)
		}
		for k := range q.Choices {
			if k != "true" && k != "false" {
				return fmt.Errorf("question %q: noul criteria keys must be \"true\" or \"false\", got %q", id, k)
			}
		}
	default:
		return fmt.Errorf("question %q: unknown type %q (want choice, noul, or score)", id, q.Type)
	}
	return nil
}

// SortedIDs returns the question IDs in a stable order.
func SortedIDs[V any](qs map[string]V) []string {
	ids := make([]string, 0, len(qs))
	for id := range qs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// EstimateTokens estimates the input size of a call from its wire encoding.
func EstimateTokens(state any, qs map[string]Question) (int, error) {
	b, err := json.Marshal(encodeRequest(Request{State: state, Questions: qs}))
	if err != nil {
		return 0, fmt.Errorf("marshaling decision input: %w", err)
	}
	return (len(b) + bytesPerToken - 1) / bytesPerToken, nil
}
