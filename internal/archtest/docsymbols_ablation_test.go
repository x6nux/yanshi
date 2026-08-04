package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// GOV9 — layer ablation: is each emphasis defence still earning its keep?
// ---------------------------------------------------------------------------
//
// GOV9's typographic defence is three layers deep — stripMarkdownEmphasis, the
// letter-start anchor in docSymbolRefRe, and deglued(). A review measured them
// against the LIVE corpus and found the marginal recall of every single one to
// be zero: the gate's verdict on every citation in every live document is
// identical with all three on, with two on, with one on, and with none on. The
// layers are mutually redundant on the text this repository actually contains.
//
// That measurement is the reason CLAUDE.md's GOV9 section now carries a closing
// clause: a typographic bypass that can be CONSTRUCTED on a synthetic module is
// not, by itself, a defect to fix. What makes it a defect is a real dead
// citation escaping through it in a live document. The distinction only means
// anything if somebody can compute which side of it a proposed hole falls on,
// and "somebody re-runs the ablation by hand during review" is exactly the kind
// of promise F3 was written about. So the measurement lives here instead.
//
// WHY THE LAYERS ARE NOT DELETED, even though today's marginal recall is zero.
// The redundancy is a property of the CORPUS, not of the code. Each layer still
// has shapes only it covers, and those shapes are absent from today's documents
// rather than impossible:
//
//   - Only the strip covers `**path**::Symbol`, where the delimiters land
//     BETWEEN the path and the separator. Neither of the other two layers ever
//     sees a match to repair — the regexp produces none at all.
//   - Only the anchor covers a digit or a `.`/`-` glued to the front of the
//     path, because the strip's keep rule reads such a run as intraword and
//     deglued() only retries on `_`.
//   - Only deglued() covers an ASCII word glued on with an underscore
//     (`see_internal/...`), which is byte-for-byte a legal path at the other
//     two layers.
//
// Deleting a layer would therefore trade a bounded, already-written cost for an
// unbounded and SILENT one — a citation that goes invisible the day somebody
// writes one of those spellings, which is the precise failure GOV9 exists to
// remove. And it would destroy the signal that makes the closing clause usable:
// once a layer is gone, its marginal recall can no longer be measured, so
// "nobody needs this" becomes unfalsifiable.
//
// A zero here is thus the healthy reading, not a deletion request. A NON-zero
// is the interesting event: it means the corpus has grown a shape that exactly
// one layer catches, which both settles that layer's fate permanently and is
// the trigger condition the closing clause names.
//
// WHEN THIS TEST REDDENS ON AN INNOCENT EDIT, and why that is kept rather than
// designed away. Exactly two spellings of a LIVE citation move a number:
// underscore emphasis around the whole citation (`_pkg/x.go::Sym_`, judged
// against "Sym_" without the strip) and emphasis around the path alone
// (`**pkg/x.go**::Sym`, which produces no match at all without the strip).
// Both documents are perfectly legitimate and both numbers are perfectly true —
// such a citation really has become the first one in the corpus whose verdict
// rests on a single layer. The remedy is one edit either way: change the
// decoration, or record the new count with the citation named beside it. This
// is the bargain acceptancePins strikes for the ledger, and it is written down
// so the red arrives as a documented signal instead of a surprise.
//
// Everything else leaves every number at zero — including `**...**` around a
// whole citation, this repository's dominant style, because `*` is in neither
// the path nor the symbol character class.
// TestGOV9CommonEmphasisDoesNotMoveMarginalRecall pins both halves of that
// boundary, so "which edits can redden this" is a fact rather than a guess.

// gov9Layers selects which of GOV9's three typographic defences are active.
// The zero value is the fully ablated pipeline — every defence off — which is
// the configuration the review found indistinguishable from the full one.
type gov9Layers struct {
	Strip  bool // stripMarkdownEmphasis runs before extraction
	Anchor bool // docSymbolRefRe's "path must start with a letter" rule
	Deglue bool // (*goSymbolIndex).deglued retries an underscore-glued path
}

// gov9FullDefence is the shipping configuration: all three layers on.
var gov9FullDefence = gov9Layers{Strip: true, Anchor: true, Deglue: true}

// docSymbolRefNoAnchorRe is docSymbolRefRe with the letter-start anchor
// removed, i.e. the shape the regexp had before that rule was added. It exists
// only so the anchor can be ablated; nothing in the live gate uses it.
var docSymbolRefNoAnchorRe = regexp.MustCompile(`([A-Za-z0-9_./-]+)::([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)

// parseDocSymbolRefsWith is parseDocSymbolRefs with the strip and anchor layers
// made switchable. With l == gov9FullDefence it is byte-for-byte the production
// pipeline, and TestGOV9AblationBaselineMatchesProductionPipeline pins that so
// the ablation cannot drift into measuring a different gate than the one that
// ships.
func parseDocSymbolRefsWith(doc, body string, l gov9Layers) []docSymbolRef {
	re := docSymbolRefNoAnchorRe
	if l.Anchor {
		re = docSymbolRefRe
	}
	var refs []docSymbolRef
	for i, line := range strings.Split(body, "\n") {
		scanned := line
		if l.Strip {
			scanned = stripMarkdownEmphasis(line)
		}
		for _, m := range re.FindAllStringSubmatch(scanned, -1) {
			refs = append(refs, docSymbolRef{
				Doc: doc, Line: i + 1,
				Path: m[1], Symbol: m[2], Raw: m[0],
			})
		}
	}
	return refs
}

// gov9ProtectedKeys returns the set of citations the gate can actually reach
// under l, keyed by "doc:line:symbol".
//
// "Reached" means the PATH resolved, because that is the condition under which
// unresolvedDocSymbols is willing to judge the symbol at all; an unresolvable
// path is skipped as "not ours" and the citation carries no protection. Keying
// on the symbol rather than on the position is what makes a DEMOTED citation
// count as lost too: an unstripped `_pkg/x.go::Sym_` resolves its path but gets
// judged against the name "Sym_", so the real citation of "Sym" is unprotected
// even though something at that position was examined.
func gov9ProtectedKeys(idx *goSymbolIndex, refs []docSymbolRef, l gov9Layers) map[string]bool {
	out := map[string]bool{}
	for _, r := range refs {
		files := idx.candidates(r.Path)
		if len(files) == 0 && l.Deglue {
			files = idx.deglued(r.Path)
		}
		if len(files) == 0 {
			continue
		}
		out[fmt.Sprintf("%s:%d:%s", r.Doc, r.Line, r.Symbol)] = true
	}
	return out
}

// unresolvedDocSymbolsWith is unresolvedDocSymbols with the deglue layer made
// switchable. unresolvedDocSymbols itself hardcodes the retry, so ablating that
// layer is impossible through the production entry point — a fact that cost
// this file one wrong test before it was noticed.
func unresolvedDocSymbolsWith(idx *goSymbolIndex, refs []docSymbolRef, l gov9Layers) []docSymbolRef {
	var bad []docSymbolRef
	for _, r := range refs {
		files := idx.candidates(r.Path)
		if len(files) == 0 && l.Deglue {
			files = idx.deglued(r.Path)
		}
		if len(files) == 0 {
			continue
		}
		if !idx.resolves(files, r.Symbol) {
			bad = append(bad, r)
		}
	}
	return bad
}

// gov9LiveCorpus reads every live document once so the four ablation runs share
// the bytes instead of re-walking the tree.
func gov9LiveCorpus(t *testing.T, root string) map[string]string {
	t.Helper()
	corpus := map[string]string{}
	for _, doc := range liveDocs(t, root) {
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		corpus[doc] = string(body)
	}
	return corpus
}

// gov9ProtectedUnder runs the whole pipeline over the live corpus under l.
func gov9ProtectedUnder(idx *goSymbolIndex, corpus map[string]string, l gov9Layers) map[string]bool {
	docs := make([]string, 0, len(corpus))
	for doc := range corpus {
		docs = append(docs, doc)
	}
	sort.Strings(docs)
	var refs []docSymbolRef
	for _, doc := range docs {
		refs = append(refs, parseDocSymbolRefsWith(doc, corpus[doc], l)...)
	}
	return gov9ProtectedKeys(idx, refs, l)
}

// gov9Ablation is one row of the measurement below.
type gov9Ablation struct {
	Name   string
	Layers gov9Layers
	// WantLost is the number of citations that the full defence protects and
	// this configuration does not — the configuration's marginal recall. See
	// gov9MarginalRecallIsMeasured for what a mismatch means.
	WantLost int
}

// gov9Ablations enumerates the configurations the closing clause is stated
// over: each layer removed on its own, plus the fully-ablated pipeline the
// review found indistinguishable from the shipping one.
//
// Every WantLost is 0, and that is a MEASUREMENT of the current corpus rather
// than a target. Do not "fix" a mismatch by editing the number: a non-zero
// value is the closing clause's trigger condition and means the corpus now
// contains a citation that only this layer keeps in view. Find that citation
// (the failure message names it), confirm the layer is what saves it, and only
// then record the new count with the citation cited alongside it.
var gov9Ablations = []gov9Ablation{
	{Name: "strip off", Layers: gov9Layers{Anchor: true, Deglue: true}, WantLost: 0},
	{Name: "anchor off", Layers: gov9Layers{Strip: true, Deglue: true}, WantLost: 0},
	{Name: "deglue off", Layers: gov9Layers{Strip: true, Anchor: true}, WantLost: 0},
	{Name: "all three off", Layers: gov9Layers{}, WantLost: 0},
}

// TestGOV9MarginalRecallOfEachDefenceLayer measures, on the live corpus, how
// many citations each of GOV9's typographic defences is the sole reason the
// gate can still see.
//
// This is the machine half of CLAUDE.md's GOV9 closing clause. The clause says
// a constructible bypass is not a blocker unless a real dead citation escapes
// through it; this test computes the "unless". It replaces an argument that
// otherwise runs every review — "could somebody write text that slips past this
// layer?" (yes, always) — with a number nobody has to trust: how many citations
// in the documents that exist today would lose their protection if the layer
// were removed.
//
// The same run also prints the gate's absolute coverage, which is the other
// number the clause leans on: GOV9 is worth defending at all because it already
// reaches nearly every citation in the corpus, and the handful it does not are
// the deliberate escape hatch (placeholders, phantoms, foreign `::` syntaxes).
func TestGOV9MarginalRecallOfEachDefenceLayer(t *testing.T) {
	root := moduleRoot(t)
	idx := buildGoSymbolIndex(t, root)
	corpus := gov9LiveCorpus(t, root)

	full := gov9ProtectedUnder(idx, corpus, gov9FullDefence)
	// Denominator and numerator must be keyed identically or the ratio is
	// nonsense: two identical citations on one line collapse to one key, so
	// counting raw matches on one side and keys on the other understates
	// coverage. Both sides are distinct "doc:line:symbol" keys here.
	parsed := map[string]bool{}
	occurrences := 0
	for _, doc := range sortedCorpusDocs(corpus) {
		refs := parseDocSymbolRefsWith(doc, corpus[doc], gov9FullDefence)
		occurrences += len(refs)
		for _, r := range refs {
			parsed[fmt.Sprintf("%s:%d:%s", r.Doc, r.Line, r.Symbol)] = true
		}
	}
	if len(parsed) == 0 || len(full) == 0 {
		t.Fatalf("no citations reached under the full defence (%d distinct, %d protected) — "+
			"the scan is broken, not the docs", len(parsed), len(full))
	}
	t.Logf("live corpus: %d citation occurrences, %d distinct, %d protected by the full defence (%.1f%%)",
		occurrences, len(parsed), len(full), 100*float64(len(full))/float64(len(parsed)))

	for _, ab := range gov9Ablations {
		got := gov9ProtectedUnder(idx, corpus, ab.Layers)
		lost := gov9SetDifference(full, got)
		gained := gov9SetDifference(got, full)
		t.Logf("%-14s protected=%d marginal-recall=%d (also reached %d the full defence does not)",
			ab.Name, len(got), len(lost), len(gained))
		if len(lost) != ab.WantLost {
			t.Errorf("%q: marginal recall is %d, recorded as %d.\n"+
				"\tCitations that lose their protection under this configuration:\n\t\t%s\n"+
				"\tThis is not a bug in this test and not necessarily a bug in the document:\n"+
				"\tit means the live corpus now contains a citation whose verdict depends on\n"+
				"\tthis one layer, which is the trigger condition in CLAUDE.md's GOV9 closing\n"+
				"\tclause. Two ways out, both one edit:\n"+
				"\t  1. If the citation is merely dressed in UNDERSCORE emphasis (_pkg/x.go::Sym_),\n"+
				"\t     drop the emphasis or switch to ** ** — asterisks move no number.\n"+
				"\t  2. Otherwise confirm the layer is genuinely what saves those citations,\n"+
				"\t     then record the new count in gov9Ablations with the citation named.",
				ab.Name, len(lost), ab.WantLost, strings.Join(lost, "\n\t\t"))
		}
	}
}

// gov9SetDifference returns the sorted keys present in a but not in b.
func gov9SetDifference(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestGOV9AblationBaselineMatchesProductionPipeline pins the ablation harness
// to the gate it claims to measure.
//
// parseDocSymbolRefsWith is a second copy of the extraction loop, and a copy is
// a place where a measurement can quietly stop describing the thing measured:
// if the production pipeline gained a step this one did not, every marginal
// recall above would still read 0 and would still mean nothing. So the two are
// compared on the real corpus, not on a sample — the full-defence configuration
// must produce byte-identical citations to parseDocSymbolRefs.
//
// It also pins the other direction that matters for the closing clause: the
// fully ablated pipeline must reach FEWER-OR-EQUAL citations, never more. A
// configuration that protects something the shipping gate does not would mean a
// layer is net-harmful, and the marginal-recall numbers would be measuring the
// wrong sign.
func TestGOV9AblationBaselineMatchesProductionPipeline(t *testing.T) {
	root := moduleRoot(t)
	corpus := gov9LiveCorpus(t, root)

	for _, doc := range sortedCorpusDocs(corpus) {
		want := parseDocSymbolRefs(doc, corpus[doc])
		got := parseDocSymbolRefsWith(doc, corpus[doc], gov9FullDefence)
		if len(want) != len(got) {
			t.Fatalf("%s: full-defence ablation parsed %d citations, production parsed %d",
				doc, len(got), len(want))
		}
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("%s: citation %d diverges\n\tproduction: %+v\n\tablation:   %+v",
					doc, i, want[i], got[i])
			}
		}
	}

	idx := buildGoSymbolIndex(t, root)
	full := gov9ProtectedUnder(idx, corpus, gov9FullDefence)
	for _, ab := range gov9Ablations {
		got := gov9ProtectedUnder(idx, corpus, ab.Layers)
		if len(got) > len(full) {
			t.Errorf("%q protects %d citations, more than the full defence's %d — "+
				"a layer is removing coverage instead of adding it", ab.Name, len(got), len(full))
		}
	}
}

// TestGOV9CommonEmphasisDoesNotMoveMarginalRecall is the reverse probe for the
// measurement, and it bounds the one way that measurement can redden on an
// innocent edit.
//
// A pinned number that moved every time somebody bolded a citation would be
// deleted within a week — this repository's documents are full of
// `**path::Symbol**`. So the dominant decorations are driven end to end and
// must leave every layer's marginal recall at zero. Only the underscore
// spelling is allowed to move it, and that row is here too, asserted rather
// than merely tolerated, so the boundary is a fact instead of an assumption.
func TestGOV9CommonEmphasisDoesNotMoveMarginalRecall(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\nfunc NewName() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	marginal := func(body string, l gov9Layers) int {
		corpus := map[string]string{"d.md": body}
		full := gov9ProtectedUnder(idx, corpus, gov9FullDefence)
		return len(gov9SetDifference(full, gov9ProtectedUnder(idx, corpus, l)))
	}

	for _, body := range []string{
		"plain: internal/pkg/thing.go::NewName",
		"bold: **internal/pkg/thing.go::NewName**",
		"italic: *internal/pkg/thing.go::NewName*",
		"bold+italic: ***internal/pkg/thing.go::NewName***",
		"code span: `internal/pkg/thing.go::NewName`",
		"strikethrough: ~~internal/pkg/thing.go::NewName~~",
		"link text: [internal/pkg/thing.go::NewName](x.md)",
		"CJK quotes: 「internal/pkg/thing.go::NewName」",
		"| table cell | internal/pkg/thing.go::NewName |",
	} {
		for _, ab := range gov9Ablations {
			if got := marginal(body, ab.Layers); got != 0 {
				t.Errorf("%q moved %q's marginal recall to %d; a decoration this common "+
					"must not disturb the measurement", body, ab.Name, got)
			}
		}
	}

	// The two documented exceptions, asserted rather than merely tolerated so
	// the boundary of the list above is a fact. Both are spellings the live
	// corpus does not currently contain, which is why every row today is 0.
	for _, tc := range []struct {
		name string
		body string
	}{
		// Underscore emphasis wraps INTO both character classes, so without
		// the strip the citation is judged against the name "NewName_".
		{"underscore italic", "italic: _internal/pkg/thing.go::NewName_"},
		// Emphasis around the PATH ONLY puts the delimiters between the path
		// and the separator, where no regexp variant produces a match at all —
		// the citation does not get demoted, it disappears.
		{"path-only bold", "partial bold: **internal/pkg/thing.go**::NewName"},
	} {
		if got := marginal(tc.body, gov9Layers{Anchor: true, Deglue: true}); got != 1 {
			t.Errorf("%s must move the strip layer's marginal recall to 1, got %d", tc.name, got)
		}
	}
}

// sortedCorpusDocs returns the corpus keys in a stable order so a failure names
// the same document on every run.
func sortedCorpusDocs(corpus map[string]string) []string {
	docs := make([]string, 0, len(corpus))
	for doc := range corpus {
		docs = append(docs, doc)
	}
	sort.Strings(docs)
	return docs
}

// TestGOV9EachLayerCoversAShapeTheOthersDoNot is the reverse of the measurement
// above, and it is why a zero marginal recall must not be read as a deletion
// request.
//
// Each row names a spelling that exactly ONE layer keeps visible, driven against
// a synthetic module holding a single renamed symbol. With the full defence the
// dead citation is reported; with that one layer removed it vanishes — not
// demoted to a weaker check, but dropped entirely as "not a citation of this
// module". The live corpus happens to contain none of these spellings today,
// which is the whole content of the zero; nothing makes them unwritable
// tomorrow.
func TestGOV9EachLayerCoversAShapeTheOthersDoNot(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\nfunc NewName() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	for _, tc := range []struct {
		name    string
		body    string
		without gov9Layers // the full defence minus the layer under test
	}{
		{
			// Emphasis covering only the path puts the delimiters between the
			// path and "::", so no regexp variant produces a match at all.
			name:    "strip: emphasis between path and separator",
			body:    "partial bold: **internal/pkg/thing.go**::NewNam",
			without: gov9Layers{Anchor: true, Deglue: true},
		},
		{
			// A digit glued to the path is intraword for the strip and is not
			// an underscore for deglued(); only the letter-start anchor
			// re-anchors the citation onto the real path.
			name:    "anchor: digit glued to the path",
			body:    "digit abutting: 1internal/pkg/thing.go::NewNam",
			without: gov9Layers{Strip: true, Deglue: true},
		},
		{
			// An ASCII word joined by an underscore is intraword for the strip
			// and starts with a letter, so it satisfies the anchor too. Only
			// the first-segment retry finds the real path underneath.
			name:    "deglue: ascii word glued with an underscore",
			body:    "latin then underscore: see_internal/pkg/thing.go::NewNam",
			without: gov9Layers{Strip: true, Anchor: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAll := unresolvedDocSymbolsWith(idx,
				parseDocSymbolRefsWith("d.md", tc.body, gov9FullDefence), gov9FullDefence)
			if len(withAll) != 1 {
				t.Fatalf("the full defence must report this dead citation, got %+v", withAll)
			}
			withoutLayer := unresolvedDocSymbolsWith(idx,
				parseDocSymbolRefsWith("d.md", tc.body, tc.without), tc.without)
			if len(withoutLayer) != 0 {
				t.Fatalf("this shape was supposed to be covered by exactly one layer, "+
					"but survives without it: %+v", withoutLayer)
			}
		})
	}
}
