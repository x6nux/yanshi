package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/x6nux/yanshi/internal/config"
)

// TestResolveGoalBudget covers the precedence rule that makes the config block
// safe to add: a flag the operator actually typed always wins, and a config
// value only fills a slot no flag claimed.
func TestResolveGoalBudget(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		cfg        config.GoalConfig
		wantTokens int
		wantIters  int
	}{
		{
			name:      "no config, no flags: flag defaults stand",
			argv:      nil,
			wantIters: 5,
		},
		{
			name:       "config only",
			argv:       nil,
			cfg:        config.GoalConfig{MaxTokens: 50000, MaxIterations: 9},
			wantTokens: 50000, wantIters: 9,
		},
		{
			name:       "typed flags beat config",
			argv:       []string{"-max-tokens", "10", "-max-iters", "2"},
			cfg:        config.GoalConfig{MaxTokens: 50000, MaxIterations: 9},
			wantTokens: 10, wantIters: 2,
		},
		{
			// The case a "non-zero wins" fold gets wrong: passing 0 explicitly
			// is a request to LIFT the configured limit for this run.
			name:       "explicit zero lifts a configured limit",
			argv:       []string{"-max-tokens", "0"},
			cfg:        config.GoalConfig{MaxTokens: 50000, MaxIterations: 9},
			wantTokens: 0, wantIters: 9,
		},
		{
			name:       "one flag typed, the other left to config",
			argv:       []string{"-max-iters", "3"},
			cfg:        config.GoalConfig{MaxTokens: 50000, MaxIterations: 9},
			wantTokens: 50000, wantIters: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("goal", flag.ContinueOnError)
			maxIters := fs.Int("max-iters", 5, "")
			maxTokens := fs.Int("max-tokens", 0, "")
			if err := fs.Parse(tc.argv); err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := resolveGoalBudget(fs, *maxTokens, *maxIters, tc.cfg)
			assert.Equal(t, tc.wantTokens, got.MaxTokens, "MaxTokens")
			assert.Equal(t, tc.wantIters, got.MaxIterations, "MaxIterations")
		})
	}
}

// TestExplicitBudgetFlags pins the signal the goal loop weighs against a
// resumed budget: only limits typed on this command line count.
//
// The two cases that matter are the ones a value-based guess gets wrong in
// opposite directions — typing the flag's own default is still typing it, and
// a value that came from the config block was never typed at all.
func TestExplicitBudgetFlags(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		wantTokens bool
		wantIters  bool
	}{
		{name: "nothing typed"},
		{name: "both typed", argv: []string{"-max-tokens", "10", "-max-iters", "2"},
			wantTokens: true, wantIters: true},
		{name: "only tokens typed", argv: []string{"-max-tokens", "10"}, wantTokens: true},
		{name: "only iters typed", argv: []string{"-max-iters", "2"}, wantIters: true},
		{
			// Same number as the flag's default. "differs from the default"
			// would call this untyped and let a stale stored budget win.
			name:       "typed value equal to the default still counts",
			argv:       []string{"-max-iters", "5", "-max-tokens", "0"},
			wantTokens: true, wantIters: true,
		},
		{
			// An unrelated flag must not mark either budget as chosen.
			name: "an unrelated flag marks nothing",
			argv: []string{"-tier", "t3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("goal", flag.ContinueOnError)
			fs.Int("max-iters", 5, "")
			fs.Int("max-tokens", 0, "")
			fs.String("tier", "auto", "")
			if err := fs.Parse(tc.argv); err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := explicitBudgetFlags(fs)
			assert.Equal(t, tc.wantTokens, got.MaxTokens, "MaxTokens")
			assert.Equal(t, tc.wantIters, got.MaxIterations, "MaxIterations")
		})
	}
}
