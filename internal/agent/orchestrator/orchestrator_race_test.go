package orchestrator

import (
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
			runners[idx] = o.runnerFor(fm, false)
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
				_ = o.runnerFor(m, false)
				_ = o.runnerFor(m, true)
			}
		}()
	}
	wg.Wait()
}

func TestRunners_FlushDuringAccess(t *testing.T) {
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
				_ = o.runnerFor(fm, false)
				_ = o.runnerFor(fm, true)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			o.FlushRunners()
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
