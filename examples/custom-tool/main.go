// custom-tool: a minimal custom yanshi tool built on the public surface.
//
// This example constructs a GuardedTool ("echo") the same way yanshi's own
// tools do — via tools.NewGuardedTool + schema.NewParamsOneOfByParams +
// tools.SyncStream — and shows it satisfies the tools.Tool contract.
//
// API GAP (see README.md): yanshi does not yet expose a public point to
// REGISTER an external tool with a running server. Tools are wired in
// internal/bootstrap.Build. So this example demonstrates CONSTRUCTION and the
// Tool interface; to actually expose `echo` to the model you currently add it
// alongside the built-in tools in internal/bootstrap. That gap is tracked for a
// future batch — the example does NOT hack around it or temp-export internals.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// echoTool builds a minimal GuardedTool that echoes its "text" argument.
// NewGuardedTool + NewParamsOneOfByParams + SyncStream are all public, so this
// is the same shape as the built-in tools.
func echoTool() tools.Tool {
	params := schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"text": {Type: schema.String, Required: true, Desc: "the text to echo back"},
	})
	stream := tools.SyncStream(func(_ context.Context, argsJSON string) (string, error) {
		// A real tool unmarshals argsJSON and does work; this one just echoes.
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
			return "", fmt.Errorf("echo: bad args: %w", err)
		}
		return "echo: " + in.Text, nil
	})
	return tools.NewGuardedTool(
		"echo", "Echo", "Echo the text argument back to the model.",
		5*time.Second, params, stream,
	)
}

func main() {
	t := echoTool()
	var _ tools.Tool = t // compile-time check: satisfies the Tool contract

	ctx := context.Background()
	info, err := t.Info(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("built custom tool %q (%s): %s\n", info.Name, t.DisplayName(), info.Desc)

	// GuardedTool is fail-closed (see docs/adr/0003): without a permission
	// profile bound in context, Stream is denied — even for our own tool.
	fmt.Println("\n-- without profile (expect deny) --")
	drain(t.Stream(ctx, `{"text":"hello"}`))

	// Bind a permissive profile via WithProfile (the same context-injection
	// pattern the orchestrator uses every turn) and the call succeeds.
	fmt.Println("\n-- with permissive profile (expect echo) --")
	permissive := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
	drain(t.Stream(tools.WithProfile(ctx, permissive), `{"text":"hello"}`))
}

// drain reads a tool's chunk channel to completion and prints result/error.
func drain(ch <-chan tools.ToolChunk) {
	for chunk := range ch {
		if chunk.Err != nil {
			fmt.Println("denied:", chunk.Err)
			return
		}
		fmt.Println("result:", chunk.Result)
	}
}
