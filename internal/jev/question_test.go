package jev

import (
	"strings"
	"testing"
)

func TestValidateQuestions(t *testing.T) {
	tests := map[string]struct {
		qs      map[string]Question
		wantErr string
	}{
		"none":             {qs: map[string]Question{}, wantErr: "at least one"},
		"empty id":         {qs: map[string]Question{" ": {Type: Noul, Instructions: "x"}}, wantErr: "id"},
		"no instructions":  {qs: map[string]Question{"a": {Type: Noul}}, wantErr: "instructions"},
		"unknown type":     {qs: map[string]Question{"a": {Type: "maybe", Instructions: "x"}}, wantErr: "unknown type"},
		"one choice":       {qs: map[string]Question{"a": {Type: Choice, Instructions: "x", Choices: []Option{{"a", "a"}}}}, wantErr: "2-255"},
		"one level":        {qs: map[string]Question{"a": {Type: Score, Instructions: "x", Levels: []string{"a"}}}, wantErr: "2-10"},
		"noul bad key":     {qs: map[string]Question{"a": {Type: Noul, Instructions: "x", Choices: []Option{{"yes", "y"}}}}, wantErr: "true"},
		"choice levels":    {qs: map[string]Question{"a": {Type: Choice, Instructions: "x", Levels: []string{"a", "b"}}}, wantErr: "not levels"},
		"dup option":       {qs: map[string]Question{"a": {Type: Choice, Instructions: "x", Choices: []Option{{"a", "1"}, {"a", "2"}}}}, wantErr: "duplicate"},
		"ok noul":          {qs: map[string]Question{"a": {Type: Noul, Instructions: "x", Choices: []Option{{"true", "t"}, {"false", "f"}}}}},
		"ok score min max": {qs: map[string]Question{"a": {Type: Score, Instructions: "x", Levels: []string{"a", "b"}}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateQuestions(tt.qs)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	small, err := EstimateTokens("hi", map[string]Question{"a": {Type: Noul, Instructions: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	big, err := EstimateTokens(strings.Repeat("x", 3000), map[string]Question{"a": {Type: Noul, Instructions: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if big-small < 990 || big-small > 1010 {
		t.Errorf("3000 extra bytes should add ~1000 tokens, added %d", big-small)
	}
}
