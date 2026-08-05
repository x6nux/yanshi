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
