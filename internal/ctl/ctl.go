// Package ctl is the operator control plane: the operations an operator (or a
// script) performs on a running or stored yanshi project, expressed once and
// consumed by every surface that needs them.
//
// Why it exists as its own package: the same operations were reachable three
// different ways before it — the TUI's WebSocket control frames, the CLI's
// hand-written subcommands, and the JSON-RPC app-server — and each surface
// carried its own copy of "list sessions", "read a job", "toggle a feature".
// Copies drift. The rule this package enforces is that a new capability lands
// here once, and the surfaces become thin adapters over it.
//
// Two kinds of operation live in here, and the difference matters to a caller:
//
//   - STORED operations (sessions, usage, persisted approvals) read and write
//     the project database. They work with no daemon running, which is what
//     makes `yanshi session` usable when the backend is wedged.
//   - LIVE operations (jobs, MCP connections, feature flags, skills, the
//     agent's in-memory approval rules, VCS) are state held by a PROCESS. A
//     second process cannot read them from the database, and pretending
//     otherwise is how an operator ends up staring at a stale list. Each live
//     operation returns ErrNeedsDaemon when its manager is not wired, and the
//     CLI turns that into "start one with `yanshi -b`" rather than an empty
//     result that reads like "there is nothing".
package ctl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// ErrNeedsDaemon marks an operation whose state lives in a running process.
//
// It is a distinct error rather than an empty result because the two are not
// interchangeable to the caller: "no background jobs" and "I cannot see the
// process that has them" lead to different actions, and the second one is a
// wiring fact the operator can fix (`yanshi -b`).
var ErrNeedsDaemon = errors.New("ctl: this information lives in a running daemon; start one with `yanshi -b`")

// ErrNotFound marks a missing subject (unknown session id, unknown skill).
var ErrNotFound = errors.New("ctl: not found")

// Deps are the managers a Service operates on. Every field is optional: a
// Service built with only Store serves the stored operations and reports
// ErrNeedsDaemon for the rest, which is exactly what an offline CLI call does.
type Deps struct {
	ConfigPath string
	Store      *store.Store
	// Close releases what Open created (the store). Nil for a Service built
	// around managers the caller owns.
	Close func()

	Skills    *skills.Registry
	MCP       *mcp.Manager
	Jobs      *shell.Manager
	Features  *features.Registry
	Approvals *approval.Manager
	VCS       *vcs.VCS
	VCSRepoID string
	// Models is the per-name model registry the agent can switch between.
	Models []string
}

// Service is the control plane. All methods are safe for concurrent use to the
// extent their underlying managers are (the store serialises writes, and each
// manager is documented as goroutine-safe by its own package).
type Service struct {
	deps Deps
}

// New wraps an already-assembled set of managers.
func New(deps Deps) *Service { return &Service{deps: deps} }

// Open builds a Service for a project from its config file alone: the store is
// opened (and closed by the returned func) and the persistent half of the
// approval rules is loaded. Live managers stay nil — see the package comment.
//
// SelfHeal is deliberately NOT enabled here, matching every other incidental
// reader (cmd/yanshi's queue drainer, doctor): a repair initiated by a `list`
// command is a surprise, and the daemon owns healing.
func Open(configPath string) (*Service, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("ctl: load config %s: %w", configPath, err)
	}
	if cfg.Storage.SQLitePath == "" {
		return nil, fmt.Errorf("ctl: config %s sets no storage.sqlite_path", configPath)
	}
	st, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("ctl: open %s: %w", cfg.Storage.SQLitePath, err)
	}
	approvals, err := approval.New(st, "yanshi-cli", nil)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("ctl: approval rules: %w", err)
	}
	return &Service{deps: Deps{
		ConfigPath: configPath,
		Store:      st,
		Close:      func() { _ = st.Close() },
		Approvals:  approvals,
	}}, nil
}

// Close releases what this Service owns.
func (s *Service) Close() {
	if s.deps.Close != nil {
		s.deps.Close()
	}
}

// ---------------------------------------------------------------------------
// sessions (stored)
// ---------------------------------------------------------------------------

// SessionView is the JSON shape every surface prints for a session. Field
// names are lowerCamel and stable; a script reads them.
type SessionView struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Archived  bool    `json:"archived"`
	CreatedAt string  `json:"createdAt,omitempty"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
	Model     string  `json:"model,omitempty"`
	Thinking  string  `json:"thinking,omitempty"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	CachedIn  int     `json:"cachedTokens,omitempty"`
	Reasoning int     `json:"reasoningTokens,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	CostKnown bool    `json:"costKnown"`
	Messages  int     `json:"messages,omitempty"`
}

func sessionView(s store.SessionSummary) SessionView {
	v := SessionView{
		ID: s.ID, Title: s.Title, Archived: s.Archived,
		Model: s.Model, Thinking: s.Thinking, Turns: s.Turns,
		TokensIn: s.TokensIn, TokensOut: s.TokensOut,
		CachedIn: s.CachedTokens, Reasoning: s.ReasoningTokens,
		CostUSD: s.CostUSD, CostKnown: s.CostKnown,
	}
	if s.CreatedAt > 0 {
		v.CreatedAt = time.Unix(s.CreatedAt, 0).Format(time.RFC3339)
	}
	if s.UpdatedAt > 0 {
		v.UpdatedAt = time.Unix(s.UpdatedAt, 0).Format(time.RFC3339)
	}
	return v
}

func (s *Service) requireStore() (*store.Store, error) {
	if s.deps.Store == nil {
		return nil, ErrNeedsDaemon
	}
	return s.deps.Store, nil
}

// Sessions lists stored sessions, newest first.
func (s *Service) Sessions(limit int, archived bool) ([]SessionView, error) {
	st, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	var rows []store.SessionSummary
	if archived {
		rows, err = st.ListArchivedSessions(limit)
	} else {
		rows, err = st.ListSessions(limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionView(r))
	}
	return out, nil
}

// Session returns one session with its message count.
func (s *Service) Session(id string) (SessionView, error) {
	st, err := s.requireStore()
	if err != nil {
		return SessionView{}, err
	}
	summary, err := st.GetSession(id)
	if err != nil {
		return SessionView{}, err
	}
	if summary == nil {
		return SessionView{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	view := sessionView(*summary)
	if n, cerr := st.SessionMessageCount(id); cerr == nil {
		view.Messages = n
	}
	return view, nil
}

// requireSession fails unless the session exists.
//
// The store's mutators are `UPDATE/DELETE … WHERE id = ?`, which report SUCCESS
// for zero matching rows. On the operator surface that is a lie with a specific
// cost: measured, `yanshi ipc session/delete -params '{"id":"does-not-exist",
// "confirm":"yes"}'` answered {"ok":true} and exit 0 — a script cannot tell a
// typo'd id from a deleted session, which is exactly what the confirm guard
// exists to prevent. Every mutation below therefore checks first.
func (s *Service) requireSession(id, verb string) error {
	if id == "" {
		return fmt.Errorf("ctl: %s needs a session id", verb)
	}
	if _, err := s.Session(id); err != nil {
		return err
	}
	return nil
}

// RenameSession sets a session's title.
func (s *Service) RenameSession(id, title string) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	if err := s.requireSession(id, "rename"); err != nil {
		return err
	}
	return st.UpdateSessionTitle(id, title)
}

// SetSessionArchived hides or restores a session.
func (s *Service) SetSessionArchived(id string, archived bool) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	if err := s.requireSession(id, "archive"); err != nil {
		return err
	}
	return st.SetSessionArchived(id, archived)
}

// DeleteSession deletes a session and its messages.
func (s *Service) DeleteSession(id string) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	if err := s.requireSession(id, "delete"); err != nil {
		return err
	}
	return st.DeleteSession(id)
}

// ForkSession copies a session into a new one, truncated at upToSeq (-1 keeps
// everything). It is the store-level half of the TUI's /fork and of pi's
// --fork: exploring "what if I had answered differently" without disturbing the
// conversation that actually happened.
//
// The fork is a stored operation (no daemon needed) because a session is rows.
func (s *Service) ForkSession(id string, upToSeq int) (string, error) {
	st, err := s.requireStore()
	if err != nil {
		return "", err
	}
	if err := s.requireSession(id, "fork"); err != nil {
		return "", err
	}
	return st.ForkSession(id, upToSeq)
}

// SessionMessages returns the last n messages of a session (all when n <= 0),
// newest last, clipped for transport safety by the caller if it needs to.
func (s *Service) SessionMessages(id string, n int) ([]MessageView, error) {
	st, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	msgs, err := st.Messages(id)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, MessageView{Seq: m.Seq, Role: m.Role, Content: m.Content, ToolName: m.ToolName})
	}
	return out, nil
}

// MessageView is one transcript row.
type MessageView struct {
	Seq      int    `json:"seq"`
	Role     string `json:"role"`
	Content  string `json:"content"`
	ToolName string `json:"toolName,omitempty"`
}

// UsageView is a token/cost roll-up: one session, or many.
type UsageView struct {
	Sessions  int     `json:"sessions"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	CachedIn  int     `json:"cachedTokens,omitempty"`
	Reasoning int     `json:"reasoningTokens,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	// CostKnown is false when ANY counted session had an unpriced provider, so
	// the cost figure is a floor rather than a total. Reporting a number that
	// looks complete when part of it is unknown is the failure this flag
	// prevents: the TUI renders the same condition as "N/A".
	CostKnown bool `json:"costKnown"`
}

func (u *UsageView) add(v SessionView) {
	u.Sessions++
	u.Turns += v.Turns
	u.TokensIn += v.TokensIn
	u.TokensOut += v.TokensOut
	u.CachedIn += v.CachedIn
	u.Reasoning += v.Reasoning
	u.CostUSD += v.CostUSD
	if !v.CostKnown {
		u.CostKnown = false
	}
}

// Usage rolls up the most recent limit sessions (one session when id is set).
//
// Why a roll-up rather than one number: the question an operator actually asks
// is "what has this been costing me lately", and the per-session ledger is
// already in the store. limit bounds the read the same way `session list` does.
func (s *Service) Usage(id string, limit int) (UsageView, error) {
	if id != "" {
		view, err := s.Session(id)
		if err != nil {
			return UsageView{}, err
		}
		u := UsageView{CostKnown: true}
		u.add(view)
		return u, nil
	}
	rows, err := s.Sessions(limit, false)
	if err != nil {
		return UsageView{}, err
	}
	u := UsageView{CostKnown: true}
	for _, r := range rows {
		u.add(r)
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// features (live)
// ---------------------------------------------------------------------------

// FeatureView is one flag: what it is, its stage, and its current value.
type FeatureView struct {
	Key     string `json:"key"`
	Stage   string `json:"stage"`
	Owner   string `json:"owner,omitempty"`
	Enabled bool   `json:"enabled"`
}

// Features lists the runtime feature-flag table.
func (s *Service) Features() ([]FeatureView, error) {
	if s.deps.Features == nil {
		return nil, ErrNeedsDaemon
	}
	rows := s.deps.Features.List()
	out := make([]FeatureView, 0, len(rows))
	for _, r := range rows {
		out = append(out, FeatureView{Key: r.Key, Stage: r.Stage, Owner: r.Owner, Enabled: r.Enabled})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// SetFeature applies a NON-PERSISTENT runtime override, exactly like the TUI's
// /features: the config file is not written, so the change lasts as long as the
// daemon. An operator who wants it permanently edits config.yaml — a CLI that
// rewrote the file behind their back would be a bigger surprise than a restart.
func (s *Service) SetFeature(key string, enabled bool) error {
	if s.deps.Features == nil {
		return ErrNeedsDaemon
	}
	return s.deps.Features.Set(key, enabled)
}

// ---------------------------------------------------------------------------
// skills (live)
// ---------------------------------------------------------------------------

// SkillView is one loaded skill.
type SkillView struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	Dir         string   `json:"dir,omitempty"`
	Enabled     bool     `json:"enabled"`
	Trusted     bool     `json:"trusted"`
	Requires    []string `json:"requires,omitempty"`
	// Missing names required programs that are not on PATH: non-empty means the
	// skill's first step would fail here, which is worth knowing BEFORE a turn.
	Missing []string `json:"missing,omitempty"`
	// Unsafe is non-empty when the content scan withheld the skill from the
	// model. It is reported, never hidden: a skill that silently does nothing
	// is indistinguishable from a broken one.
	Unsafe []string `json:"unsafe,omitempty"`
}

func skillView(sk *skills.Skill) SkillView {
	v := SkillView{
		Name: sk.Name, Description: sk.Description, Source: sk.Source, Dir: sk.Dir,
		Enabled: sk.Enabled, Trusted: sk.Trusted, Missing: sk.Missing,
	}
	for _, r := range sk.Requires {
		v.Requires = append(v.Requires, r.Bin)
	}
	for _, f := range sk.Unsafe {
		v.Unsafe = append(v.Unsafe, f.RuleID+" ("+string(f.Severity)+"): "+f.Description)
	}
	return v
}

// Skills lists every loaded skill, including the ones withheld from the model.
func (s *Service) Skills() ([]SkillView, error) {
	if s.deps.Skills == nil {
		return nil, ErrNeedsDaemon
	}
	all := s.deps.Skills.List()
	out := make([]SkillView, 0, len(all))
	for _, sk := range all {
		out = append(out, skillView(sk))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Skill returns one skill plus its instructions body.
func (s *Service) Skill(name string) (SkillView, string, error) {
	if s.deps.Skills == nil {
		return SkillView{}, "", ErrNeedsDaemon
	}
	sk, ok := s.deps.Skills.Get(name)
	if !ok {
		return SkillView{}, "", fmt.Errorf("%w: skill %s", ErrNotFound, name)
	}
	body, err := s.deps.Skills.Body(sk)
	if err != nil {
		return skillView(sk), "", err
	}
	return skillView(sk), body, nil
}

// SetSkillEnabled enables or disables a skill (persisted in its directory as
// the absence/presence of .disabled, which is what the loader reads back).
func (s *Service) SetSkillEnabled(name string, enabled bool) error {
	if s.deps.Skills == nil {
		return ErrNeedsDaemon
	}
	if enabled {
		return s.deps.Skills.Enable(name)
	}
	return s.deps.Skills.Disable(name)
}

// SetSkillTrusted records (or withdraws) operator review of a skill. Trust is
// NOT an execution grant — it never authorizes running a script; it records
// that a human looked.
func (s *Service) SetSkillTrusted(name string, trusted bool) error {
	if s.deps.Skills == nil {
		return ErrNeedsDaemon
	}
	if trusted {
		return s.deps.Skills.Trust(name)
	}
	return s.deps.Skills.Untrust(name)
}

// ---------------------------------------------------------------------------
// approvals (live for session rules, stored for persistent ones)
// ---------------------------------------------------------------------------

// ApprovalView is one remembered approval rule.
type ApprovalView struct {
	ID        string `json:"id"`
	Action    string `json:"action,omitempty"`
	Tool      string `json:"tool,omitempty"`
	TTL       string `json:"ttl"`
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// Approvals lists the rules visible to sessionID. Passing "" lists the
// persistent rules only, which is all an offline reader can see — session and
// one-shot rules live in the daemon's memory by design.
func (s *Service) Approvals(sessionID string) ([]ApprovalView, error) {
	if s.deps.Approvals == nil {
		return nil, ErrNeedsDaemon
	}
	rules := s.deps.Approvals.List(sessionID, time.Now())
	out := make([]ApprovalView, 0, len(rules))
	for _, r := range rules {
		v := ApprovalView{
			ID: r.ID, Action: r.Action, TTL: string(r.TTL), Source: string(r.Source),
			Tool: r.Scope.Tool,
		}
		if !r.CreatedAt.IsZero() {
			v.CreatedAt = r.CreatedAt.Format(time.RFC3339)
		}
		if !r.ExpiresAt.IsZero() {
			v.ExpiresAt = r.ExpiresAt.Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out, nil
}

// RevokeApproval drops one rule by id.
func (s *Service) RevokeApproval(sessionID, id string) error {
	if s.deps.Approvals == nil {
		return ErrNeedsDaemon
	}
	if id == "" {
		return fmt.Errorf("ctl: revoke needs a rule id")
	}
	return s.deps.Approvals.Revoke(sessionID, id)
}

// ---------------------------------------------------------------------------
// background jobs (live)
// ---------------------------------------------------------------------------

// JobView is one background shell job.
type JobView struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Command   string `json:"command"`
	State     string `json:"state"`
	ExitCode  int    `json:"exitCode"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	// OutputLen is a size, not the output: listing jobs must not pull megabytes
	// of command output into a list render. jobs/read fetches the bytes.
	OutputLen int `json:"outputLen"`
}

// Jobs lists the background jobs this daemon owns.
func (s *Service) Jobs() ([]JobView, error) {
	if s.deps.Jobs == nil {
		return nil, ErrNeedsDaemon
	}
	jobs := s.deps.Jobs.ListJobs()
	out := make([]JobView, 0, len(jobs))
	for _, j := range jobs {
		v := JobView{
			ID: j.ID, SessionID: j.SessionID, Command: j.Command, State: string(j.State),
			ExitCode: j.ExitCode, PID: j.PID, OutputLen: len(j.Output),
		}
		if !j.StartedAt.IsZero() {
			v.StartedAt = j.StartedAt.Format(time.RFC3339)
		}
		if j.EndedAt != nil {
			v.EndedAt = j.EndedAt.Format(time.RFC3339)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}

// ReadJob returns a job's output (the last max bytes when max > 0).
func (s *Service) ReadJob(id string, max int) (string, error) {
	if s.deps.Jobs == nil {
		return "", ErrNeedsDaemon
	}
	return s.deps.Jobs.ReadJob(id, max)
}

// WriteJob sends bytes to a job's stdin.
func (s *Service) WriteJob(id, data string) (int, error) {
	if s.deps.Jobs == nil {
		return 0, ErrNeedsDaemon
	}
	return s.deps.Jobs.WriteJob(id, []byte(data))
}

// CancelJob stops a running job.
func (s *Service) CancelJob(id string) error {
	if s.deps.Jobs == nil {
		return ErrNeedsDaemon
	}
	return s.deps.Jobs.CancelJob(id)
}

// ---------------------------------------------------------------------------
// MCP (live)
// ---------------------------------------------------------------------------

// MCPView is one configured MCP server.
type MCPView struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Resources int      `json:"resources,omitempty"`
}

// MCP lists the configured MCP servers with their live connection status.
func (s *Service) MCP(ctx context.Context) ([]MCPView, error) {
	if s.deps.MCP == nil {
		return nil, ErrNeedsDaemon
	}
	statuses := s.deps.MCP.Snapshot(ctx)
	out := make([]MCPView, 0, len(statuses))
	for _, st := range statuses {
		v := MCPView{
			Name: st.Name, Transport: string(st.Transport), Status: string(st.Status),
			Error: st.Error, Resources: len(st.Resources),
		}
		for _, td := range st.Tools {
			v.Tools = append(v.Tools, td.Qualified)
		}
		sort.Strings(v.Tools)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetMCPEnabled connects or disconnects one MCP server.
func (s *Service) SetMCPEnabled(ctx context.Context, name string, enabled bool) error {
	if s.deps.MCP == nil {
		return ErrNeedsDaemon
	}
	if enabled {
		return s.deps.MCP.Enable(ctx, name)
	}
	return s.deps.MCP.Disable(ctx, name)
}

// ---------------------------------------------------------------------------
// VCS (live)
// ---------------------------------------------------------------------------

// CommitView is one autoVCS commit.
type CommitView struct {
	ID           string `json:"id"`
	Author       string `json:"author,omitempty"`
	Message      string `json:"message,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	FilesChanged int    `json:"filesChanged"`
	MergedFrom   string `json:"mergedFrom,omitempty"`
}

// VCSLog returns the last limit commits on the main branch.
func (s *Service) VCSLog(limit int) ([]CommitView, error) {
	if s.deps.VCS == nil {
		return nil, ErrNeedsDaemon
	}
	if s.deps.VCSRepoID == "" {
		return nil, fmt.Errorf("%w: no VCS repository in this project", ErrNotFound)
	}
	if limit <= 0 {
		limit = 20
	}
	commits, err := s.deps.VCS.LogMain(s.deps.VCSRepoID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]CommitView, 0, len(commits))
	for _, c := range commits {
		v := CommitView{ID: c.ID, Author: c.Author, Message: c.Message, FilesChanged: c.FilesChanged, MergedFrom: c.MergedFrom}
		if c.CreatedAt > 0 {
			v.CreatedAt = time.Unix(c.CreatedAt, 0).Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out, nil
}

// FileDiffView is one changed path between two refs.
type FileDiffView struct {
	Path    string `json:"path"`
	Op      string `json:"op"`
	OldHash string `json:"oldHash,omitempty"`
	NewHash string `json:"newHash,omitempty"`
}

// VCSDiff returns the path-level changes between two refs. An empty refB means
// "compare refA with the current main head".
func (s *Service) VCSDiff(refA, refB string) ([]FileDiffView, error) {
	if s.deps.VCS == nil {
		return nil, ErrNeedsDaemon
	}
	if s.deps.VCSRepoID == "" {
		return nil, fmt.Errorf("%w: no VCS repository in this project", ErrNotFound)
	}
	if refB == "" {
		head, err := s.deps.VCS.RepoMainHead(s.deps.VCSRepoID)
		if err != nil {
			// An empty repository has no head, and the store reports that as
			// sql.ErrNoRows. Leaking that to a caller produces `vcs/diff: sql:
			// no rows in result set`, which names the layer rather than the
			// situation. Measured on a project whose repo had no commits.
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: project has no commits yet", ErrNotFound)
			}
			return nil, err
		}
		refB = head
	}
	// Validate both refs BEFORE diffing. Diff resolves missing ids to empty
	// trees, so an unknown ref would come back as "every file was added" —
	// a plausible-looking answer to a typo. Measured through `yanshi ipc
	// vcs/diff` with from=deadbeef: exit 0, three files "added".
	for _, ref := range []string{refA, refB} {
		if ref == "" {
			continue
		}
		if _, ok, err := s.deps.VCS.CommitInfo(ref); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("%w: no commit %s", ErrNotFound, ref)
		}
	}
	diffs, err := s.deps.VCS.Diff(s.deps.VCSRepoID, refA, refB)
	if err != nil {
		return nil, err
	}
	out := make([]FileDiffView, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, FileDiffView{Path: d.Path, Op: d.Op, OldHash: d.OldHash, NewHash: d.NewHash})
	}
	return out, nil
}

// Models returns the model names a session can switch to.
//
// It returns an EMPTY slice rather than nil when there are none: the JSON-RPC
// layer wraps this in {"models": …}, and a nil slice marshals to `null`, so a
// client doing `for m in result.models` would get a null iteration instead of
// an empty one. Measured: `yanshi ipc models/list` on a fake-model daemon
// answered {"models":null}.
func (s *Service) Models() []string {
	out := make([]string, 0, len(s.deps.Models))
	out = append(out, s.deps.Models...)
	sort.Strings(out)
	return out
}
