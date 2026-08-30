// `yanshi models` (review-whole.md M-1): explicitly user-invoked pull/preheat
// for the local runtimes internal/llm/eino already discovers.
//
// It lives in its own file for the same GOV2 reason provider.go and ops.go
// do (main.go stays clear of a third verb's flag parsing), and because,
// like acp/provider above, it never talks to an already-running yanshi
// daemon — it talks to Ollama/LM Studio directly, the same local-runtime
// clients doctorlocalruntimes.go already builds.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/llm/eino"
)

// runModels implements `yanshi models <pull|preheat>`.
func runModels(args []string, stdout, stderr io.Writer) int {
	if isHelpArg(args) {
		fmt.Fprint(stdout, modelsUsage)
		return exitOK
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: yanshi models <pull|preheat> [flags]")
		return exitUsage
	}
	switch args[0] {
	case "pull":
		return modelsPull(args[1:], stdout, stderr)
	case "preheat":
		return modelsPreheat(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown models subcommand %q (want pull or preheat)\n", args[0])
		return exitUsage
	}
}

// modelsPull parses `yanshi models pull` and runs it against a real Ollama
// daemon. There is no -timeout flag: PullModel's own doc comment says a pull
// can legitimately run for many minutes, so the only deadline offered is
// SIGINT/SIGTERM (via ctx), the same contract `yanshi serve` uses.
func modelsPull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("models pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "", "Ollama base URL (default "+eino.DefaultOllamaBaseURL+")")
	model := fs.String("model", "", "Ollama model tag to pull, e.g. llama3.1:8b (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *model == "" {
		fmt.Fprintln(stderr, "yanshi models pull: -model is required")
		return exitUsage
	}
	ctx, stop := newInterruptibleContext()
	defer stop()
	if err := cli.RunModelsPull(ctx, cli.ModelsPullOptions{BaseURL: *baseURL, Model: *model, Progress: stdout}); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitErr
	}
	fmt.Fprintf(stdout, "%s: pulled\n", *model)
	return exitOK
}

// modelsPreheat parses `yanshi models preheat` and runs it against a real LM
// Studio daemon. Like modelsPull, no -timeout flag — LoadModel's own doc
// comment says a cold load has no fixed upper bound.
func modelsPreheat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("models preheat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "", "LM Studio base URL (default "+eino.DefaultLMStudioBaseURL+")")
	apiKey := fs.String("api-key", "", "LM Studio bearer token, if configured")
	model := fs.String("model", "", "LM Studio model id to load (required)")
	contextLength := fs.Int("context-length", 0, "llama.cpp context_length override (0 = server default)")
	evalBatchSize := fs.Int("eval-batch-size", 0, "llama.cpp eval_batch_size override (0 = server default)")
	numExperts := fs.Int("num-experts", 0, "llama.cpp num_experts override for MoE models (0 = server default)")
	flashAttention := fs.Bool("flash-attention", false, "enable flash attention")
	offloadKVCache := fs.Bool("offload-kv-cache-to-gpu", false, "offload the KV cache to GPU")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *model == "" {
		fmt.Fprintln(stderr, "yanshi models preheat: -model is required")
		return exitUsage
	}
	ctx, stop := newInterruptibleContext()
	defer stop()
	result, err := cli.RunModelsPreheat(ctx, cli.ModelsPreheatOptions{
		BaseURL: *baseURL, APIKey: *apiKey, Model: *model,
		Load: eino.LoadOptions{
			ContextLength:       *contextLength,
			EvalBatchSize:       *evalBatchSize,
			NumExperts:          *numExperts,
			FlashAttention:      *flashAttention,
			OffloadKVCacheToGPU: *offloadKVCache,
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitErr
	}
	fmt.Fprintf(stdout, "%s: %s (instance %s, %.2fs)\n", *model, result.Status, result.InstanceID, result.LoadTimeSeconds)
	return exitOK
}

// newInterruptibleContext gives modelsPull/modelsPreheat a context that
// cancels on SIGINT/SIGTERM instead of leaving a many-minute pull or an
// unbounded load with no way to stop it short of killing the process — see
// PullModel's and LoadModel's doc comments for why neither operation carries
// its own deadline. Same signal set runACPServer already uses.
func newInterruptibleContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// modelsUsage is the `yanshi models -h` text. A literal for the same reason
// providerUsage is: the verb is positional and the flags differ per verb.
const modelsUsage = `Usage: yanshi models <pull|preheat> [flags]

Explicitly pull an Ollama model or preheat (cold-load) an LM Studio model.
Unlike ` + "`yanshi doctor`" + `, which only ever probes (see its own -h text),
both of these have a real side effect the operator asked for: pull downloads
gigabytes, preheat loads a model into memory. Both force-refresh the local
discovery cache afterward, so the change shows up on the very next doctor run
or model-picker read.

Verbs:
  pull     POST /api/pull to a local Ollama daemon and stream progress to
           stdout. Runs until the pull completes, fails, or Ctrl-C.
  preheat  POST /api/v1/models/load to a local LM Studio daemon. Runs until
           the load completes, fails, or Ctrl-C.

Flags (pull):
  -base-url string   Ollama base URL (default ` + eino.DefaultOllamaBaseURL + `)
  -model string      Ollama model tag to pull, e.g. llama3.1:8b (required)

Flags (preheat):
  -base-url string             LM Studio base URL (default ` + eino.DefaultLMStudioBaseURL + `)
  -api-key string               LM Studio bearer token, if configured
  -model string                 LM Studio model id to load (required)
  -context-length int           llama.cpp context_length override
  -eval-batch-size int          llama.cpp eval_batch_size override
  -num-experts int               llama.cpp num_experts override for MoE models
  -flash-attention               enable flash attention
  -offload-kv-cache-to-gpu       offload the KV cache to GPU
`
