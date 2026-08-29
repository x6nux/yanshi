package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// narrowProfile allows the tool surface but no filesystem paths and no network,
// so every request below is one the profile would refuse.
func narrowProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"src/**"}, Write: []string{"src/**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	}
}

// requestCtx builds a context with a profile, an approval manager and a
// permission callback that answers with the supplied decision. The work root is
// a fresh temp dir; requestRoot reads it back for tests that need to spell an
// absolute target.
func requestCtx(t *testing.T, answer PermissionDecision) (context.Context, *approval.Manager, *int) {
	t.Helper()
	mgr, err := approval.New(nil, "proc-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	ctx := WithProfile(context.Background(), narrowProfile())
	ctx = WithWorkRoot(ctx, t.TempDir())
	ctx = WithApprovalManager(ctx, mgr, "sess-1")
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		asked++
		return answer
	})
	return ctx, mgr, &asked
}

// requestRoot returns the work root requestCtx bound.
func requestRoot(t *testing.T, ctx context.Context) string {
	t.Helper()
	root := WorkRootFromContext(ctx)
	if root == "" {
		t.Fatal("no work root bound")
	}
	return root
}

func runRequest(t *testing.T, ctx context.Context, args string) permissionRequestResult {
	t.Helper()
	out, err := runRequestPermission(ctx, args)
	if err != nil {
		t.Fatalf("request_permission errored instead of answering: %v", err)
	}
	var res permissionRequestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	return res
}

// TestGrantedPermissionAdmitsTheLaterCall is the anti-"零读者" test named in the
// file header, and it is the one that fails if this tool becomes a dialog that
// grants nothing.
//
// approval.Manager matches scopes with reflect.DeepEqual, so a grant whose
// scope does not reproduce the later action's byte for byte is silently inert:
// the operator approves, the model is told "granted", and the next call prompts
// anyway. Nothing errors. The only way to know is to make the later call.
//
// "The later call" means the REAL TOOL, and that is the whole difference from
// the version this replaces. That one built the guard.Action by hand and passed
// it to Authorize, a shape production never produces: fs tools resolve their
// paths through FSTools.abs first and hand Authorize the resolved absolute
// path. The hand-built Action carried the model's raw string, so the test
// asserted the two ends agree with each other and never that either agrees with
// the tool in between. Both spellings a model may plausibly use for an in-root
// target are exercised here, and each is checked against BOTH spellings at call
// time, because the grant has to survive the model changing its mind about how
// to write the path.
func TestGrantedPermissionAdmitsTheLaterCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		dim  string
		tool string
	}{
		{"fs read", "fs_read", "fs_read"},
		{"fs write", "fs_write", "fs_write"},
	} {
		for _, granted := range []string{"relative", "absolute"} {
			for _, called := range []string{"relative", "absolute"} {
				t.Run(tc.name+"/granted "+granted+"/called "+called, func(t *testing.T) {
					ctx, _, asked := requestCtx(t, PermissionAllow)
					root := requestRoot(t, ctx)
					const rel = "outside/x.json"
					abs := filepath.Join(root, rel)
					if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(abs, []byte("{}"), 0o644); err != nil {
						t.Fatal(err)
					}
					spell := func(which string) string {
						if which == "absolute" {
							return strings.ReplaceAll(abs, `\`, `\\`)
						}
						return rel
					}

					res := runRequest(t, ctx, `{"dimension":"`+tc.dim+`","tool":"`+tc.tool+
						`","target":"`+spell(granted)+`","scope":"session","reason":"the fixture lives there"}`)
					if !res.Granted {
						t.Fatalf("an approved request was not granted: %+v", res)
					}
					if *asked != 1 {
						t.Fatalf("the user was asked %d times, want 1", *asked)
					}

					// The real tool, on a context whose callback fails the test:
					// reaching it means the recorded grant did not match.
					later := WithProfile(context.Background(), narrowProfile())
					later = WithWorkRoot(later, root)
					later = WithApprovalManager(later, mgrFromCtx(t, ctx), "sess-1")
					later = WithPermissionCallback(later, func(PermissionRequest) PermissionDecision {
						t.Error("the recorded grant did not admit the call it was granted for")
						return PermissionDeny
					})
					fs := NewFSTools(root)
					var err error
					if tc.dim == "fs_read" {
						_, err = fs.runRead(later, `{"path":"`+spell(called)+`"}`)
					} else {
						_, err = fs.runWrite(later, `{"path":"`+spell(called)+`","content":"x"}`)
					}
					if err != nil {
						t.Fatalf("the granted call was still refused: %v", err)
					}
				})
			}
		}
	}
}

// TestRequestPermissionRefusesTargetsNoApprovalCanAdmit is the other half of
// Blocking-1, and the one that keeps the dialog honest.
//
// A path outside the project root is not a permission question. FSTools.abs
// runs before Authorize in every fs tool, so such a call is refused before the
// guard ever sees it and no approval rule — however the operator answers —
// could admit it. Reporting granted=true for one is a false statement to BOTH
// parties: the operator signs something that grants nothing, and the model is
// told to go ahead and hits the same wall.
//
// asked==0 is the load-bearing assertion. It says the refusal happened without
// putting a dialog in front of anybody, which is the same rule the unregistered
// tool and structural floor outcomes follow.
func TestRequestPermissionRefusesTargetsNoApprovalCanAdmit(t *testing.T) {
	for _, target := range []string{
		"../shared/schema.json", // traversal
		"/etc/hosts",            // absolute, outside the root
		"../../etc/passwd",      // traversal, deeper
	} {
		t.Run(target, func(t *testing.T) {
			ctx, mgr, asked := requestCtx(t, PermissionAllow)
			res := runRequest(t, ctx,
				`{"dimension":"fs_read","tool":"fs_read","target":"`+target+`","scope":"session","reason":"r"}`)
			if res.Granted {
				t.Fatalf("a target the fs jail refuses was reported as granted: %+v", res)
			}
			if *asked != 0 {
				t.Fatalf("the operator was asked %d times about a grant that could not take effect", *asked)
			}
			if len(mgr.List("sess-1", time.Now())) != 0 {
				t.Fatal("a rule was recorded for a call that can never reach the guard")
			}
			if !strings.Contains(res.Detail, "project root") {
				t.Fatalf("the refusal does not tell the model why: %q", res.Detail)
			}
			// And the real tool agrees: this is refused, not prompted.
			fs := NewFSTools(requestRoot(t, ctx))
			if _, err := fs.runRead(ctx, `{"path":"`+target+`"}`); err == nil {
				t.Fatal("the fs jail admitted a path this test assumes it refuses")
			}
		})
	}
}

// mgrFromCtx reads back the approval manager requestCtx bound, so the "later
// call" runs against the SAME manager the grant was written to.
func mgrFromCtx(t *testing.T, ctx context.Context) *approval.Manager {
	t.Helper()
	ac, ok := approvalFromContext(ctx)
	if !ok {
		t.Fatal("no approval manager bound")
	}
	return ac.Manager
}

// TestRequestPermissionNeverGrantsWithoutAnExplicitYes is the property that
// makes this tool safe to hand a model.
//
// It is the same structural guarantee askEscalation has next door: the ONLY
// success return is behind RequireApproval returning nil, so a denial, a
// timeout (which the WS callback delivers as a denial) and a transport with no
// callback at all fall to the same refusal.
func TestRequestPermissionNeverGrantsWithoutAnExplicitYes(t *testing.T) {
	// An IN-ROOT target the narrow profile refuses. An out-of-root one would
	// never reach RequireApproval at all (see
	// TestRequestPermissionRefusesTargetsNoApprovalCanAdmit), so it would pass
	// this test while testing nothing.
	const args = `{"dimension":"fs_write","tool":"fs_write","target":"outside/report.md","scope":"session","reason":"please"}`

	t.Run("user says no", func(t *testing.T) {
		ctx, mgr, asked := requestCtx(t, PermissionDeny)
		res := runRequest(t, ctx, args)
		if res.Granted {
			t.Fatal("a refused request reported granted")
		}
		if *asked != 1 {
			t.Fatalf("the user was asked %d times, want 1", *asked)
		}
		if len(mgr.List("sess-1", time.Now())) != 0 {
			t.Fatal("a refused request still recorded an approval rule")
		}
	})

	t.Run("no interactive channel", func(t *testing.T) {
		mgr, err := approval.New(nil, "proc-test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithProfile(context.Background(), narrowProfile())
		ctx = WithWorkRoot(ctx, t.TempDir())
		ctx = WithApprovalManager(ctx, mgr, "sess-1")
		res := runRequest(t, ctx, args)
		if res.Granted {
			t.Fatal("a request on a transport with nobody to ask reported granted")
		}
		if len(mgr.List("sess-1", time.Now())) != 0 {
			t.Fatal("a request nobody answered recorded an approval rule")
		}
	})
}

// TestAlreadyPermittedShortCircuitsWithoutAsking covers the outcome that keeps
// this tool from becoming a source of dialog fatigue.
//
// A model that requests something the profile already allows must get "you
// already have this" rather than a prompt. Putting a dialog in front of the
// operator for a capability they granted is how operators learn to click
// through dialogs, which costs more than the prompt saves.
//
// It also asserts NO rule is recorded: a redundant approval rule for a call
// that never needed one is a grant nobody reviewed sitting in the archive.
func TestAlreadyPermittedShortCircuitsWithoutAsking(t *testing.T) {
	ctx, mgr, asked := requestCtx(t, PermissionAllow)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	res := runRequest(t, ctx,
		`{"dimension":"fs_read","tool":"fs_read","target":"x.go","scope":"once","reason":"r"}`)
	if !res.Granted || !strings.Contains(res.Detail, "already permitted") {
		t.Fatalf("an already-permitted request should short-circuit as granted: %+v", res)
	}
	if *asked != 0 {
		t.Fatalf("an already-permitted request asked the user %d times", *asked)
	}
	if len(mgr.List("sess-1", time.Now())) != 0 {
		t.Fatal("an already-permitted request recorded a redundant rule")
	}
}

// TestStructuralFloorIsUnreachableFromTheSupportedDimensions is the honest
// record for the branch above it in runRequestPermission.
//
// That branch refuses a structural HardDeny without asking, and no input this
// tool accepts can produce one: fs and net actions carry no Shell, so
// checkDestructive and checkShell both return Allow, and every other dimension
// tops out at an OVERRIDABLE deny. So the branch is defensive — it exists for
// the dimension somebody adds next, on the same terms as guard's own
// unknown-verdict default.
//
// Asserting the unreachability is the point. Without it the branch would be
// indistinguishable from one that is live and simply untested, and the next
// reader would take its presence as evidence the case occurs.
func TestStructuralFloorIsUnreachableFromTheSupportedDimensions(t *testing.T) {
	broken := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "bogus-policy"}, // structural HardDeny for any shell action
	}
	for _, dim := range []PermissionRequestDimension{DimensionFSRead, DimensionFSWrite, DimensionNet} {
		action, err := permissionActionFor(dim, "probe_tool", "target", t.TempDir())
		if err != nil {
			t.Fatalf("dimension %q: %v", dim, err)
		}
		d := guard.New().Check(broken, action)
		if d.Verdict == guard.HardDeny && !d.Overridable {
			t.Fatalf("dimension %q reached the structural floor; the branch is live and "+
				"needs a behavioural test, not this one: %+v", dim, d)
		}
	}
}

// TestRequestPermissionRejectsMalformedRequests covers the arguments a model
// gets wrong, and one it must never be allowed to get right.
//
// The dimension has no default: "net 与 fs 分维" is the point of the field, and
// a silent fallback would grant a dimension nobody asked for. The scope DOES
// default, and it defaults to the narrow one.
func TestRequestPermissionRejectsMalformedRequests(t *testing.T) {
	ctx, _, asked := requestCtx(t, PermissionAllow)
	for _, args := range []string{
		`{"tool":"fs_read","target":"x","reason":"r"}`,                                         // no dimension
		`{"dimension":"shell","tool":"shell_run","target":"x","reason":"r"}`,                   // unknown dimension
		`{"dimension":"fs_read","tool":"fs_read","target":"x","scope":"forever","reason":"r"}`, // unknown scope
		`{"dimension":"fs_read","tool":"fs_read","target":"x"}`,                                // no reason
		`{"dimension":"fs_read","tool":"","target":"x","reason":"r"}`,                          // no tool
	} {
		if _, err := runRequestPermission(ctx, args); err == nil {
			t.Fatalf("malformed request accepted: %s", args)
		}
	}
	if *asked != 0 {
		t.Fatalf("a malformed request reached the user %d times", *asked)
	}
	// The default scope is the narrow one.
	if got, err := normalizeRequestScope(""); err != nil || got != ScopeOnce {
		t.Fatalf("omitted scope = (%q,%v), want once", got, err)
	}
	if got, err := normalizeRequestScope("turn"); err != nil || got != ScopeOnce {
		t.Fatalf(`scope "turn" = (%q,%v), want once`, got, err)
	}
}

// TestRequestedScopeLifetimeMatchesTheAskedForScope pins once vs session.
//
// A "once" grant that behaved like a session one would be the widening this
// whole file has to avoid, and the difference is invisible from the first call
// — both admit it. It only shows on the second.
func TestRequestedScopeLifetimeMatchesTheAskedForScope(t *testing.T) {
	for _, tc := range []struct {
		scope       string
		secondCalls int
	}{
		{"once", 1},    // consumed: the second call asks again
		{"session", 0}, // still held
	} {
		t.Run(tc.scope, func(t *testing.T) {
			ctx, mgr, _ := requestCtx(t, PermissionAllow)
			root := requestRoot(t, ctx)
			if err := os.WriteFile(filepath.Join(root, "x.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			res := runRequest(t, ctx,
				`{"dimension":"fs_read","tool":"fs_read","target":"x.json","scope":"`+tc.scope+`","reason":"r"}`)
			if !res.Granted {
				t.Fatalf("not granted: %+v", res)
			}
			second := 0
			later := WithProfile(context.Background(), narrowProfile())
			later = WithWorkRoot(later, root)
			later = WithApprovalManager(later, mgr, "sess-1")
			later = WithPermissionCallback(later, func(PermissionRequest) PermissionDecision {
				second++
				return PermissionAllow
			})
			fs := NewFSTools(root)
			if _, err := fs.runRead(later, `{"path":"x.json"}`); err != nil {
				t.Fatalf("first call after the grant was refused: %v", err)
			}
			if second != 0 {
				t.Fatalf("the grant did not admit the first call (%d prompts)", second)
			}
			if _, err := fs.runRead(later, `{"path":"x.json"}`); err != nil {
				t.Fatalf("second call errored: %v", err)
			}
			if second != tc.secondCalls {
				t.Fatalf("scope %q: second call prompted %d times, want %d",
					tc.scope, second, tc.secondCalls)
			}
		})
	}
}

// TestRequestPermissionRefusesAnUnregisteredTool covers the fourth terminal
// outcome, the only one that had no test at all (review b4 Minor-1: emptying
// the toolreg.Check call left the whole package green).
//
// It is the S8 refusal: a dialog offering Allow for a name nothing can execute
// trains the operator to click through, and toolreg is the only thing that
// knows which names this execution scope actually holds. The registered set
// must be bound explicitly — toolreg.Check is a documented no-op when it is
// not, so a version of this test without WithRegistered would pass on the
// mutation it exists to catch.
func TestRequestPermissionRefusesAnUnregisteredTool(t *testing.T) {
	ctx, mgr, asked := requestCtx(t, PermissionAllow)
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read", "fs_write"})
	res := runRequest(t, ctx,
		`{"dimension":"fs_read","tool":"no_such_tool","target":"x.json","scope":"once","reason":"r"}`)
	if res.Granted {
		t.Fatalf("a request naming an unregistered tool was granted: %+v", res)
	}
	if *asked != 0 {
		t.Fatalf("the operator was asked %d times about a tool nothing can execute", *asked)
	}
	if len(mgr.List("sess-1", time.Now())) != 0 {
		t.Fatal("a rule was recorded for a tool that does not exist")
	}
	if !strings.Contains(res.Detail, "no such tool") {
		t.Fatalf("the refusal does not name the reason: %q", res.Detail)
	}
	// Same request, now registered: it reaches the operator. Without this the
	// test would also pass if request_permission refused everything.
	ctx2, _, asked2 := requestCtx(t, PermissionAllow)
	ctx2 = toolreg.WithRegistered(ctx2, []string{"fs_read"})
	if res := runRequest(t, ctx2,
		`{"dimension":"fs_read","tool":"fs_read","target":"x.json","scope":"once","reason":"r"}`); !res.Granted {
		t.Fatalf("a registered tool was refused as unregistered: %+v", res)
	}
	if *asked2 != 1 {
		t.Fatalf("a registered tool asked %d times, want 1", *asked2)
	}
}
