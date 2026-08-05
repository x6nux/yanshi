package main

import (
	"flag"

	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/config"
)

// resolveGoalBudget combines the goal loop's two budget sources: the `goal:`
// config block and the -max-tokens / -max-iters flags.
//
// An explicitly passed flag always wins, including when it is passed as 0.
// That case is the reason this cannot be a plain "non-zero wins" fold:
// `-max-tokens 0` against a config that sets 50000 is the operator asking to
// LIFT the limit for one run, and silently reinstating the config value would
// stop a run they deliberately unbounded. fs.Visit reports only flags actually
// present on the command line, which is the only way to tell 0-because-typed
// from 0-because-default.
func resolveGoalBudget(fs *flag.FlagSet, flagTokens, flagIters int, cfg config.GoalConfig) goalloop.Budget {
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	b := goalloop.Budget{MaxTokens: cfg.MaxTokens, MaxIterations: cfg.MaxIterations}
	if passed["max-tokens"] || b.MaxTokens == 0 {
		b.MaxTokens = flagTokens
	}
	if passed["max-iters"] || b.MaxIterations == 0 {
		b.MaxIterations = flagIters
	}
	return b
}
