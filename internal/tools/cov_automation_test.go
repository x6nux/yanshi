package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

// ---------------------------------------------------------------------------
// oneChunk — error path
// ---------------------------------------------------------------------------

func TestOneChunk_Success(t *testing.T) {
	ch := oneChunk(func() (string, error) {
		return "ok", nil
	})
	result := <-ch
	assert.Equal(t, "ok", result.Result)
}

func TestOneChunk_Error(t *testing.T) {
	ch := oneChunk(func() (string, error) {
		return "", errTestString("boom")
	})
	result := <-ch
	assert.Contains(t, result.Result, "boom")
}

// ---------------------------------------------------------------------------
// decodeInput — multiple error paths
// ---------------------------------------------------------------------------

func TestDecodeInput_EmptyInput(t *testing.T) {
	var target struct{}
	err := decodeInput(`{}`, &target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input is required")
}

func TestDecodeInput_NestedInvalidJSON(t *testing.T) {
	var target struct{}
	err := decodeInput(`{"input":"{bad"}`, &target)
	require.Error(t, err)
}

func TestDecodeInput_Success(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}
	err := decodeInput(`{"input":"{\"name\":\"x\"}"}`, &target)
	require.NoError(t, err)
	assert.Equal(t, "x", target.Name)
}

// ---------------------------------------------------------------------------
// Internal automation test helpers (in-package to access unexported run* fns)
// ---------------------------------------------------------------------------

type covQueueStub struct {
	mu    sync.Mutex
	calls int
}

func (r *covQueueStub) SubmitRun(_ context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return automation.RunReceipt{WorkTaskID: "wt-" + p.RunID, BrokerTaskID: "broker-" + p.RunID}, nil
}
func (r *covQueueStub) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	return automation.RunStatus{Status: automation.RunQueued}, nil
}

func setupCovAutomationMgr(t *testing.T) (*AutomationTools, *automation.Manager) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	q := &covQueueStub{}
	m, err := automation.NewManager(repo, q, nil)
	require.NoError(t, err)
	return NewAutomationTools(m), m
}

func createCovAutomationItem(t *testing.T, mgr *automation.Manager, name, prompt string) string {
	t.Helper()
	sched, _ := automation.ParseSchedule("interval", "3600")
	item, err := mgr.Create(automation.CreateInput{
		Name: name, Prompt: prompt, Schedule: sched,
	})
	require.NoError(t, err)
	return item.ID
}

func readAutomationChunkT(t *testing.T, ch <-chan ToolChunk) string {
	t.Helper()
	var result string
	for c := range ch {
		if c.Result != "" {
			result = c.Result
		}
	}
	return result
}

type errTestString string

func (e errTestString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// runAutomationCreate — error paths
// ---------------------------------------------------------------------------

func TestRunAutomationCreate_BadSchedule(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"name":"x","prompt":"p","schedule_kind":"interval","schedule":"not-a-number"}`,
	})
	ch := runAutomationCreate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "interval")
}

func TestRunAutomationCreate_DecodeError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationCreate(context.Background(), m, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "invalid")
}

func TestRunAutomationCreate_MissingName(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"name":"","prompt":"p","schedule_kind":"interval","schedule":"3600"}`,
	})
	ch := runAutomationCreate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "name")
}

func TestRunAutomationCreate_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"name":"cov1","prompt":"p","schedule_kind":"interval","schedule":"3600"}`,
	})
	ch := runAutomationCreate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "cov1")
}

// ---------------------------------------------------------------------------
// runAutomationList
// ---------------------------------------------------------------------------

func TestRunAutomationList_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	createCovAutomationItem(t, m, "list1", "p")
	ch := runAutomationList(context.Background(), m, `{"input":"{}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "list1")
}

func TestRunAutomationList_Empty(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationList(context.Background(), m, `{"input":"{}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "null")
}

// ---------------------------------------------------------------------------
// runAutomationRead
// ---------------------------------------------------------------------------

func TestRunAutomationRead_DecodeError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationRead(context.Background(), m, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "invalid")
}

func TestRunAutomationRead_NotFound(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"nonexistent"}`,
	})
	ch := runAutomationRead(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestRunAutomationRead_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	id := createCovAutomationItem(t, m, "readtest", "p")
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"` + id + `"}`,
	})
	ch := runAutomationRead(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, id)
}

// ---------------------------------------------------------------------------
// runAutomationUpdate
// ---------------------------------------------------------------------------

func TestRunAutomationUpdate_DecodeError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationUpdate(context.Background(), m, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "invalid")
}

func TestRunAutomationUpdate_BadSchedule(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	id := createCovAutomationItem(t, m, "updtest", "p")
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"` + id + `","schedule_kind":"interval","schedule":"bad"}`,
	})
	ch := runAutomationUpdate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "interval")
}

func TestRunAutomationUpdate_NotFound(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"nonexistent","name":"new"}`,
	})
	ch := runAutomationUpdate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestRunAutomationUpdate_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	id := createCovAutomationItem(t, m, "updok", "p")
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"` + id + `","name":"newname"}`,
	})
	ch := runAutomationUpdate(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "newname")
}

// ---------------------------------------------------------------------------
// runAutomationIDOp (pause/resume/delete)
// ---------------------------------------------------------------------------

func TestRunAutomationIDOp_DecodeError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationIDOp(context.Background(), m, `not-json`, func(string) error { return nil })
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "invalid")
}

func TestRunAutomationIDOp_OpError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"x"}`,
	})
	ch := runAutomationIDOp(context.Background(), m, string(args), func(string) error {
		return errTestString("op failed")
	})
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "op failed")
}

func TestRunAutomationIDOp_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	id := createCovAutomationItem(t, m, "idop", "p")
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"` + id + `"}`,
	})
	ch := runAutomationIDOp(context.Background(), m, string(args), func(string) error { return nil })
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, `"ok":true`)
}

// ---------------------------------------------------------------------------
// runAutomationRun
// ---------------------------------------------------------------------------

func TestRunAutomationRun_DecodeError(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	ch := runAutomationRun(context.Background(), m, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "invalid")
}

func TestRunAutomationRun_NotFound(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"nonexistent"}`,
	})
	ch := runAutomationRun(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestRunAutomationRun_Success(t *testing.T) {
	_, m := setupCovAutomationMgr(t)
	id := createCovAutomationItem(t, m, "runtest", "p")
	args, _ := json.Marshal(map[string]string{
		"input": `{"id":"` + id + `"}`,
	})
	ch := runAutomationRun(context.Background(), m, string(args))
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "queued")
}
