package registry

import (
	"context"
	"sync"
	"testing"
)

type fakeRunner struct{}

func (f fakeRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
	return "done", nil
}

func TestManager_ConcurrentSpawnCancel(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          t.TempDir(),
		SessionBootID: "race-test",
		MaxConcurrent: 10,
	})

	var wg sync.WaitGroup
	const n = 20

	agentIDs := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, err := m.Spawn(context.Background(), SpawnRequest{
				AgentType: "race-test",
				Prompt:    "do something",
				Runner:    fakeRunner{},
			})
			if err == nil {
				agentIDs[idx] = id
			}
		}(i)
	}
	wg.Wait()

	for _, id := range agentIDs {
		if id != "" {
			wg.Add(1)
			go func(aID string) {
				defer wg.Done()
				_ = m.Cancel(aID)
			}(id)
		}
	}
	wg.Wait()
	m.Close()
}

func TestManager_ConcurrentListAndResult(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          t.TempDir(),
		SessionBootID: "race-test",
		MaxConcurrent: 10,
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		_, _ = m.Spawn(context.Background(), SpawnRequest{
			AgentType: "race-test",
			Prompt:    "test",
			Runner:    fakeRunner{},
		})
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.List(false)
			m.Result("nonexistent")
		}()
	}
	wg.Wait()
	m.Close()
}

func TestRegistry_BootstrapFrozenConvention(t *testing.T) {
	r := New()
	r.Register(Entry{Name: "agent-a", Kind: KindLocal, Description: "test agent"})
	r.Register(Entry{Name: "agent-b", Kind: KindLocal, Description: "another agent"})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Get("agent-a")
			_ = r.All()
			_ = r.ByCapability("shell_*")
		}()
	}
	wg.Wait()
}
