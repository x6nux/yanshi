package orchestrator

import (
	"strconv"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestRunners_SameModelReturnsSamePointer(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	const n = 20
	runners := make([]*adk.Runner, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runners[idx] = o.runnerFor(fm, false, "")
		}(i)
	}
	wg.Wait()

	first := runners[0]
	for i := 1; i < n; i++ {
		if runners[i] != first {
			t.Fatalf("runner[%d] is different pointer from runner[0]", i)
		}
	}
}

func TestRunners_DifferentModelKeys(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	models := make([]model.BaseChatModel, 10)
	for i := range models {
		models[i] = einollm.NewFakeModelWithMessages(nil, nil)
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, m := range models {
				_ = o.runnerFor(m, false, "")
				_ = o.runnerFor(m, true, "")
			}
		}()
	}
	wg.Wait()
}

// TestRunners_NewKeysDuringAccess drives concurrent Stores into the runners map
// while other goroutines Load hot keys.
//
// It used to call FlushRunners as its writer. That method is gone (it had zero
// production callers and nothing to evict — see the runners field), so the
// writer is now the thing that really does insert new entries at runtime: a
// stream of previously unseen model ids, each of which is its own
// runnerCacheKey. Same contention shape, and unlike the flush loop it is a
// sequence production can actually produce.
func TestRunners_NewKeysDuringAccess(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{
		Model:   fm,
		Profile: testProfile(),
	})
	assert.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = o.runnerFor(fm, false, "")
				_ = o.runnerFor(fm, true, "")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			_ = o.runnerFor(fm, false, "model-"+strconv.Itoa(j))
		}
	}()
	wg.Wait()
}

func testProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	}
}
