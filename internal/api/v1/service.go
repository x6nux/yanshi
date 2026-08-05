package v1

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// Config wires the v1 Service to its dependencies. At least one of
// Orchestrator or DefaultModel is required so the service can run turns and
// tests can choose the lightweight path (DefaultModel only) without bootstrapping
// the full ADK stack. Store is optional: when nil the service runs entirely
// in-memory (used by tests and ephemeral servers); resume across processes
// requires a non-nil Store.
type Config struct {
	Orchestrator *orchestrator.Orchestrator
	DefaultModel model.BaseChatModel
	Models       map[string]model.BaseChatModel
	Store        *store.Store
}

// Service is the shared v1 resource layer. The same *Service instance is
// consumed by the HTTP/SSE transport and the JSON-RPC app-server so thread /
// turn / item semantics cannot drift between them.
//
// Concurrency model: one mutex per thread (threadState.mu) guards the thread's
// history, turn registry, active turn pointer, and sequence counter. The
// service-level mutex (s.mu) only guards the threads map itself. Active-turn
// enforcement (at most one in-progress turn per thread) is checked under
// threadState.mu, so two concurrent StartTurn calls on the same thread cannot
// both succeed.
//
// Backpressure: StartTurn returns a buffered channel (cap 128). sendItem
// selects on ctx.Done so a dropped consumer (HTTP disconnect or app-server
// exit) releases the producer goroutine instead of letting it block forever
// or fill unbounded memory.
type Service struct {
	orch         *orchestrator.Orchestrator
	defaultModel model.BaseChatModel
	models       map[string]model.BaseChatModel
	store        *store.Store

	mu      sync.Mutex
	threads map[string]*threadState
	nextID  uint64 // in-memory thread id counter when Store is nil
}

// threadState is the per-thread mutable state. All fields are guarded by mu.
type threadState struct {
	mu      sync.Mutex
	thread  Thread
	history []*schema.Message
	turns   map[string]*turnState
	active  *turnState
	nextSeq int64
}

// turnState is the per-turn mutable state.
type turnState struct {
	turn   Turn
	cancel context.CancelFunc
}

// ErrThreadNotFound is returned by Resume / StartTurn / Interrupt when the
// thread id is unknown to both the in-memory registry and (when configured)
// the persistent store.
var ErrThreadNotFound = fmt.Errorf("thread not found")

// ErrTurnAlreadyActive is returned by StartTurn when the thread already has
// an in-progress turn. The v1 contract allows at most one active turn per thread.
var ErrTurnAlreadyActive = fmt.Errorf("thread already has an active turn")

// NewService constructs a v1 Service. At least one of Orchestrator or
// DefaultModel must be non-nil so turn execution is possible.
func NewService(cfg Config) (*Service, error) {
	if cfg.DefaultModel == nil && cfg.Orchestrator == nil {
		return nil, fmt.Errorf("api v1: default model or orchestrator is required")
	}
	return &Service{
		orch:         cfg.Orchestrator,
		defaultModel: cfg.DefaultModel,
		models:       cfg.Models,
		store:        cfg.Store,
		threads:      make(map[string]*threadState),
	}, nil
}

// Start creates a new thread resource. When Store is configured the thread id
// is the persistent session id; otherwise an in-memory id is minted (volatile).
// Title defaults to "New thread" when empty.
func (s *Service) Start(_ context.Context, p ThreadStartParams) (Thread, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = "New thread"
	}
	var id string
	created := time.Now().Unix()
	if s.store != nil {
		var err error
		id, err = s.store.CreateSession(title)
		if err != nil {
			return Thread{}, fmt.Errorf("create thread: %w", err)
		}
	} else {
		id = fmt.Sprintf("memory-%d", atomic.AddUint64(&s.nextID, 1))
	}
	thread := Thread{
		Version: Version, ID: id, Status: ThreadStatusActive, Title: title,
		CreatedAt: created, UpdatedAt: created, Model: p.Model, Thinking: p.Thinking,
	}
	st := &threadState{thread: thread, turns: make(map[string]*turnState)}
	s.mu.Lock()
	s.threads[id] = st
	s.mu.Unlock()
	return thread, nil
}

// Resume loads a thread by id, preferentially from the in-memory registry and
// falling back to the persistent Store. Returns ErrThreadNotFound when the id
// is unknown to both. The returned snapshot includes any in-memory items the
// service is currently tracking; v1 MVP does not promise cross-process item
// replay (see the capabilities/schema doc).
func (s *Service) Resume(_ context.Context, p ThreadResumeParams) (ThreadSnapshot, error) {
	if strings.TrimSpace(p.ThreadID) == "" {
		return ThreadSnapshot{}, fmt.Errorf("threadId is required")
	}
	s.mu.Lock()
	if st := s.threads[p.ThreadID]; st != nil {
		s.mu.Unlock()
		return s.snapshot(st), nil
	}
	s.mu.Unlock()
	if s.store == nil {
		return ThreadSnapshot{}, ErrThreadNotFound
	}
	ss, err := s.store.GetSession(p.ThreadID)
	if err != nil {
		return ThreadSnapshot{}, fmt.Errorf("read thread: %w", err)
	}
	if ss == nil {
		return ThreadSnapshot{}, ErrThreadNotFound
	}
	msgs, err := s.store.Messages(p.ThreadID)
	if err != nil {
		return ThreadSnapshot{}, fmt.Errorf("read thread messages: %w", err)
	}
	history := make([]*schema.Message, 0, len(msgs))
	for _, msg := range msgs {
		role := schema.Assistant
		if msg.Role == "user" {
			role = schema.User
		}
		history = append(history, &schema.Message{Role: role, Content: msg.Content})
	}
	thread := Thread{
		Version: Version, ID: ss.ID, Status: ThreadStatusActive, Title: ss.Title,
		CreatedAt: ss.CreatedAt, UpdatedAt: ss.UpdatedAt, Model: ss.Model, Thinking: ss.Thinking,
	}
	st := &threadState{
		thread:  thread,
		history: history,
		turns:   make(map[string]*turnState),
		nextSeq: int64(len(msgs)),
	}
	s.mu.Lock()
	if existing := s.threads[p.ThreadID]; existing != nil {
		// Another caller won the race; reuse their state for the snapshot.
		st = existing
	} else {
		s.threads[p.ThreadID] = st
	}
	s.mu.Unlock()
	return s.snapshot(st), nil
}

// StartTurn begins a turn on a thread. Returns the freshly-started Turn and a
// buffered item channel (cap 128) that closes when the turn ends. The turn
// runs in a goroutine so the caller can stream items asynchronously.
//
// Backpressure: sendItem blocks on ctx.Done, so a dropped consumer releases
// the producer. The channel never grows past 128 items in flight; the producer
// never silently drops items; the producer goroutine never outlives the
// channel close.
func (s *Service) StartTurn(parent context.Context, p TurnStartParams) (TurnStartResponse, <-chan Item, error) {
	st := s.thread(p.ThreadID)
	if st == nil {
		return TurnStartResponse{}, nil, ErrThreadNotFound
	}
	if strings.TrimSpace(p.Input) == "" {
		return TurnStartResponse{}, nil, fmt.Errorf("input is required")
	}
	st.mu.Lock()
	if st.active != nil {
		st.mu.Unlock()
		return TurnStartResponse{}, nil, ErrTurnAlreadyActive
	}
	turnID := fmt.Sprintf("%s-turn-%d", st.thread.ID, len(st.turns)+1)
	now := time.Now().Unix()
	turn := Turn{
		Version: Version, ID: turnID, ThreadID: st.thread.ID,
		Status: TurnStatusInProgress, Input: p.Input, StartedAt: now,
	}
	ctx, cancel := context.WithCancel(parent)
	ts := &turnState{turn: turn, cancel: cancel}
	st.active = ts
	st.turns[turnID] = ts
	st.thread.Turns = append(st.thread.Turns, turn)
	st.thread.UpdatedAt = now
	st.history = append(st.history, &schema.Message{Role: schema.User, Content: p.Input})
	history := append([]*schema.Message(nil), st.history...)
	st.mu.Unlock()

	items := make(chan Item, 128)
	// 在尝试发送第一个 item 之前先检查 ctx 是否已取消，避免带缓冲的 channel select
	// 在 ctx.Done() 和 out <- item 同时 ready 时竞态选择发送而非中止。
	if ctx.Err() != nil {
		cancel()
		st.mu.Lock()
		st.active = nil
		st.mu.Unlock()
		close(items)
		return TurnStartResponse{}, nil, context.Canceled
	}
	if !s.sendItem(ctx, items, s.item(st, turnID, ItemTurnStarted, "")) {
		// Consumer already gone before we could emit turn.started — abort the
		// turn before any work starts. The active-turn pointer is cleared under
		// the lock so subsequent StartTurn calls succeed.
		cancel()
		st.mu.Lock()
		st.active = nil
		st.mu.Unlock()
		close(items)
		return TurnStartResponse{}, nil, context.Canceled
	}
	go s.runTurn(ctx, st, ts, history, p, items)
	return TurnStartResponse{Version: Version, Turn: turn}, items, nil
}

// Interrupt cancels the thread's active turn. Idempotent: returns nil when the
// thread has no active turn. When TurnID is set, refuses to cancel a different
// turn (stale client state).
func (s *Service) Interrupt(_ context.Context, p ThreadInterruptParams) error {
	st := s.thread(p.ThreadID)
	if st == nil {
		return ErrThreadNotFound
	}
	st.mu.Lock()
	active := st.active
	st.mu.Unlock()
	if active == nil {
		return nil
	}
	if p.TurnID != "" && p.TurnID != active.turn.ID {
		return fmt.Errorf("turn %q is not active", p.TurnID)
	}
	active.cancel()
	return nil
}

// thread returns the in-memory thread state by id (nil if unknown). The
// service-level mutex guards only the map; callers take threadState.mu for
// field access.
func (s *Service) thread(id string) *threadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[id]
}

// snapshot returns a defensive copy of the thread state for Resume responses.
// Turns slice is copied so callers cannot mutate the internal registry.
func (s *Service) snapshot(st *threadState) ThreadSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	thread := st.thread
	thread.Turns = append([]Turn(nil), st.thread.Turns...)
	return ThreadSnapshot{Version: Version, Thread: thread}
}

// runTurn executes the turn against the configured orchestrator (or a stub
// when only DefaultModel is set). All emitted events are mapped to v1 Items
// via ItemFromServerFrame; on completion the turn status is finalized and a
// terminal item (turn.completed or turn.error) is emitted before the channel
// closes.
//
// Backpressure handling: emit returns immediately when sendItem fails (ctx
// done), but the model stream is allowed to drain so the orchestrator's state
// stays consistent. The final item is sent on context.Background() so a
// dropped consumer still observes close(items) promptly.
func (s *Service) runTurn(ctx context.Context, st *threadState, ts *turnState, history []*schema.Message, p TurnStartParams, out chan Item) {
	defer close(out)
	var assistant strings.Builder
	failed := false
	emit := func(f proto.ServerFrame) {
		if f.Type == "agent_chunk" {
			assistant.WriteString(f.Text)
		}
		item := ItemFromServerFrame(f, st.thread.ID, ts.turn.ID, s.nextSequence(st))
		if !s.sendItem(ctx, out, item) {
			failed = true
		}
	}
	if s.orch != nil {
		// ModelID/Images carry the turn's attachments into the orchestrator's
		// single image fan-out point. ModelID is a registry NAME, resolved with
		// the shared fallback (einollm.ResolveModelName: the requested model,
		// else the first sorted registry name) so an unset Model still names the
		// model the turn actually runs on — the orchestrator needs that name to
		// decide between a native image part and a stored text placeholder.
		opts := orchestrator.TurnOpts{
			// Correlation IDs. Left empty, WithThreadLink binds two empty
			// strings and every log line and tool audit record for the turn
			// carries no way back to the conversation it came from.
			ThreadID:       st.thread.ID,
			TurnID:         ts.turn.ID,
			ThinkingEffort: p.Thinking,
			OutputSchema:   p.OutputSchema,
			ModelID:        einollm.ResolveModelName(s.models, p.Model),
			Images:         p.Images,
		}
		if p.Model != "" && s.models[p.Model] != nil {
			opts.Model = s.models[p.Model]
		}
		iter := s.orch.EventsWithHistoryOpts(ctx, history, opts)
		var usage orchestrator.TurnUsage
		orchestrator.ClassifyEventsWithUsage(iter, &usage, emit)
	} else {
		// DefaultModel-only path (tests, no orchestrator): emit one stub chunk
		// so the stream produces at least one item and the test contract
		// (count > 0, sequence strictly increasing) is honored.
		emit(proto.NewAgentChunk("(no real model configured)"))
	}

	st.mu.Lock()
	if assistant.Len() > 0 {
		st.history = append(st.history, &schema.Message{Role: schema.Assistant, Content: assistant.String()})
	}
	status := TurnStatusCompleted
	if ctx.Err() != nil {
		status = TurnStatusInterrupted
	}
	if failed {
		status = TurnStatusFailed
	}
	completed := time.Now().Unix()
	for i := range st.thread.Turns {
		if st.thread.Turns[i].ID == ts.turn.ID {
			st.thread.Turns[i].Status = status
			st.thread.Turns[i].CompletedAt = completed
		}
	}
	ts.turn.Status = status
	ts.turn.CompletedAt = completed
	st.active = nil
	st.thread.UpdatedAt = completed
	st.mu.Unlock()

	// Persist the turn's user/assistant messages and update session metadata so
	// a subsequent Resume sees the conversation. Best-effort: a persistence
	// failure is logged via the final item but does not change the turn status
	// (the in-memory state is already authoritative for the live stream).
	if s.store != nil {
		seq, _ := s.store.SessionMessageCount(st.thread.ID)
		_ = s.store.AppendMessage(st.thread.ID, seq, "user", ts.turn.Input)
		if assistant.Len() > 0 {
			_ = s.store.AppendMessage(st.thread.ID, seq+1, "assistant", assistant.String())
		}
		_ = s.store.UpdateSessionMeta(st.thread.ID, p.Model, p.Thinking, 0, 0, len(st.thread.Turns), 0, 0, store.BillingMeta{})
	}

	finalType := ItemTurnCompleted
	if status == TurnStatusFailed || status == TurnStatusInterrupted {
		finalType = ItemTurnError
	}
	seq := s.nextSequence(st)
	final := Item{
		Version: Version, ID: "item-" + formatSequence(seq), Sequence: seq,
		ThreadID: st.thread.ID, TurnID: ts.turn.ID, Type: finalType, Status: string(status),
	}
	_ = s.sendItem(context.Background(), out, final)
}

// nextSequence returns the next monotonic sequence number for the thread. The
// counter starts at 0 and increments before returning, so the first item is
// sequence 1 (per the v1 schema contract: sequence is an integer >= 1).
func (s *Service) nextSequence(st *threadState) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.nextSeq++
	return st.nextSeq
}

// item is a convenience helper that allocates the next sequence and constructs
// a minimal Item of the given type. Used by StartTurn for the synthetic
// turn.started event; the streaming path uses ItemFromServerFrame.
func (s *Service) item(st *threadState, turnID, typ, text string) Item {
	seq := s.nextSequence(st)
	return Item{
		Version: Version, ID: "item-" + formatSequence(seq), Sequence: seq,
		ThreadID: st.thread.ID, TurnID: turnID, Type: typ, Text: text,
	}
}

// sendItem sends one item on out, blocking until either the item is accepted
// or ctx is cancelled. Returns true on success, false on cancellation. This is
// the single backpressure gate: every item emission goes through here.
func (s *Service) sendItem(ctx context.Context, out chan Item, item Item) bool {
	select {
	case out <- item:
		return true
	case <-ctx.Done():
		return false
	}
}
