// Package archtest — removal assertions.
//
// Some audit items close by deleting code rather than finishing it. A ledger
// entry with verdict "removed" points here: the evidence for "we removed it"
// has to be a test that fails if it comes back.
package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestVSCodeExtensionRemoved asserts the VS Code extension and its CI helper
// are gone.
//
// Audit item D2/O12 closed by removal (spec §3.2 ④): the extension was never
// finished (runWithRecovery was never imported by extension.ts, and the
// README advertised reconnect behaviour that did not exist), and with TUI and
// the Web IDE both first-class, a third front end has an overlapping audience
// and does not earn its maintenance cost.
//
// This is the evidence backing that ledger entry — if either path reappears,
// it must come with a decision to reverse §3.2 ④.
//
// ledger: D2/O12#1 ide/vscode/ 与 scripts/check-d2.sh 不存在
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

// d2Mentions are the patterns that name the deleted deliverable.
//
// The first two are the removed paths. The rest name the extension in prose,
// and they must cover ENGLISH: this repository's README, docs/vcs.md,
// docs/commit-convention.md and several specs have English bodies, so an
// English sentence re-advertising the extension is the most likely way the
// deliverable comes back — and D2/O12's second acceptance clause ("文档无对其
// 作为交付物的描述") has no evidence other than this test. A Chinese-only
// pattern set made that clause vacuous for exactly the documents most likely
// to break it.
//
// The prose patterns pair the product name with the words "extension"/"plugin"
// (or 扩展/插件) rather than matching "vscode" alone, because "vscode" alone
// has legitimate live uses: ".vscode" in docs/vcs.md's ignore list, and
// "ide-vscode" as a commit scope in docs/commit-convention.md. Neither is a
// claim that the extension ships, and a word-level match would redden on both.
//
// The Chinese patterns allow the possessive particle between the product name
// and the noun. CLAUDE.md requires Chinese prose, and in Chinese the possessive
// form is at least as natural as the bare compound — measured, the two differ
// by a single character, and the earlier `\s*` matched only the bare one. That
// is a recall hole in the phrasing this repository is most likely to produce.
//
// Three holes that earlier revisions of this list documented as open are now
// closed, and the reason they stayed open is worth keeping: every pattern that
// closes them also matches ONE live document — the clause-level acceptance
// breakdown under docs/superpowers/, which transcribed the missed phrasings
// verbatim in order to record the holes. Widening the regexes alone reddened
// the build, so the widening had to land together with a tombstone in that
// document and an entry for it in d2HistoricalDocs. It now has. What is closed:
//
//	the product's official full name        d2Product spells both forms
//	the packaged artefact's name            the last pattern below
//	the inverted word order, product name   d2Mentions' parenthetical pattern
//	in trailing parentheses                 (the ledger's own title shape)
//
// The last one mattered most: the parenthesised inversion is exactly how the
// ledger titles this item, so anyone copying the title into a document was
// walking straight through the gate.
//
// A further limit, unchanged: a sentence that describes the deliverable without
// ever putting the product name next to the noun ("a third front end for that
// editor"), or that names a storefront rather than the product ("install it
// from the marketplace"), slips through. Widening that far trades precision for
// recall, and a gate that reddens on unrelated editor notes gets deleted.
//
// The limit in the OTHER direction is worth stating too, because everything
// above is about recall and it would be easy to read this list as precise. It
// is not: these patterns match the phrase, not the claim. A sentence that
// mentions the product-plus-noun in order to DENY that yanshi ships one — the
// competitor comparison, "unlike <rival>, which ships one of these, yanshi is
// terminal-first" — reddens the gate even though it asserts the opposite of an
// advertisement. No live document does that today. The escape is the same one
// the historical documents use: land the sentence with a tombstone and an entry
// in d2HistoricalDocs. Distinguishing assertion from denial needs a parser, not
// a regexp, and this direction of error is the cheap one — it costs an author
// one rewrite, whereas a recall miss costs the acceptance clause its evidence.
const (
	// d2Product matches the product name in both spellings. Hard-coding the
	// abbreviation was the widest hole in this list: the official full name is
	// the more natural spelling in English prose and in any sentence that reads
	// like an announcement, and it matched none of the phrase patterns.
	d2Product = `(?:vs[ _-]?code|visual\s+studio\s+code)`
	// d2Noun matches the nouns that turn a bare product mention into a claim
	// that yanshi delivers something for it, in both languages.
	d2Noun = `(?:扩展|插件|extensions?|plugins?)`
)

var d2Mentions = []*regexp.Regexp{
	regexp.MustCompile(`ide/vscode`),
	regexp.MustCompile(`scripts/check-d2`),
	// Chinese: product name, optional possessive particle, then the noun.
	regexp.MustCompile(`(?i)` + d2Product + `\s*(的|之)?\s*(扩展|插件)`),
	// English: product name then the noun.
	regexp.MustCompile(`(?i)` + d2Product + `\s+(extension|plugin)s?\b`),
	// English, inverted: the noun, then "for", then the product name.
	regexp.MustCompile(`(?i)\b(extension|plugin)s?\s+for\s+` + d2Product + `\b`),
	// Either language, inverted: the noun, then the product name parenthesised
	// as a qualifier. Both bracket shapes, because Chinese prose uses the
	// full-width pair and this repository's prose is Chinese by convention.
	regexp.MustCompile(`(?i)` + d2Noun + `\s*[（(]\s*` + d2Product + `\s*[)）]`),
	// The packaged artefact. Matched bare rather than as a file suffix so that
	// both the file name and prose about publishing the package are caught; the
	// token has no other meaning, so precision costs nothing here.
	regexp.MustCompile(`(?i)\bvsix\b`),
}

// mentionsD2 reports whether body advertises the removed VS Code extension.
func mentionsD2(body string) bool {
	for _, re := range d2Mentions {
		if re.MatchString(body) {
			return true
		}
	}
	return false
}

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
	"docs/superpowers/acceptance-breakdown.md":                             "子句级拆解，逐字抄录了本门禁曾漏掉的写法",
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
// The distinction a grep cannot make — "we will ship this" versus "we cancelled
// shipping this" — is resolved structurally instead of semantically: live docs
// must not mention it at all, and the dated audit/plan/spec documents that must
// keep their mentions are enumerated in d2HistoricalDocs and required to carry
// a tombstone. That is why this clause is testable at all; without the
// enumeration it would be a judgement call, and a judgement call is not
// acceptance criteria.
//
// docs/archive/ is excluded wholesale by directory: it is a declared archive of
// superseded roadmaps, not a description of the current product. reference/ is
// excluded for a stronger reason — it is gitignored working material (upstream
// codex / deepseek-tui checkouts kept for comparison), so it is not yanshi's
// documentation at all and its authors' VS Code extensions are not ours to
// disown.
//
// ledger: D2/O12#2 文档无对其作为交付物的描述
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
				rel == "reference",
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
		if !mentionsD2(body) {
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
