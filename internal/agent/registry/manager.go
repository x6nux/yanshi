package registry

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// NewManagerOpts
// ---------------------------------------------------------------------------

// NewManagerOpts configures a Manager.
type NewManagerOpts struct {
	RootContext   context.Context
	Path          string
	SessionBootID string
	MaxConcurrent int // 0 → default 10; must be 1..20
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager is the process-wide runtime for managed sub-agents. It provides
// lifecycle management (Spawn / Wait / Result / List / Close / Resume /
// Cancel), concurrency limiting, atomic persistence, cancellation trees, and
// lifecycle events.
//
// Design invariants:
//   - persistMu → mu → snapshot → unlock mu → write → unlock persistMu
//   - Closed gate checked before any mutation
//   - Running persisted before returning from Spawn/Resume
type Manager struct {
	mtx        sync.RWMutex      // guards records + runtime
	records    map[string]Record // all agents (running + terminal)
	runtime    map[string]*runtimeAgent
	limit      int
	path       string
	bootID     string
	rootCtx    context.Context
	rootCancel context.CancelFunc
	closed     atomic.Bool
	wg         sync.WaitGroup
	agentIDSeq atomic.Int64
	persistMu  sync.Mutex // serialises writes
}

// runtimeAgent tracks the in-memory state of a running managed sub-agent.
type runtimeAgent struct {
	ctx        context.Context
	cancel     context.CancelFunc
	turnCancel context.CancelFunc // cancels the current turn (not the agent)
	accepting  bool               // true when mailbox open for SendInput
	assignment string
	mailbox    chan string
	emit       EventSink
	runner     Runner
	done       chan struct{} // closed when runner finishes
}

// NewManager creates a Manager with closed gate open.
func NewManager(opts NewManagerOpts) *Manager {
	limit := opts.MaxConcurrent
	if limit == 0 {
		limit = 10
	} else if limit < 1 {
		limit = 1
	} else if limit > 20 {
		limit = 20
	}
	rootCtx, rootCancel := context.WithCancel(opts.RootContext)
	m := &Manager{
		records:    make(map[string]Record),
		runtime:    make(map[string]*runtimeAgent),
		limit:      limit,
		path:       opts.Path,
		bootID:     opts.SessionBootID,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
	}
	// Load persisted state: mark Running/Pending as Interrupted for new boot.
	st, err := loadState(opts.Path)
	if err == nil {
		for _, rec := range st.Agents {
			if !rec.Status.Terminal() {
				rec.Status = StatusInterrupted
			}
			m.records[rec.ID] = rec
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Spawn
// ---------------------------------------------------------------------------

// Spawn creates a new managed sub-agent record, persists it as Running, then
// starts the runner goroutine. On persistence failure the record is rolled
// back and no goroutine is started.
func (m *Manager) Spawn(ctx context.Context, req SpawnRequest) (string, error) {
	if m.closed.Load() {
		return "", ErrClosed
	}
	if req.Runner == nil {
		return "", fmt.Errorf("subagent spawn: runner is required")
	}
	id := m.nextID()
	parentID := req.ParentID
	if parentID == "" {
		parentID = CurrentAgentID(ctx)
	}

	m.persistMu.Lock()
	m.mtx.Lock()

	depth := 0
	var parentRT *runtimeAgent
	if parentID != "" {
		// Review #3 race fix: read runtime/records UNDER m.mtx (Go maps are not
		// safe for concurrent read+write even on different keys). Capture the
		// parent's runtime agent pointer here so runtimeChildCtx doesn't need
		// to re-read the map unlocked below.
		if rt, live := m.runtime[parentID]; live {
			parentRT = rt
			depth = m.records[parentID].Depth + 1
			// --- depth vs concurrency: two orthogonal dimensions ---
			// Depth (vertical, MaxSubAgentDepth=3 from subagent.go:99) is
			// checked FIRST. When both depth and concurrency are exceeded,
			// ErrTooDeep wins — a deeper agent will never starve a shallower
			// slot (the concurrency limit governs independently). Both
			// dimensions are in effect simultaneously: an agent at max depth
			// may still spawn its own sub-agent if the concurrency budget
			// permits, and a sub-agent at the concurrency limit cannot spawn
			// another even if the depth budget is available.
			if depth > 3 { // MaxSubAgentDepth
				m.mtx.Unlock()
				m.persistMu.Unlock()
				return "", ErrTooDeep
			}
		}
	}

	// --- concurrency cap gate (second dimension) ---
	// runningLocked() returns the max of runtime map entries and
	// StatusRunning records. The two can diverge transiently under heavy
	// concurrency; taking the max is conservative (prefer spawning fewer
	// than budgeted over more). If RAC1 reports a measurable race here,
	// the resolution is to use the runtime map as the sole authoritative
	// count (the records count is only advisory).
	if m.runningLocked() >= m.limit {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", &SpawnErrCap{Cap: m.limit}
	}
	// Closed gate double-check.
	if m.closed.Load() {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", ErrClosed
	}

	now := time.Now().UTC()
	rec := Record{
		ID:              id,
		SessionBootID:   m.bootID,
		AgentType:       req.AgentType,
		Role:            req.Role,
		Nickname:        req.Nickname,
		ParentID:        parentID,
		Depth:           depth,
		Status:          StatusRunning,
		Prompt:          req.Prompt,
		Instruction:     req.Instruction,
		AllowedTools:    copyStrings(req.AllowedTools),
		ModelOverride:   req.ModelOverride,
		ReasoningEffort: req.ReasoningEffort,
		Custom:          copyCustomRole(req.Custom),
		StartedAt:       now,
	}
	m.records[id] = cloneRecord(rec)
	rt := m.newRuntimeAgent(req.Runner, req.Emit, req.Prompt)
	m.runtime[id] = rt

	// Capture parent context under lock — parentRT.ctx is written by
	// runAgentLoop (line 631) and must not be read unlocked.
	parentCtx := m.rootCtx
	if parentRT != nil {
		parentCtx = parentRT.ctx
	}

	snapshot, snapErr := m.snapshotLocked()
	m.mtx.Unlock()

	if snapErr != nil {
		// Rollback.
		m.mtx.Lock()
		delete(m.records, id)
		delete(m.runtime, id)
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", fmt.Errorf("persist subagent spawn: %w", snapErr)
	}
	if err := writeAtomic(m.path, snapshot); err != nil {
		m.mtx.Lock()
		delete(m.records, id)
		delete(m.runtime, id)
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", fmt.Errorf("persist subagent spawn: %w", err)
	}
	m.persistMu.Unlock()

	childCtx, childCancel := context.WithCancel(parentCtx)
	rt.ctx = childCtx
	rt.cancel = childCancel
	m.wg.Add(1)
	go m.runAgentLoop(childCtx, id, rt)

	return id, nil
}

// ---------------------------------------------------------------------------
// Result
// ---------------------------------------------------------------------------

// Result returns a cloned snapshot of the record for agentID. Returns
// (Record{}, false) if the agent does not exist.
func (m *Manager) Result(agentID string) (Record, bool) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	rec, ok := m.records[agentID]
	if !ok {
		return Record{}, false
	}
	return cloneRecord(rec), true
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List returns all records. When includeArchived is false only agents from
// the current session boot are returned. Running count and limit are always
// computed from live state.
func (m *Manager) List(includeArchived bool) ListResult {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	running := 0
	agents := make([]Record, 0, len(m.records))
	for _, rec := range m.records {
		if !includeArchived && rec.SessionBootID != m.bootID {
			continue
		}
		if rec.Status == StatusRunning {
			running++
		}
		agents = append(agents, cloneRecord(rec))
	}
	return ListResult{
		Agents:  agents,
		Running: running,
		Limit:   m.limit,
	}
}

// ---------------------------------------------------------------------------
// SendInput
// ---------------------------------------------------------------------------

// SendInput queues follow-up assignment text into the running agent's mailbox.
// When interrupt is true, the current turn is cancelled so the runner picks up
// the new assignment.
func (m *Manager) SendInput(agentID, text string, interrupt bool) error {
	if m.closed.Load() {
		return ErrClosed
	}
	m.mtx.Lock()
	rt, ok := m.runtime[agentID]
	rec, recOK := m.records[agentID]
	if !ok || !recOK || rec.Status != StatusRunning || !rt.accepting {
		m.mtx.Unlock()
		return ErrNotRunning
	}
	select {
	case rt.mailbox <- text:
		turnCancel := rt.turnCancel
		sink := rt.emit
		m.mtx.Unlock()
		if interrupt && turnCancel != nil {
			turnCancel()
		}
		m.emit(sink, Event{
			Type: EventInputQueued, AgentID: agentID, Role: rec.Role,
			Status: StatusRunning, Text: text, Timestamp: time.Now().UTC(),
		})
		return nil
	default:
		m.mtx.Unlock()
		return ErrMailboxFull
	}
}

// ---------------------------------------------------------------------------
// Assign
// ---------------------------------------------------------------------------

// Assign persists a new assignment for the agent and then enqueues it into the
// runtime mailbox. On persistence failure the assignment is rolled back.
func (m *Manager) Assign(agentID, assignment string) error {
	m.persistMu.Lock()
	m.mtx.Lock()
	rec, ok := m.records[agentID]
	if !ok {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return ErrNotFound
	}
	rt, running := m.runtime[agentID]
	if running && rt.accepting && len(rt.mailbox) >= cap(rt.mailbox) {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return ErrMailboxFull
	}
	oldAssignment := rec.Assignment
	rec.Assignment = assignment
	m.records[agentID] = cloneRecord(rec)
	snapshot, snapErr := m.snapshotLocked()
	sink := m.sinkLocked(agentID)
	m.mtx.Unlock()
	if snapErr != nil {
		m.restoreAssignment(agentID, oldAssignment)
		m.persistMu.Unlock()
		return snapErr
	}
	if err := writeAtomic(m.path, snapshot); err != nil {
		m.restoreAssignment(agentID, oldAssignment)
		m.persistMu.Unlock()
		return err
	}
	if running && rt.accepting {
		select {
		case rt.mailbox <- assignment:
		default:
		}
	}
	m.persistMu.Unlock()
	m.emit(sink, Event{
		Type: EventAssigned, AgentID: agentID, Role: rec.Role,
		Status: rec.Status, Text: assignment, Timestamp: time.Now().UTC(),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

// Cancel cancels the agent's root context. The runAgentLoop detects ctx.Done()
// and transitions to StatusCancelled.
func (m *Manager) Cancel(agentID string) error {
	m.mtx.Lock()
	rt, ok := m.runtime[agentID]
	m.mtx.Unlock()
	if !ok {
		if _, ok := m.Result(agentID); !ok {
			return ErrNotFound
		}
		return nil // already terminal
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resume
// ---------------------------------------------------------------------------

// Resume restores a previously interrupted or completed sub-agent, reusing
// its persisted constraints (AllowedTools, Instruction, role, override).
func (m *Manager) Resume(ctx context.Context, agentID string, req ResumeRequest) (string, error) {
	if req.Runner == nil {
		return "", fmt.Errorf("resume: runner is required")
	}
	if m.closed.Load() {
		return "", ErrClosed
	}
	m.persistMu.Lock()
	m.mtx.Lock()
	if m.closed.Load() {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", ErrClosed
	}
	rec, ok := m.records[agentID]
	if !ok {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", ErrNotFound
	}
	if rec.Status == StatusRunning {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", fmt.Errorf("subagent %s is already running", agentID)
	}
	if _, exists := m.runtime[agentID]; exists {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", fmt.Errorf("subagent %s runtime already active", agentID)
	}
	if m.runningLocked() >= m.limit {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return "", &SpawnErrCap{Cap: m.limit}
	}

	// Determine assignment: req.Prompt > record.Assignment > record.Prompt.
	assignment := req.Prompt
	if assignment == "" {
		assignment = rec.Assignment
	}
	if assignment == "" {
		assignment = rec.Prompt
	}

	// Parent cancellation tree.
	parentCtx := m.rootCtx
	if rec.ParentID != "" {
		if parentRT, live := m.runtime[rec.ParentID]; live {
			parentCtx = parentRT.ctx
		}
	}

	oldRec := cloneRecord(rec)
	rec.Status = StatusRunning
	rec.Error = ""
	rec.EndedAt = time.Time{}
	rec.StartedAt = time.Now().UTC()
	rec.Assignment = assignment
	rec.SessionBootID = m.bootID
	m.records[agentID] = cloneRecord(rec)
	childCtx, childCancel := context.WithCancel(parentCtx)
	rt := &runtimeAgent{
		ctx:        childCtx,
		cancel:     childCancel,
		accepting:  true,
		assignment: assignment,
		runner:     req.Runner,
		emit:       req.Emit,
		mailbox:    make(chan string, 8),
		done:       make(chan struct{}),
	}
	m.runtime[agentID] = rt
	snapshot, snapErr := m.snapshotLocked()
	m.mtx.Unlock()
	if snapErr != nil {
		m.restoreRecord(agentID, oldRec, childCancel)
		m.persistMu.Unlock()
		return "", snapErr
	}
	if err := writeAtomic(m.path, snapshot); err != nil {
		m.restoreRecord(agentID, oldRec, childCancel)
		m.persistMu.Unlock()
		return "", err
	}
	m.persistMu.Unlock()

	m.emit(req.Emit, Event{
		Type: EventResumed, AgentID: agentID, ParentID: rec.ParentID, Role: rec.Role,
		Status: StatusRunning, Timestamp: time.Now().UTC(),
	})
	m.wg.Add(1)
	go m.runAgentLoop(childCtx, agentID, rt)
	return agentID, nil
}

func (m *Manager) restoreRecord(agentID string, old Record, cancelToCall context.CancelFunc) {
	m.mtx.Lock()
	m.records[agentID] = old
	delete(m.runtime, agentID)
	m.mtx.Unlock()
	if cancelToCall != nil {
		cancelToCall()
	}
}

// ---------------------------------------------------------------------------

// Wait blocks until the sub-agent reaches a terminal state or the context is
// done. Returns (Record, nil) on terminal; (latest Record, ctx.Err()) on
// timeout/cancel; (Record{}, ErrNotFound) when the agent does not exist.
// opts.Timeout imposes an additional deadline on top of ctx.
func (m *Manager) Wait(ctx context.Context, agentID string, opts WaitOpts) (Record, error) {
	if rec, ok := m.Result(agentID); ok && rec.Status.Terminal() {
		return rec, nil
	}
	var done chan struct{}
	m.mtx.RLock()
	rt, ok := m.runtime[agentID]
	if ok {
		done = rt.done
	}
	m.mtx.RUnlock()

	waitCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	if done == nil {
		rec, ok := m.Result(agentID)
		if !ok {
			return Record{}, ErrNotFound
		}
		return rec, nil
	}
	select {
	case <-waitCtx.Done():
		rec, _ := m.Result(agentID)
		return rec, waitCtx.Err()
	case <-done:
		rec, _ := m.Result(agentID)
		return rec, nil
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

// Close cancels the Manager root context and waits for all running sub-agents
// to finish. It best-effort persists the terminal state. Close is idempotent.
func (m *Manager) Close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	m.rootCancel()
	m.wg.Wait()
	// Best-effort final persist.
	m.persistMu.Lock()
	m.mtx.Lock()
	snapshot, _ := m.snapshotLocked()
	m.mtx.Unlock()
	if snapshot != nil {
		_ = writeAtomic(m.path, snapshot)
	}
	m.persistMu.Unlock()
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (m *Manager) nextID() string {
	n := m.agentIDSeq.Add(1)
	return fmt.Sprintf("ag-%d-%d", time.Now().UnixMilli(), n)
}

// runningLocked returns the max of runtime map entries and StatusRunning
// records. The two can diverge transiently under heavy concurrency; taking
// the max is conservative (prefer spawning fewer than budgeted over more).
// If RAC1 reports a measurable race here, resolution: use the runtime map
// as the sole authoritative count (the records count is only advisory).
func (m *Manager) runningLocked() int {
	n := 0
	for _, rt := range m.runtime {
		if rt != nil {
			n++
		}
	}
	// Cross-check with records: only count StatusRunning records.
	count := 0
	for _, rec := range m.records {
		if rec.Status == StatusRunning {
			count++
		}
	}
	if n > count {
		return n
	}
	return count
}

func (m *Manager) snapshotLocked() ([]byte, error) {
	agents := make([]Record, 0, len(m.records))
	for _, rec := range m.records {
		agents = append(agents, rec)
	}
	return marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: m.bootID,
		Agents:        agents,
	})
}

func (m *Manager) newRuntimeAgent(runner Runner, emit EventSink, assignment string) *runtimeAgent {
	return &runtimeAgent{
		accepting:  true,
		assignment: assignment,
		mailbox:    make(chan string, 8),
		emit:       emit,
		runner:     runner,
		done:       make(chan struct{}),
	}
}

func (m *Manager) runtimeChildCtx(parentID string) context.Context {
	if parentID != "" {
		if parentRT, live := m.runtime[parentID]; live {
			return parentRT.ctx
		}
	}
	return m.rootCtx
}

// runAgentLoop manages the sub-agent lifecycle. It calls the runner's Run
// method once per assignment, then transitions to terminal state. When the
// mailbox has additional input, Run is called again with the new assignment.
func (m *Manager) runAgentLoop(ctx context.Context, agentID string, rt *runtimeAgent) {
	defer m.wg.Done()
	defer close(rt.done)

	// Assign context to runtime for parent-child cancellation.
	m.mtx.Lock()
	if existing, ok := m.runtime[agentID]; ok {
		existing.ctx = ctx
	}
	m.mtx.Unlock()

	// Emit started event (outside lock).
	m.emit(rt.emit, Event{
		Type: EventStarted, AgentID: agentID, Role: m.recordRole(agentID),
		Status: StatusRunning, Timestamp: time.Now().UTC(),
	})

	// Bind the agent's role for the whole run. This is the only place every
	// sub-agent run funnels through, and it is what makes the role catalog in
	// internal/tools/agentroles.go take effect: the orchestrator reads
	// RoleFromContext to pick the PromptPrefix and the per-role
	// tools.RolePolicy. Without this binding the lookup runs against the empty
	// string and all seven role definitions are inert.
	//
	// An empty role is deliberately NOT bound: it matches no catalog entry and
	// would shadow any role bound further out with a useless value.
	if role := m.recordRole(agentID); role != "" {
		ctx = WithRole(ctx, role)
	}

	assignment := rt.assignment
	var result string
	var runErr error

	// Run loop: execute once, then re-execute if mailbox has more input.
	for {
		result, runErr = rt.runner.Run(ctx, agentID, assignment)
		if runErr != nil {
			break
		}
		// Check for additional assignment in mailbox.
		select {
		case assignment = <-rt.mailbox:
			// Continue with new assignment.
		default:
			assignment = "" // no more input; signal completed
		}
		if assignment == "" {
			break // terminal (no error)
		}
	}

	// Determine terminal status.
	status := StatusCompleted
	errMsg := ""
	if runErr != nil {
		status = StatusFailed
		errMsg = runErr.Error()
	}
	// If the context was cancelled, always report cancelled regardless of what
	// the runner returned (race: runner may finish between cancellation and
	// when runAgentLoop checks the context).
	if ctx.Err() != nil {
		status = StatusCancelled
		errMsg = ctx.Err().Error()
	}

	// Finish terminal (persist + events).
	m.finishTerminal(agentID, status, result, errMsg)
}

func (m *Manager) recordRole(agentID string) string {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	if rec, ok := m.records[agentID]; ok {
		return rec.Role
	}
	return ""
}

func (m *Manager) emit(sink EventSink, ev Event) {
	if sink != nil {
		sink(ev)
	}
}

func copyStrings(src []string) []string {
	if src == nil {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func copyCustomRole(src *CustomRole) *CustomRole {
	if src == nil {
		return nil
	}
	c := *src
	if src.AllowedTools != nil {
		c.AllowedTools = make([]string, len(src.AllowedTools))
		copy(c.AllowedTools, src.AllowedTools)
	}
	return &c
}

// finishTerminal persists the terminal record and emits events outside Manager
// locks. The event order is: (on persist failure) persistence_failed → terminal.
func (m *Manager) finishTerminal(agentID string, status Status, result, errMsg string) {
	m.persistMu.Lock()
	m.mtx.Lock()
	rec, ok := m.records[agentID]
	if !ok {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return
	}
	rec.Status = status
	rec.Result = result
	rec.Error = errMsg
	rec.EndedAt = time.Now().UTC()
	m.records[agentID] = cloneRecord(rec)
	snapshot, snapErr := m.snapshotLocked()
	sink := m.sinkLocked(agentID)
	m.mtx.Unlock()

	terminalEvent := Event{
		Type: mapStatusToEvent(status), AgentID: rec.ID, ParentID: rec.ParentID, Role: rec.Role,
		Status: status, Text: result, Usage: rec.Usage, Timestamp: rec.EndedAt,
	}
	if snapErr == nil {
		snapErr = writeAtomic(m.path, snapshot)
	}
	if snapErr != nil {
		m.emit(sink, Event{
			Type: EventPersistenceFailed, AgentID: rec.ID, ParentID: rec.ParentID, Role: rec.Role,
			Status: status, Text: snapErr.Error(), Timestamp: time.Now().UTC(),
		})
	}
	m.emit(sink, terminalEvent)
	m.persistMu.Unlock()
}

func (m *Manager) sinkLocked(agentID string) EventSink {
	rt, ok := m.runtime[agentID]
	if !ok {
		return nil
	}
	return rt.emit
}

func (m *Manager) restoreAssignment(agentID, old string) {
	m.mtx.Lock()
	if rec, ok := m.records[agentID]; ok {
		rec.Assignment = old
		m.records[agentID] = cloneRecord(rec)
	}
	m.mtx.Unlock()
}

func mapStatusToEvent(s Status) EventType {
	switch s {
	case StatusCompleted:
		return EventCompleted
	case StatusFailed:
		return EventFailed
	case StatusCancelled:
		return EventCancelled
	case StatusInterrupted:
		return EventType("interrupted")
	default:
		return EventFailed
	}
}

// ---------------------------------------------------------------------------
// AddUsage
// ---------------------------------------------------------------------------

// AddUsage accumulates token usage for a managed sub-agent. On persistence
// failure the usage is rolled back in memory and a persistence_failed event
// is emitted (outside the Manager lock).
func (m *Manager) AddUsage(agentID string, delta Usage) error {
	if m.closed.Load() {
		return ErrClosed
	}
	m.persistMu.Lock()
	m.mtx.Lock()
	rec, ok := m.records[agentID]
	if !ok {
		m.mtx.Unlock()
		m.persistMu.Unlock()
		return ErrNotFound
	}
	oldUsage := rec.Usage
	rec.Usage = rec.Usage.Add(delta)
	m.records[agentID] = cloneRecord(rec)
	snapshot, snapErr := m.snapshotLocked()
	sink := m.sinkLocked(agentID)
	role := rec.Role
	status := rec.Status
	newUsage := rec.Usage
	m.mtx.Unlock()
	if snapErr != nil {
		m.restoreUsage(agentID, oldUsage)
		m.persistMu.Unlock()
		m.emit(sink, Event{
			Type: EventPersistenceFailed, AgentID: agentID, Role: role, Status: status,
			Text: snapErr.Error(), Timestamp: time.Now().UTC(),
		})
		return snapErr
	}
	if err := writeAtomic(m.path, snapshot); err != nil {
		m.restoreUsage(agentID, oldUsage)
		m.persistMu.Unlock()
		m.emit(sink, Event{
			Type: EventPersistenceFailed, AgentID: agentID, Role: role, Status: status,
			Text: err.Error(), Timestamp: time.Now().UTC(),
		})
		return err
	}
	m.persistMu.Unlock()
	m.emit(sink, Event{
		Type: EventType("usage"), AgentID: agentID, Role: role,
		Status: status, Usage: newUsage, Timestamp: time.Now().UTC(),
	})
	return nil
}

func (m *Manager) restoreUsage(agentID string, old Usage) {
	m.mtx.Lock()
	if rec, ok := m.records[agentID]; ok {
		rec.Usage = old
		m.records[agentID] = cloneRecord(rec)
	}
	m.mtx.Unlock()
}
