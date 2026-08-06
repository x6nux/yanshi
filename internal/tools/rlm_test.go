package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// allowProfile 是 inline helper，不复用不存在的 testAllowProfile。
func allowProfile(toolNames ...string) guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: toolNames},
	}
}

func TestRLMQueryMetadataAndGenerateOnly(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"classification"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake, MaxConcurrency: 16})
	require.NotNil(t, set.Query)

	// GuardedTool 没有 Name()/Description() 方法；通过 Info(ctx) 读取。
	info, err := set.Query.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "rlm_query", info.Name)
	for _, phrase := range []string{"cost-class", "cheap", "non-streaming", "16"} {
		assert.True(
			t,
			strings.Contains(strings.ToLower(info.Desc), strings.ToLower(phrase)),
			"Desc %q missing %q", info.Desc, phrase,
		)
	}

	ctx := tools.WithProfile(context.Background(), allowProfile("rlm_query"))
	promptPayload, _ := json.Marshal([]string{"one", "two"})
	args := fmt.Sprintf(`{"prompts":%s}`, strconv.Quote(string(promptPayload)))
	result, err := set.Query.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"index":0`)
	assert.Contains(t, result, `"index":1`)

	// 字段访问，不是方法调用。
	assert.Equal(t, 2, fake.GenerateCalls, "GenerateCalls")
	assert.Equal(t, 0, fake.StreamCalls, "StreamCalls")
}

// TestRLMQueryAcceptsBothEndsOfTheRange is the accept direction of the range.
//
// The rejection test below is the only thing that used to carry this clause,
// and a rejection test alone says nothing about what the tool ACCEPTS.
// Measured: narrowing the gate to `len(prompts) >= 1` — a tool that refuses
// every non-empty batch — leaves TestRLMQueryRejectsMoreThanSixteen green.
// The clause names an interval, so both its ends have to be walked.
//
// The result count is asserted, not just the absence of an error: a gate that
// silently truncated to a smaller batch would also return successfully.
//
// ledger: C1/RLM1#1 1-16 并发
func TestRLMQueryAcceptsBothEndsOfTheRange(t *testing.T) {
	for _, n := range []int{1, 16} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			replies := make([]string, n)
			for i := range replies {
				replies[i] = "ok"
			}
			fake := einollm.NewFakeModel(replies, nil)
			set := tools.NewRLMTools(rlm.Runner{Model: fake, MaxConcurrency: 16})
			ctx := tools.WithProfile(context.Background(), allowProfile("rlm_query"))

			prompts := make([]string, n)
			for i := range prompts {
				prompts[i] = fmt.Sprintf("classify %d", i)
			}
			encoded, err := json.Marshal(prompts)
			require.NoError(t, err)
			result, err := set.Query.InvokableRun(ctx,
				fmt.Sprintf(`{"prompts":%s}`, strconv.Quote(string(encoded))))
			require.NoError(t, err)
			require.NotContains(t, result, "1 to 16",
				"a batch of %d was rejected as out of range", n)

			assert.Equal(t, n, fake.GenerateCalls,
				"the tool accepted %d prompts but made %d model calls", n, fake.GenerateCalls)
			for i := 0; i < n; i++ {
				assert.Contains(t, result, fmt.Sprintf(`"index":%d`, i),
					"result %d is missing: the batch was silently truncated", i)
			}
		})
	}
}

// ledger: C1/RLM1#1 1-16 并发
func TestRLMQueryRejectsMoreThanSixteen(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	ctx := tools.WithProfile(context.Background(), allowProfile("rlm_query"))

	prompts := make([]string, 17)
	encoded, _ := json.Marshal(prompts)
	result, err := set.Query.InvokableRun(ctx, fmt.Sprintf(`{"prompts":%s}`, strconv.Quote(string(encoded))))
	require.NoError(t, err)
	assert.Contains(t, result, "1 to 16")
	assert.Equal(t, 0, fake.GenerateCalls, "must not call model on oversize batch")
}

func TestRLMQueryDeniedWithoutProfile(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	// 无 profile → GuardedTool.Stream 必须返回 permission denied 结果。
	result, err := set.Query.InvokableRun(context.Background(), `{"prompts":""}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
	assert.Equal(t, 0, fake.GenerateCalls)
}

func TestRLMQueryDeniedWhenProfileOmitsName(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	// profile 允许其它工具但不允许 rlm_query → 拒绝。
	ctx := tools.WithProfile(context.Background(), allowProfile("memory_search"))
	result, err := set.Query.InvokableRun(ctx, `{"prompts":""}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
	assert.Equal(t, 0, fake.GenerateCalls)
}
