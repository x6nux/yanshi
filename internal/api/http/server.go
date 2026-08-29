// Package http exposes yanshi over an HTTP API.
package http

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// Config holds settings for constructing a Server.
type Config struct {
	Token string
	// Compaction configures automatic context-compaction (Task 35b). The default
	// zero value disables compaction (threshold 0), which is what most tests want
	// — bootstrap forwards the loaded config.Compaction (defaults applied) so the
	// real app gets an enabled 0.8/4/256000 policy.
	Compaction CompactionConfig
	// Store is the persistence layer for sessions and messages. When non-nil,
	// ChatWS records conversations and supports session_list / restore_session.
	// Pass nil to disable recording (legacy behaviour).
	Store *store.Store
	// VCS + RepoID wire the autoVCS instance + repo id to the WS/SSE handlers
	// for seam lifecycle (B2-RB1). nil/"" disables seam features silently.
	VCS    *vcs.VCS
	RepoID string
	// Approvals is the shared approval manager. When non-nil, ChatWS uses it for
	// permissions_list / permission_revoke and binds a per-connection session
	// view onto connCtx so Authorize can match / record rules. When nil,
	// permissions_list returns an empty list and the connection falls back to a
	// per-connection in-memory manager (Task 8 left this in place so the WS path
	// keeps working in tests that don't wire Approvals).
	Approvals *approval.Manager
	// ApprovalAudit is the shared audit bus (mirrors the manager's emit
	// callback). When non-nil, each WS connection subscribes a goroutine that
	// forwards events as permission_rule_hit frames so the operator can audit
	// exactly which rule admitted which call, live.
	ApprovalAudit *approval.AuditBus
	// ShellManager backs the /jobs TUI command and the WS job control frames
	// (Task 22). When nil, jobs_list returns an empty list. bootstrap wires
	// the same manager the orchestrator uses so /jobs reflects every live
	// session started by shell_start / task_shell_start.
	ShellManager *shell.Manager
	// MCP is the MCP connection manager. When non-nil, mcp_action frames dispatch
	// through it. bootstrap wires the same manager the orchestrator uses.
	MCP *mcp.Manager
	// MemoryPath is the active user memory file path (MEM1), surfaced on
	// status frames so the TUI can display it. Empty when MEM1 is disabled.
	MemoryPath string
	// LogPath is the structured-log file path (empty = logs on stderr).
	// Surfaced on status frames so the TUI /logs command tails the right
	// file instead of guessing the default location.
	LogPath string
	// SkillsRegistry is the loaded skill registry (E03). When non-nil, /skills
	// and /skill mutation frames route through it.
	SkillsRegistry *skills.Registry
	// SkillsLoader must be the same loader used for bootstrap Load(), not a
	// user-only reconstruction; Reload then preserves builtin/plugin roots (FN1).
	SkillsLoader *skills.Loader
	// SkillsDstRoot is the writable user root where Install/Uninstall publish.
	SkillsDstRoot string
	// SkillsCloner is an optional test seam. nil means Install's production
	// realClone; bootstrap intentionally leaves it nil.
	SkillsCloner skills.CloneImpl
	// SkillsFetcher is the test seam for the HTTP direct-install path (T8),
	// the counterpart of SkillsCloner. nil means InstallFromURL's production
	// realFetch; bootstrap intentionally leaves it nil.
	SkillsFetcher skills.Fetcher
	// PriceTab is the per-model USD pricing table (COST1). When non-nil,
	// status/turn-end code uses it to compute the per-session cost estimate;
	// when nil, /cost renders "N/A". Populated by bootstrap via
	// einollm.MergePricing(einollm.DefaultPricing(), cfg.Pricing.Overrides).
	PriceTab map[string]eino.ModelPricing
	// FeaturesReg is the runtime feature-flag registry (OBS3). When non-nil,
	// /features_list and /features_set route through it; when nil, those
	// frames return an empty table / a "no registry" error. Populated by
	// bootstrap.
	FeaturesReg *features.Registry
	// Redactor is the process-wide secrets redactor (S10). When non-nil, the
	// WS and SSE handlers pass it to writeSSEFrame / wsConn.write so secret
	// substrings never cross the wire. nil disables redaction (tests only).
	Redactor *secrets.Redactor
	// PermissionTimeout bounds how long an interactive permission prompt waits
	// for a human, and after how many consecutive unanswered prompts the
	// connection is treated as unattended (prompts then deny immediately
	// instead of waiting). The zero value applies the package defaults; see
	// PermissionTimeoutPolicy.
	//
	// WS-only: the SSE path installs no permission callback and already fails
	// closed without waiting, so there is nothing there for a deadline to bound.
	PermissionTimeout PermissionTimeoutPolicy
	// DistillModel drives the memory-consolidation pass (A2/W-A-05): the
	// distill_memories control frame and, when the memory_distill_after_turn
	// feature flag is on, an automatic post-turn pass. bootstrap prefers the
	// cheap batch.rlm_model provider when one is configured, falling back to
	// the main chat model — a consolidation pass summarizes existing memory
	// rows, not the kind of reasoning that needs the expensive model. nil
	// disables /distill (the WS handler answers with an error frame) and
	// silently skips the post-turn pass.
	DistillModel tools.DistillModel
	// GuardianPrompt is the operator's replacement for the auto-mode risk
	// policy shown to the model (W-B-14, security.guardian_prompt_file).
	// Empty = the built-in policy. Already validated by config.Load;
	// guard.AutoApprovalPromptWith re-checks and falls back on its own, so a
	// caller that skips validation cannot hollow the gate out.
	GuardianPrompt string
}

// CompactionConfig is the http-layer mirror of config.CompactionConfig. It is
// defined here (rather than importing internal/config) so the http package stays
// decoupled from the config loader. bootstrap converts.
type CompactionConfig struct {
	Threshold     float64
	KeepRecent    int
	ContextWindow int // fallback when ProviderWindows has no entry for the model
	Model         string
	// ProviderWindows maps provider name -> token window, letting compaction
	// size against the ACTUAL model's window instead of one global value.
	// Populated by bootstrap from config.LLM.Providers[].ContextWindow.
	ProviderWindows map[string]int
}

// Server holds the mux and auth token. Use HandleFunc to register routes
// with Go 1.22 pattern routing (e.g. "POST /api/v1/chat").
type Server struct {
	// clientRegistry holds the open WebSocket connections so background
	// goroutines can push server-initiated frames. See broadcast.go.
	clientRegistry

	mux        *http.ServeMux
	token      string
	compaction CompactionConfig
	store      *store.Store // session persistence; nil = no recording
	// vcs + repoID enable the seam lifecycle (B2-RB1). Nil/"" = disabled.
	vcs           *vcs.VCS
	repoID        string
	approvals     *approval.Manager
	approvalAudit *approval.AuditBus
	// shellManager backs the /jobs TUI command and the WS job control frames
	// (Task 22). When nil, jobs_list returns an empty list and job_read/write/
	// cancel return an error frame.
	shellManager *shell.Manager
	// controlProfile is the orchestrator's profile, captured when ChatWS is
	// called so the WS handler can authorize control-frame actions
	// (job_write/job_cancel) against the same permission profile the in-flight
	// turns use. Without this, /jobs stdin/cancel would have no profile
	// context and tools.Authorize would fail closed with "no profile".
	mcp *mcp.Manager // MCP connection manager (nil = disabled)
	// memoryPath is the active user memory file path (MEM1), surfaced on status
	// frames so the TUI can display it. Empty when MEM1 is disabled.
	memoryPath string
	// logPath is the structured-log file path (empty = stderr). Surfaced on
	// status frames so the TUI /logs command tails the right file.
	logPath string
	// skillsRegistry serves current snapshots (E03); skillsLoader is the
	// ORIGINAL bootstrap loader retaining builtin+user+plugin roots (FN1).
	skillsRegistry *skills.Registry
	skillsLoader   *skills.Loader
	skillsDstRoot  string
	// skillsCloner is nil in production (Install uses real git); tests inject
	// CloneStub so handler-level WS tests are deterministic and offline.
	skillsCloner skills.CloneImpl
	// skillsFetcher is nil in production (InstallFromURL uses real HTTP);
	// tests inject a byte-serving stub for the same reason as skillsCloner.
	skillsFetcher  skills.Fetcher
	controlProfile guard.PermissionProfile
	// workRoot is the orchestrator's project root, captured alongside
	// controlProfile. @path attachments are resolved against it before the
	// turn starts, so no tools context exists yet to carry it.
	workRoot string
	// priceTab / featuresReg carry the COST1 pricing table and OBS3 feature
	// registry. They are only read by the WS handler (turn-end billing + the
	// /features_* control frames); nil means "render N/A" / "no registry".
	priceTab    map[string]eino.ModelPricing
	featuresReg *features.Registry
	// redactor is the process-wide secrets redactor (S10). nil disables
	// redaction (pre-D3 behaviour; tests that don't wire a Redactor).
	redactor *secrets.Redactor
	// permTimeout is the resolved approval-countdown + unattended-latch policy
	// (S5). Stored resolved so every connection reads the same numbers the
	// countdown on the wire advertises.
	permTimeout PermissionTimeoutPolicy
	// distillModel backs the distill_memories control frame and the post-turn
	// pass (A2/W-A-05). nil disables both (see Config.DistillModel).
	distillModel tools.DistillModel
	// guardianPrompt is Config.GuardianPrompt, copied onto each connSession so
	// the permission callback can reach it without a Server pointer.
	guardianPrompt string
}

// New creates a Server with the given configuration.
func New(cfg Config) *Server {
	return &Server{
		mux:            http.NewServeMux(),
		token:          cfg.Token,
		compaction:     cfg.Compaction,
		store:          cfg.Store,
		vcs:            cfg.VCS,
		repoID:         cfg.RepoID,
		approvals:      cfg.Approvals,
		approvalAudit:  cfg.ApprovalAudit,
		shellManager:   cfg.ShellManager,
		mcp:            cfg.MCP,
		memoryPath:     cfg.MemoryPath,
		logPath:        cfg.LogPath,
		skillsRegistry: cfg.SkillsRegistry,
		skillsLoader:   cfg.SkillsLoader,
		skillsDstRoot:  cfg.SkillsDstRoot,
		skillsCloner:   cfg.SkillsCloner,
		skillsFetcher:  cfg.SkillsFetcher,
		priceTab:       cfg.PriceTab,
		featuresReg:    cfg.FeaturesReg,
		redactor:       cfg.Redactor,
		permTimeout:    cfg.PermissionTimeout.resolve(),
		distillModel:   cfg.DistillModel,
		guardianPrompt: cfg.GuardianPrompt,
	}
}

// Handler returns the http.Handler with token-auth middleware applied.
func (s *Server) Handler() http.Handler {
	return s.auth(s.mux)
}

// HandleFunc registers a route on the underlying mux.
// The pattern follows Go 1.22 ServeMux routing (e.g. "GET /healthz").
func (s *Server) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	s.mux.HandleFunc(pattern, h)
}

// auth wraps next with token authentication. /healthz and /readyz are public.
// Connections from loopback (127.0.0.1 / ::1) are trusted without a token —
// this is what lets the local CLI and the in-process TUI talk to a local
// backend without a token. Non-loopback clients still require a matching
// Bearer token.
//
// /readyz is exempted for the same reason /healthz is, and the reason is not
// "they are both health-ish". A readiness probe is run by whatever supervises
// the process — a container orchestrator, a load balancer, a monitoring check
// — and none of those hold a session token. Requiring one turns every remote
// readiness probe into a 401, which reads as "not ready" forever and takes the
// backend out of rotation permanently. Neither endpoint discloses anything:
// both answer with a status code and an empty body.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicProbePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if isLoopback(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || got != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isPublicProbePath reports whether p is one of the two unauthenticated probe
// endpoints (liveness and readiness), including their sub-paths.
//
// The sub-path prefix match is kept from the original /healthz rule: a probe
// configured as /healthz/db must not become a 401 just because someone
// appended a segment, and neither endpoint routes anything below itself, so a
// sub-path is a 404 rather than a disclosure.
func isPublicProbePath(p string) bool {
	for _, base := range []string{"/healthz", "/readyz"} {
		if p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

// isLoopback reports whether addr (host:port) is a loopback source.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// sealTurnBoundary flushes pending main-scope edits and inserts a seam row of
// the given kind. No-op when the server has no VCS / repo configured. Non-fatal:
// errors are logged to stderr but never break the turn (B2-RB1 必修项 F: post-turn
// seam must fire on every path, so the helper itself cannot return an error that
// callers might skip).
//
// kind must be one of the vcs.SeamKind constants (passed as string to avoid the
// direct dep on vcs from this doc comment — callers pass vcs.SeamPreTurn etc.).
func (s *Server) sealTurnBoundary(sessionID string, turnSeq, historyLen int, kind, label string) {
	if s.vcs == nil || s.repoID == "" {
		return
	}
	if _, err := s.vcs.SealMainTurnSeam(s.repoID, sessionID, turnSeq, historyLen, vcs.SeamKind(kind), label); err != nil {
		// slog rather than stderr: this fires while a request is in flight, so
		// the redacting handler's trace/session/turn IDs are what make the
		// line attributable to a request -- a bare stderr write is an
		// orphaned sentence in a multi-connection server.
		slog.Warn("seam failed", "kind", kind, "label", label, "error", err)
	}
}

// shortHead returns the first 8 hex of the current main_head, or "" when VCS
// is unconfigured / repo has no head. Used to populate ServerFrame.CommitShort
// for DISPLAY (B2-RB1 必修项 E).
func (s *Server) shortHead() string {
	if s.vcs == nil || s.repoID == "" {
		return ""
	}
	head, err := s.vcs.RepoMainHead(s.repoID)
	if err != nil || head == "" {
		return ""
	}
	if len(head) > 8 {
		return head[:8]
	}
	return head
}

// fullHead returns the FULL current main_head commit id, or "" when VCS is
// unconfigured / repo has no head. Used to populate ServerFrame.Head for target
// BINDING — the restore_turn handler compares the client's ConfirmedHead against
// this FULL id (D6: short-hash comparison risks collision across long
// histories). Read-only; does NOT take the repo lock (DB reads are serialized
// by SetMaxOpenConns(1)).
func (s *Server) fullHead() string {
	if s.vcs == nil || s.repoID == "" {
		return ""
	}
	head, err := s.vcs.RepoMainHead(s.repoID)
	if err != nil {
		return ""
	}
	return head
}
