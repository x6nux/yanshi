package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// hatchCtx 是逃生门工具的最小上下文：guard 的 Authorize fail-closed 要求一个
// profile，测试里给一个允许这两个名字的。
func hatchCtx() context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"tools_list", "tools_load"}},
	})
}

// TestWFS11RetrieverRanksByQuery pins the retrieval clause: a query about one
// capability surfaces the tool whose description carries it, and a different
// query surfaces a different tool — the selection MOVES with the query, which
// is the property "BM25 or equivalent" is asking for.
func TestWFS11RetrieverRanksByQuery(t *testing.T) {
	r := NewToolRetriever([]ToolMeta{
		{Name: "kube_scale", Desc: "Scale kubernetes deployments and manage cluster workloads"},
		{Name: "fs_edit", Desc: "Edit a file on disk by replacing text"},
		{Name: "db_vacuum", Desc: "Reclaim space in the sqlite database"},
	})

	top := func(query string) string {
		got := r.Select(query, 1)
		if len(got) != 1 {
			t.Fatalf("Select(%q) = %v, want exactly 1", query, got)
		}
		return got[0]
	}
	if got := top("scale the kubernetes cluster workloads"); got != "kube_scale" {
		t.Errorf("cluster query picked %q", got)
	}
	if got := top("edit the file contents"); got != "fs_edit" {
		t.Errorf("file query picked %q", got)
	}
	if got := top("compact the sqlite database"); got != "db_vacuum" {
		t.Errorf("db query picked %q", got)
	}

	// k cap: a large corpus returns at most k names.
	if got := r.Select("the database or the file or the cluster", 2); len(got) != 2 {
		t.Errorf("Select k=2 returned %d names", len(got))
	}

	// Empty query selects nothing — honest degradation, the view then is
	// always + loaded only.
	if got := r.Select("", 5); len(got) != 0 {
		t.Errorf("empty query must select nothing, got %v", got)
	}
}

// TestWFS11DiscoveryListAndLoad drives the two escape-hatch tools through
// their real InvokableRun path: tools_list renders the FULL corpus (including
// itself, hidden or not), marks loaded entries; tools_load validates against
// the corpus, rejects unknown names with the exact-name hint, and loads the
// rest into the turn state.
func TestWFS11DiscoveryListAndLoad(t *testing.T) {
	disc := NewToolDiscoveryTools([]ToolMeta{
		{Name: "fs_edit", Desc: "Edit a file on disk"},
		{Name: "web_search", Desc: "Search the web"},
	})
	ctx := WithToolLoadState(hatchCtx())

	// tools_load: unknown names are refused, not half-loaded.
	out, err := disc.Load.InvokableRun(ctx, `{"names":["fs_edit","not_a_tool"]}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(out, "not_a_tool") || !strings.Contains(out, "tools_list") {
		t.Fatalf("unknown name must be refused with the hatch hint, got %q", out)
	}
	if state, _ := ToolLoadStateFromContext(ctx); state.Has("fs_edit") {
		t.Fatal("a batch containing an unknown name must load nothing")
	}

	// tools_load: valid names land in the state and the result describes them.
	out, err = disc.Load.InvokableRun(ctx, `{"names":["fs_edit"]}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(out, "fs_edit") {
		t.Fatalf("load result must name the loaded tool, got %q", out)
	}
	state, _ := ToolLoadStateFromContext(ctx)
	if !state.Has("fs_edit") {
		t.Fatal("tools_load must mutate the turn's load state")
	}

	// tools_list: the full corpus renders, loaded entries marked, including
	// the hatch itself.
	out, err = disc.List.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"fs_edit [loaded]", "web_search --", "tools_list", "tools_load"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tools_list output missing %q:\n%s", want, out)
		}
	}

	// Idempotent load: a second load of the same name says so, not "loaded".
	out, err = disc.Load.InvokableRun(ctx, `{"names":["fs_edit"]}`)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.Contains(out, "already loaded") {
		t.Fatalf("second load must be reported as a no-op, got %q", out)
	}

	// Empty names is a usage error, not a silent success.
	out, _ = disc.Load.InvokableRun(ctx, `{"names":[]}`)
	if !strings.Contains(out, "tools_list") {
		t.Fatalf("empty names must point at tools_list, got %q", out)
	}
}

// TestWFS11LoadWithoutBoundStateReportsInactive pins the honesty clause: a
// tools_load that runs where no turn state was bound (feature wiring bug,
// embedder that skipped withTurnContext) says "not active" instead of
// pretending the load happened.
func TestWFS11LoadWithoutBoundStateReportsInactive(t *testing.T) {
	disc := NewToolDiscoveryTools([]ToolMeta{{Name: "fs_edit", Desc: "edit"}})
	out, _ := disc.Load.InvokableRun(hatchCtx(), `{"names":["fs_edit"]}`)
	if !strings.Contains(out, "not active") {
		t.Fatalf("unbound state must be reported honestly, got %q", out)
	}
}

// TestWFS11StateIsConcurrencySafe runs parallel Load/Names against one state
// under -race; the mutex is the mechanism, this is the check it is real.
func TestWFS11StateIsConcurrencySafe(t *testing.T) {
	state := WithToolLoadState(context.Background())
	st, _ := ToolLoadStateFromContext(state)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				st.Load([]string{"tool_a", "tool_b"})
				_ = st.Names()
				_ = st.Has("tool_a")
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := st.Names(); len(got) != 2 {
		t.Fatalf("Names = %v, want exactly the two tools", got)
	}
}
