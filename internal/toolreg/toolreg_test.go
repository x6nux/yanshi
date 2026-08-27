package toolreg

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewSet(t *testing.T) {
	cases := []struct {
		name   string
		input  []string
		want   []string
		length int
	}{
		{"nil", nil, nil, 0},
		{"plain", []string{"fs_read", "shell_run"}, []string{"fs_read", "shell_run"}, 2},
		{"sorted output", []string{"z", "a", "m"}, []string{"a", "m", "z"}, 3},
		{"dedup", []string{"a", "a", "a"}, []string{"a"}, 1},
		// A spec that forgot to fill its Tool field authorizes the empty name.
		// Admitting it would make "" registered and defeat the whole check.
		{"empty dropped", []string{"", "a"}, []string{"a"}, 1},
		{"blank dropped", []string{"   ", "\t", "a"}, []string{"a"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSet(tc.input)
			if s.Len() != tc.length {
				t.Fatalf("Len = %d, want %d", s.Len(), tc.length)
			}
			got := s.Names()
			if len(got) != len(tc.want) {
				t.Fatalf("Names = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("Names = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSetHas(t *testing.T) {
	s := NewSet([]string{"fs_read"})
	if !s.Has("fs_read") {
		t.Fatal("registered name must be present")
	}
	if s.Has("fs_reader") || s.Has("") || s.Has("FS_READ") {
		t.Fatal("matching must be exact")
	}
	var zero Set
	if zero.Has("anything") {
		t.Fatal("zero Set must report nothing as registered")
	}
	if zero.Len() != 0 || len(zero.Names()) != 0 {
		t.Fatal("zero Set must be empty")
	}
}

// TestUnboundContextAllows pins the deliberate asymmetry documented in the
// package doc: no set bound means nobody configured this process, and denying
// everything would turn a wiring omission into a total tool outage. This is a
// TIGHTENING layer, so "not configured" degrades to "no extra tightening".
func TestUnboundContextAllows(t *testing.T) {
	ctx := context.Background()
	if _, bound := FromContext(ctx); bound {
		t.Fatal("bare context must report no bound set")
	}
	if err := Check(ctx, "totally_made_up"); err != nil {
		t.Fatalf("unbound context must allow, got %v", err)
	}
	if err := Check(ctx, ""); err != nil {
		t.Fatalf("unbound context must allow the empty name too, got %v", err)
	}
}

// TestEmptySliceDoesNotBind: an empty registry is the shape of "not populated
// yet", not "no tool may run". Binding it would deny every call in the scope.
func TestEmptySliceDoesNotBind(t *testing.T) {
	for _, names := range [][]string{nil, {}, {""}, {"  ", "\n"}} {
		ctx := WithRegistered(context.Background(), names)
		if _, bound := FromContext(ctx); bound {
			t.Fatalf("names %q must not bind a set", names)
		}
		if err := Check(ctx, "x"); err != nil {
			t.Fatalf("names %q: Check must allow, got %v", names, err)
		}
	}
}

func TestCheckFailsClosedForUnregistered(t *testing.T) {
	ctx := WithRegistered(context.Background(), []string{"fs_read", "shell_run"})
	set, bound := FromContext(ctx)
	if !bound || set.Len() != 2 {
		t.Fatalf("bound = %v, len = %d", bound, set.Len())
	}

	for _, ok := range []string{"fs_read", "shell_run"} {
		if err := Check(ctx, ok); err != nil {
			t.Fatalf("registered %q must pass, got %v", ok, err)
		}
	}

	cases := []struct {
		name string
		tool string
	}{
		{"phantom", "fs_mkdir"},
		{"hallucinated", "search_the_web"},
		{"empty name", ""},
		{"prefix of a real one", "fs_"},
		{"case variant", "FS_READ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(ctx, tc.tool)
			if err == nil {
				t.Fatalf("unregistered %q must be refused", tc.tool)
			}
			var ue *UnregisteredError
			if !errors.As(err, &ue) {
				t.Fatalf("want *UnregisteredError, got %T", err)
			}
			if ue.Tool != tc.tool {
				t.Fatalf("UnregisteredError.Tool = %q, want %q", ue.Tool, tc.tool)
			}
		})
	}
}

func TestUnregisteredErrorMessage(t *testing.T) {
	named := (&UnregisteredError{Tool: "fs_mkdir"}).Error()
	if !strings.Contains(named, "fs_mkdir") || !strings.Contains(named, "unregistered tool") {
		t.Fatalf("Error = %q", named)
	}
	// The empty-name case gets its own wording: "unregistered tool:  is not a
	// registered tool" would read as a rendering bug rather than as the real
	// finding, which is that a caller authorized with no name at all.
	empty := (&UnregisteredError{}).Error()
	if !strings.Contains(empty, "empty tool name") {
		t.Fatalf("Error = %q, must call out the empty name", empty)
	}
}

// TestNarrowerScopeWins: sub-agents run with a filtered tool subset. Rebinding
// must NARROW, not merge with, the parent's set — otherwise a sub-agent
// authorizes against the parent's wider surface.
func TestNarrowerScopeWins(t *testing.T) {
	parent := WithRegistered(context.Background(), []string{"fs_read", "shell_run", "agent_spawn"})
	child := WithRegistered(parent, []string{"fs_read"})

	if err := Check(child, "shell_run"); err == nil {
		t.Fatal("child scope must not inherit the parent's wider set")
	}
	if err := Check(child, "fs_read"); err != nil {
		t.Fatalf("child's own name must pass, got %v", err)
	}
	// The parent context is untouched.
	if err := Check(parent, "shell_run"); err != nil {
		t.Fatalf("parent scope must be unaffected, got %v", err)
	}
}
