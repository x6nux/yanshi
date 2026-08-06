package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/docgen"
)

// rewriteBlockTmp returns a temp file path backed by content. Cleanup is
// registered on tb.
func rewriteBlockTmp(tb testing.TB, content string) string {
	tb.Helper()
	dir := tb.TempDir()
	p := filepath.Join(dir, "target.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tb.Fatalf("write tmp: %v", err)
	}
	return p
}

func TestRewriteBlockReplacesExisting(t *testing.T) {
	// Case 1: file already contains the target id block → replace inner
	// content, surrounding lines preserved verbatim.
	src := "header line\n\n<!-- BEGIN GENERATED: foo -->\nold\n<!-- END GENERATED: foo -->\n\nfooter\n"
	p := rewriteBlockTmp(t, src)
	if err := docgen.RewriteBlock(p, "foo", "NEW"); err != nil {
		t.Fatalf("RewriteBlock: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	g := string(got)
	if !strings.Contains(g, "header line") || !strings.Contains(g, "footer") {
		t.Errorf("surrounding prose not preserved: %q", g)
	}
	if !strings.Contains(g, "NEW") || strings.Contains(g, "\nold\n") {
		t.Errorf("inner content not replaced: %q", g)
	}
	if !strings.Contains(g, "<!-- BEGIN GENERATED: foo -->") || !strings.Contains(g, "<!-- END GENERATED: foo -->") {
		t.Errorf("markers missing: %q", g)
	}
}

func TestRewriteBlockAppendsWhenAbsent(t *testing.T) {
	// Case 2: file lacks the id block (including the empty-file case) →
	// append the block to the end.
	t.Run("nonempty", func(t *testing.T) {
		p := rewriteBlockTmp(t, "just prose\n")
		if err := docgen.RewriteBlock(p, "bar", "content"); err != nil {
			t.Fatalf("RewriteBlock: %v", err)
		}
		g, _ := os.ReadFile(p)
		got := string(g)
		if !strings.Contains(got, "just prose") {
			t.Errorf("prose lost: %q", got)
		}
		if !strings.Contains(got, "<!-- BEGIN GENERATED: bar -->\ncontent\n<!-- END GENERATED: bar -->") {
			t.Errorf("block not appended: %q", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		p := rewriteBlockTmp(t, "")
		if err := docgen.RewriteBlock(p, "baz", "x"); err != nil {
			t.Fatalf("RewriteBlock: %v", err)
		}
		g, _ := os.ReadFile(p)
		got := string(g)
		if !strings.Contains(got, "<!-- BEGIN GENERATED: baz -->\nx\n<!-- END GENERATED: baz -->") {
			t.Errorf("block not appended to empty file: %q", got)
		}
	})
}

func TestRewriteBlockIdempotent(t *testing.T) {
	// Case 3: calling twice with the same (path, id, content) yields byte-
	// identical output.
	p := rewriteBlockTmp(t, "")
	if err := docgen.RewriteBlock(p, "idem", "same"); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if err := docgen.RewriteBlock(p, "idem", "same"); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestRenderMarkdownContainsSchemaFull(t *testing.T) {
	out := RenderMarkdown(v1.SchemaBytes())
	if !strings.Contains(out, "```json") {
		t.Errorf("missing json code fence")
	}
	if !strings.Contains(out, "<!-- BEGIN GENERATED: api-schema-full -->") {
		t.Errorf("missing api-schema-full begin marker")
	}
	if !strings.Contains(out, "<!-- END GENERATED: api-schema-full -->") {
		t.Errorf("missing api-schema-full end marker")
	}
	// The pretty-printed schema must echo the $id so we know it came from
	// SchemaBytes (not a stale literal). This is the SDK document's identity:
	// the runtime used to serve a separate hand-built literal under
	// .../agent-api-v1.json, and asserting THAT id would now pass only if the
	// two documents had drifted apart again.
	if !strings.Contains(out, "https://yanshi.dev/schema/agent-api/v1/agent-api.schema.json") {
		t.Errorf("schema $id missing from rendered output")
	}
}

// ledger: H2/APIREF1#1 v1 API 有参考
func TestRenderMarkdownContainsDefsBlocks(t *testing.T) {
	out := RenderMarkdown(v1.SchemaBytes())
	// Each $defs entry produces its own generated block table.
	for _, name := range []string{"Thread", "Turn", "Item"} {
		if !strings.Contains(out, "<!-- BEGIN GENERATED: api-defs:"+name+" -->") {
			t.Errorf("missing api-defs:%s begin marker", name)
		}
		if !strings.Contains(out, "<!-- END GENERATED: api-defs:"+name+" -->") {
			t.Errorf("missing api-defs:%s end marker", name)
		}
	}
	// A required property of Thread must show up as a table row.
	if !strings.Contains(out, "| createdAt") {
		t.Errorf("Thread.createdAt row missing")
	}
}

// TestRenderMarkdownParamsIncludeImages pins that TurnStartParams.images
// survives generation. It was cited for「v1 API 有参考」and does not show it:
// the generator producing a block in memory says nothing about whether the
// committed reference page has one.
func TestRenderMarkdownParamsIncludeImages(t *testing.T) {
	// G (multimodal) landed: TurnStartParams must surface the images field so
	// resources.md stays in sync with types.go.
	out := RenderMarkdown(v1.SchemaBytes())
	if !strings.Contains(out, "<!-- BEGIN GENERATED: api-defs:TurnStartParams -->") {
		t.Errorf("missing TurnStartParams block")
	}
	if !strings.Contains(out, "| images") {
		t.Errorf("TurnStartParams.images row missing (G multimodal field)")
	}
}
