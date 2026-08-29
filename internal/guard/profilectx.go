package guard

import "context"

// profileCtxKey is the context key PermissionProfile is bound under. It lives
// here (not in internal/tools, where the profile-gated call sites are) so a
// LEAF caller can bind a profile without importing tools.
//
// # Why this moved out of internal/tools (W-C-12)
//
// internal/tools.WithProfile/ProfileFromContext used to be the only way to
// bind the profile internal/tools.Authorize reads back — an unexported
// context-key type that only the tools package could set or read. That was
// fine as long as every caller needing a profile could afford to import
// tools. W-C-12's auth.command runtime (internal/llm/eino/cmdauth.go) cannot:
// internal/tools' OWN test files (agent_dag_cov_test.go, package tools) and
// internal/agent/rlm's (rlm_cov_test.go, package rlm — tools imports rlm in
// production) both import internal/llm/eino for its FakeModel, and eino
// importing tools closes that loop the moment `go vet`/`go test` compiles the
// test binary (go build ./... stays clean; only test builds see it).
//
// Moving the key/getter/setter down to guard — a leaf package eino, tools and
// rlm can all import without creating a cycle — fixes this the same way the
// SecureProcessFactory re-exports below tools.Authorize already fix the
// analogous problem for secproc: tools.WithProfile/ProfileFromContext become
// thin re-exports so every existing call site (bootstrap, orchestrator,
// api/http, tests — 14 files outside internal/tools at the time of writing)
// keeps compiling unchanged, while a caller that must not import tools calls
// guard.WithProfile directly.
type profileCtxKey struct{}

// WithProfile binds a PermissionProfile to ctx (the acting agent's profile).
// internal/tools.Authorize reads it back via ProfileFromContext and fails
// closed (DenyErr) when none is bound.
func WithProfile(ctx context.Context, p PermissionProfile) context.Context {
	return context.WithValue(ctx, profileCtxKey{}, p)
}

// ProfileFromContext returns the profile bound by WithProfile, if any.
func ProfileFromContext(ctx context.Context) (PermissionProfile, bool) {
	p, ok := ctx.Value(profileCtxKey{}).(PermissionProfile)
	return p, ok
}
