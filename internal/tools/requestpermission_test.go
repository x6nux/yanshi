package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
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
// permission callback that answers with the supplied decision.
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
func TestGrantedPermissionAdmitsTheLaterCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   string
		action guard.Action
	}{
		{"fs read", `{"dimension":"fs_read","tool":"fs_read","target":"../shared/schema.json","scope":"session","reason":"the schema lives outside the repo"}`,
			guard.Action{Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{"../shared/schema.json"}}}},
		{"fs write", `{"dimension":"fs_write","tool":"fs_write","target":"../out/report.md","scope":"session","reason":"the report goes to a sibling directory"}`,
			guard.Action{Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"../out/report.md"}}}},
		{"net host", `{"dimension":"net","tool":"web_fetch","target":"api.example.test","scope":"session","reason":"the API docs live there"}`,
			guard.Action{Tool: "web_fetch", NetHost: "api.example.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, asked := requestCtx(t, PermissionAllow)
			res := runRequest(t, ctx, tc.args)
			if !res.Granted {
				t.Fatalf("an approved request was not granted: %+v", res)
			}
			if *asked != 1 {
				t.Fatalf("the user was asked %d times, want 1", *asked)
			}
			// The later call must now find the rule. A callback that panics
			// proves the approval manager admitted it BEFORE any prompt.
			later := WithProfile(context.Background(), narrowProfile())
			later = WithApprovalManager(later, mgrFromCtx(t, ctx), "sess-1")
			later = WithPermissionCallback(later, func(PermissionRequest) PermissionDecision {
				t.Fatal("the recorded grant did not admit the call it was granted for")
				return PermissionDeny
			})
			if err := Authorize(later, tc.action, ""); err != nil {
				t.Fatalf("the granted call was still refused: %v", err)
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
	const args = `{"dimension":"fs_write","tool":"fs_write","target":"/etc/hosts","scope":"session","reason":"please"}`

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
		action := permissionActionFor(dim, "probe_tool", "target", "/work")
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
	action := guard.Action{Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{"../x.json"}}}

	for _, tc := range []struct {
		scope       string
		secondCalls int
	}{
		{"once", 1},    // consumed: the second call asks again
		{"session", 0}, // still held
	} {
		t.Run(tc.scope, func(t *testing.T) {
			ctx, mgr, _ := requestCtx(t, PermissionAllow)
			res := runRequest(t, ctx,
				`{"dimension":"fs_read","tool":"fs_read","target":"../x.json","scope":"`+tc.scope+`","reason":"r"}`)
			if !res.Granted {
				t.Fatalf("not granted: %+v", res)
			}
			second := 0
			later := WithProfile(context.Background(), narrowProfile())
			later = WithApprovalManager(later, mgr, "sess-1")
			later = WithPermissionCallback(later, func(PermissionRequest) PermissionDecision {
				second++
				return PermissionAllow
			})
			if err := Authorize(later, action, ""); err != nil {
				t.Fatalf("first call after the grant was refused: %v", err)
			}
			if second != 0 {
				t.Fatalf("the grant did not admit the first call (%d prompts)", second)
			}
			if err := Authorize(later, action, ""); err != nil {
				t.Fatalf("second call errored: %v", err)
			}
			if second != tc.secondCalls {
				t.Fatalf("scope %q: second call prompted %d times, want %d",
					tc.scope, second, tc.secondCalls)
			}
		})
	}
}
