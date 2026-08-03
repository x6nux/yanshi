// Package archtest — removal assertions.
//
// Some audit items close by deleting code rather than finishing it. A ledger
// entry with verdict "removed" points here: the evidence for "we removed it"
// has to be a test that fails if it comes back.
package archtest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestVSCodeExtensionRemoved asserts the VS Code extension and its CI helper
// are gone.
//
// ledger: D2/O12#1 ide/vscode/ 与 scripts/check-d2.sh 不存在
//
// Audit item D2/O12 closed by removal (spec §3.2 ④): the extension was never
// finished (runWithRecovery was never imported by extension.ts, and the
// README advertised reconnect behaviour that did not exist), and with TUI and
// the Web IDE both first-class, a third front end has an overlapping audience
// and does not earn its maintenance cost.
//
// This is the evidence backing that ledger entry — if either path reappears,
// it must come with a decision to reverse §3.2 ④.
func TestVSCodeExtensionRemoved(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range []string{
		"ide/vscode",
		"scripts/check-d2.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s still exists — audit item D2/O12 closed by removal "+
				"(spec §3.2 ④). If this is a deliberate reversal, update the spec "+
				"and the ledger entry first.", rel)
		}
	}
}

// d2Tombstone is the marker a document must carry if it still mentions the
// removed VS Code extension. One fixed literal, so the check is exact and the
// reader always lands on the same sentence.
const d2Tombstone = "D2/O12 已作废"

// d2Mentions are the strings that name the deleted deliverable. "ide-vscode"
// is deliberately absent: it is a commit scope in docs/commit-convention.md,
// not a claim that the extension ships.
var d2Mentions = []string{"ide/vscode", "scripts/check-d2", "VS Code 扩展"}

// d2HistoricalDocs are the documents allowed to keep mentioning the extension
// because they ARE the record of it — audits, plans and specs are dated
// artefacts, and rewriting history to hide a cancelled deliverable is its own
// kind of dishonesty. Each one must still carry d2Tombstone so a reader (or an
// agent mining plans for work) cannot mistake the mention for a live promise.
//
// Same rules as lineExceptions and docExceptionSymbols: this map only shrinks.
// An entry whose file no longer mentions the extension is a dead exemption and
// fails.
var d2HistoricalDocs = map[string]string{
	"docs/feature-status-audit.md":                                         "2026-07-31 审计快照",
	"docs/superpowers/notes/2026-08-03-w9-w10-verification.md":             "W9/W10 核查记录",
	"docs/superpowers/plans/2026-07-21-d2-sdk-ide.md":                      "D2 原始计划，交付物已作废",
	"docs/superpowers/plans/2026-08-03-s0-w0-governance.md":                "执行删除的那份计划",
	"docs/superpowers/plans/2026-08-03-s0-w10-release.md":                  "W10 计划，文档扫描条目",
	"docs/superpowers/specs/2026-07-22-h2-docs-examples-contrib-design.md": "H2 spec，列过 ide/vscode",
	"docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md":           "S0 spec §3.2 ④，移除决策本身",
	"sdk/schema/CONTRACT_HANDOFF.md":                                       "SDK 交接说明，已带移除声明",
}

// TestVSCodeExtensionNotAdvertisedInDocs is the second half of D2/O12: the
// extension is not merely deleted from disk, it is no longer described as
// something yanshi delivers.
//
// ledger: D2/O12#2 文档无对其作为交付物的描述
//
// The distinction a grep cannot make — "we will ship this" versus "we cancelled
// shipping this" — is resolved structurally instead of semantically: live docs
// must not mention it at all, and the dated audit/plan/spec documents that must
// keep their mentions are enumerated in d2HistoricalDocs and required to carry
// a tombstone. That is why this clause is testable at all; without the
// enumeration it would be a judgement call, and a judgement call is not
// acceptance criteria.
//
// docs/archive/ is excluded wholesale by directory: it is a declared archive of
// superseded roadmaps, not a description of the current product.
func TestVSCodeExtensionNotAdvertisedInDocs(t *testing.T) {
	root := moduleRoot(t)
	mentioned := map[string]bool{}
	var live, untombstoned []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(mustRel(t, root, path))
		if d.IsDir() {
			switch {
			case rel == ".git", rel == "docs/archive", rel == "third_party",
				strings.HasSuffix(rel, "/node_modules"), rel == "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		hit := false
		for _, m := range d2Mentions {
			if strings.Contains(body, m) {
				hit = true
				break
			}
		}
		if !hit {
			return nil
		}
		mentioned[rel] = true
		if _, historical := d2HistoricalDocs[rel]; !historical {
			live = append(live, rel)
			return nil
		}
		if !strings.Contains(body, d2Tombstone) {
			untombstoned = append(untombstoned, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range live {
		t.Errorf("%s still describes the VS Code extension as a deliverable — "+
			"audit item D2/O12 closed by removal (spec §3.2 ④). Delete the "+
			"description, or add the file to d2HistoricalDocs with a %q tombstone "+
			"if it is a dated record.", rel, d2Tombstone)
	}
	for _, rel := range untombstoned {
		t.Errorf("%s mentions the removed VS Code extension without the %q "+
			"tombstone — a reader mining this document for work would take it as a "+
			"live deliverable.", rel, d2Tombstone)
	}

	var dead []string
	for rel := range d2HistoricalDocs {
		if !mentioned[rel] {
			dead = append(dead, rel)
		}
	}
	sort.Strings(dead)
	for _, rel := range dead {
		t.Errorf("d2HistoricalDocs lists %s but it no longer mentions the VS Code "+
			"extension — delete the exemption (this map only shrinks).", rel)
	}
}
