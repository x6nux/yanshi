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

// d2Mentions are the patterns that name the deleted deliverable. The first two
// are the removed paths; the rest name the extension in prose.
//
// WHICH SHAPES these cover is deliberately not written here. It is in
// d2ScanCases, a table this package executes — including the shapes that are
// knowingly missed. Prose in this position has been the blocking finding of two
// consecutive reviews, and the cause was structural rather than careless: a
// paragraph that asserts coverage is never run, so it cannot be falsified by
// the build, and every rewrite produced a fresh set of claims that were true
// the day they were written and rotted in silence afterwards. The most recent
// one even contradicted itself, calling a shape closed that a two-line probe
// showed open. What follows is only the reasoning a table cannot carry.
//
// The prose patterns must cover ENGLISH. This repository's README, docs/vcs.md,
// docs/commit-convention.md and several specs have English bodies, so an
// English sentence re-advertising the extension is the most likely way the
// deliverable comes back — and D2/O12's second acceptance clause ("文档无对其
// 作为交付物的描述") has no evidence other than this test. A Chinese-only
// pattern set made that clause vacuous for exactly the documents most likely
// to break it. They must cover CHINESE possessive phrasing for the mirror-image
// reason: CLAUDE.md requires Chinese prose, and in Chinese the possessive form
// is at least as natural as the bare compound while differing from it by a
// single character.
//
// They pair the product name with a noun rather than matching "vscode" alone,
// because "vscode" alone has legitimate live uses: ".vscode" in docs/vcs.md's
// ignore list, "ide-vscode" as a commit scope in docs/commit-convention.md, and
// this gate's own test names quoted in CLAUDE.md and the review checklist. None
// is a claim that the extension ships, and a word-level match reddens on all
// three.
//
// The two directions of error are both real and they do not cost the same. A
// recall miss costs the acceptance clause its evidence, silently, on a green
// build. A false positive costs one author one rewrite, and it announces
// itself. So these patterns lean towards recall — which is also why they match
// the PHRASE and not the CLAIM: a sentence naming product-plus-noun in order to
// DENY that yanshi ships one reddens the gate just the same, and telling
// assertion from denial needs a parser rather than a regexp. The escape is the
// one the historical documents already use: land the sentence with a tombstone
// and an entry in d2HistoricalDocs.
//
// That escape is not optional bookkeeping, and it is worth knowing before
// widening anything: every pattern that closes a hole also matches the document
// that RECORDED the hole. The clause-level acceptance breakdown under
// docs/superpowers/ transcribes missed phrasings verbatim, so widening the
// regexes alone reddens the build — a widening has to land together with that
// document's tombstone and its d2HistoricalDocs entry.
const (
	// d2Product matches the product name in both spellings. Hard-coding the
	// abbreviation was the widest hole in this list: the official full name is
	// the more natural spelling in English prose and in any sentence that reads
	// like an announcement, and it matched none of the phrase patterns.
	d2Product = `(?:vs[ _-]?code|visual\s+studio\s+code)`
	// d2NounCN matches the Chinese nouns. 拓展 is not the standard word (扩展
	// is), but it is a common enough mis-spelling that excluding it buys
	// nothing.
	d2NounCN = `(?:扩展|拓展|插件)`
	// d2NounEN matches the English nouns. The hyphen is not an optional extra:
	// "plug-in" is the dictionary head-word, so a pattern set that knows only
	// the closed-up spelling misses the MORE standard one.
	//
	// The SPACED variant was a mistake and is gone. "plug in" is a verb phrase,
	// not a noun, so `plug[ -]?in` reddened on sentences containing no noun at
	// all ("plug in for VS Code remote debugging sessions"). Dropping it is a
	// strict narrowing that costs no recall, because the noun has exactly two
	// standard spellings and this now matches both. The `s?` sits outside the
	// group so every spelling pluralises.
	d2NounEN = `(?:extension|plug-?in)s?`
	// d2Noun matches the nouns that turn a bare product mention into a claim
	// that yanshi delivers something for it, in both languages. Every phrase
	// pattern below composes from these constants rather than inlining an
	// alternation of its own, and that rule was bought with a bug: the nouns
	// used to be spelled out in FOUR places — d2Noun itself, two English phrase
	// patterns writing `(extension|plugin)s?` by hand, and one Chinese pattern
	// writing the unrelated `(扩展|插件)` — so widening the shared constant fixed
	// one site of four and looked done. The Chinese pattern is why the tally
	// matters rather than the arithmetic: it was never an English pattern, so
	// fixing "the English ones" left mixed-script prose ("VS Code 的 plug-in")
	// invisible for a further round. It is fixed here by moving the possessive
	// particle into d2GlueEN, not by feeding English nouns to a pattern whose
	// glue may be empty — see d2GlueEN.
	d2Noun = `(?:` + d2NounCN + `|` + d2NounEN + `)`
	// d2GlueEN is what may sit between the product name and an English noun: an
	// optional possessive (straight or curly apostrophe), then a REAL separator
	// — whitespace, one connector character ("vscode-extension"), or the Chinese
	// possessive particle for mixed-script prose.
	//
	// Two things it deliberately is not. It is not `[\s_-]+`: a class mixing
	// whitespace with connectors swallows a markdown line break plus the next
	// bullet's dash, so an ignore list reading ".vscode" and then "- extension
	// settings" reddens on two unrelated entries.
	//
	// And it is not optional: a zero-width glue turns a CamelCase identifier
	// that ENDS at the noun into a match — "VSCodeExtension" and its plural,
	// which is the shape a Go symbol, a type name or a doc heading takes. Both
	// are `want: false` rows in d2ScanCases, and making this group optional
	// reddens exactly those two rows. That mutation is how this sentence is
	// checked rather than believed; run it before editing this group.
	//
	// What this group does NOT do is keep the gate off its own test name.
	// "TestVSCodeExtensionRemoved" survives an optional glue as well, because
	// the pattern that would have to match it ends in `\b` and the noun is
	// followed by the "R" of "Removed". The mechanism there is the word
	// boundary, not this group. A previous revision of this comment credited
	// the glue for it — and of the three neighbouring shapes, that is the one
	// where the claim happens to be false.
	d2GlueEN = `(?:['’]s)?(?:\s*(?:的|之)\s*|\s+|[_-])`
)

var d2Mentions = []*regexp.Regexp{
	regexp.MustCompile(`ide/vscode`),
	regexp.MustCompile(`scripts/check-d2`),
	// Chinese: product name, optional possessive particle, then a CJK noun. The
	// glue may be zero-width because CJK compounds take no separator — which is
	// also why this pattern stays CJK-only. Mixed-script prose goes through the
	// English pattern instead, whose glue accepts the particle.
	regexp.MustCompile(`(?i)` + d2Product + `\s*(的|之)?\s*` + d2NounCN),
	// English (and mixed-script): product name, glue, then the noun.
	regexp.MustCompile(`(?i)` + d2Product + d2GlueEN + d2NounEN + `\b`),
	// English, inverted: the noun, then "for", then the product name.
	regexp.MustCompile(`(?i)\b` + d2NounEN + `\s+for\s+` + d2Product + `\b`),
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

// d2ScanCases is the executable statement of what mentionsD2 covers.
//
// It exists because that statement used to be a paragraph above d2Mentions. A
// paragraph asserts and never runs, so nothing could falsify it, and it went
// stale twice in a row — the second time claiming a shape was covered that a
// two-line probe showed slipping through. Every claim about this gate's reach
// now lives here as a case with a `want` and a `why`, so a wrong claim is a red
// build rather than a misleading comment.
//
// The `want: false` rows carry as much weight as the `want: true` ones, and do
// two different jobs. Most pin a LEGITIMATE live use that must never redden —
// the ignore-list path, the commit scope, this gate's own test names, English
// verb phrases that happen to contain the noun's letters. A few pin a KNOWN
// MISS: a shape deliberately not chased because catching it would redden on
// unrelated editor notes, and a gate that does that gets deleted. Those rows
// are the point of the table. They turn "what this gate does not catch" from a
// claim into a fact, and the day someone widens a pattern far enough to catch
// one, this table goes red and makes them say so in writing.
//
// `why` is mandatory for the same reason: whoever deletes a row should have to
// read what it was guarding first.
var d2ScanCases = []struct {
	phrase string
	want   bool
	why    string
}{
	// --- 已删路径 ---
	{"ide/vscode/extension.ts", true, "已删的扩展源码路径"},
	{"scripts/check-d2.sh", true, "已删的 CI 辅助脚本"},

	// --- 英文短语：产品名 + 名词 ---
	{"VS Code extension", true, "基线形态：缩写产品名 + 名词"},
	{"Visual Studio Code plugin", true, "官方全称——英文公告体最自然的写法，曾是本表最大的洞"},
	{"vscode plugins", true, "无空格缩写 + 复数"},
	{"VS Code plug-in", true, "第 22 轮：名词内部的连字符，词典标准写法"},
	{"VS Code plug-ins", true, "连字符 + 复数，`s?` 必须在组外"},
	{"vscode-extension", true, "第 23 轮：产品名与名词之间的分隔符（d2GlueEN 的 [_-] 分支）"},
	{"vs_code_extension", true, "第 23 轮：同上，下划线变体"},
	{"VS Code's extension", true, "第 23 轮：所有格，直角引号"},
	{"VS Code’s extension", true, "第 23 轮：所有格，弯引号（markdown 编辑器常自动替换）"},

	// --- 中文与中英混写 ---
	{"VS Code 扩展", true, "中文复合，产品名与名词之间零宽"},
	{"VS Code 的插件", true, "中文所有格——CLAUDE.md 要求中文行文，这是最可能产生的写法"},
	{"VS Code 之拓展", true, "文言所有格 + 常见误拼 拓展"},
	{"yanshi 提供 VS Code 的 plug-in。", true, "第 23 轮：中英混写，中文助词 + 英文名词"},

	// --- 倒装 ---
	{"extension for VS Code", true, "倒装：名词 + for + 产品名"},
	{"plug-ins for Visual Studio Code", true, "倒装 + 连字符 + 全称"},
	{"插件（VS Code）", true, "括号倒装，全角括号——台账给本条目起的标题就是这个形状"},
	{"extension (VSCode)", true, "括号倒装，半角括号"},

	// --- 打包制品 ---
	{"yanshi.vsix", true, "打包制品的文件名"},
	{"publish the vsix", true, "同一个 token 的散文用法，该 token 没有别的含义"},

	// --- 已知误伤：匹配短语而非主张 ---
	{
		"unlike <rival>, which ships a VS Code extension, yanshi is terminal-first",
		true,
		"已知误伤：这句在否认 yanshi 交付，门禁照样变红。正则匹配短语不匹配主张，" +
			"区分二者要的是 parser。这个方向的代价是作者改一次措辞，逃生门是墓碑 + d2HistoricalDocs",
	},
	{
		"https://github.com/microsoft/vscode-extension-samples",
		true,
		"已知误伤（第 24 轮补记）：d2GlueEN 的 [_-] 分支让上游仓库的 URL 也变红，加进那个分支之前不会。" +
			"当前没有任何活文档命中，所以不改正则——与本门禁「召回优先于精度」的取舍一致，" +
			"且收窄它就等于放掉 vscode-extension 这个真实写法。要引用这个仓库，逃生门同上",
	},

	// --- 合法活用：不得误伤 ---
	{".vscode", false, "docs/vcs.md 的 ignore 列表"},
	{".vscode/settings.json", false, "同上，带路径后缀"},
	{"ide-vscode", false, "docs/commit-convention.md 的提交 scope"},
	{"vscode", false, "裸产品名，不构成交付主张"},
	{"Visual Studio extension", false, "另一个产品（少了 Code），不是本条审计项"},
	{
		"TestVSCodeExtensionRemoved",
		false,
		"本门禁自己的测试名，CLAUDE.md 与评审清单都引用它。守住它的是英文模式尾部的 `\\b`" +
			"（撞上 Removed 的 R），不是 d2GlueEN——把 glue 零宽化这一行照样绿。" +
			"它实际守的是另一件事：中文模式的 glue 本来就可以零宽，一旦把 d2NounEN 喂给它，" +
			"这一行立刻误伤，所以中英名词不得合并进同一个零宽 glue 的模式",
	},
	{
		"VSCodeExtension",
		false,
		"CamelCase 标识符：产品名与名词零宽相接，末尾没有别的词。" +
			"d2GlueEN 强制非空正是为了它——把 glue 改成可选，这一行当场变红。" +
			"这一行与下一行才是钉住那个设计选择的用例",
	},
	{
		"VSCodeExtensions",
		false,
		"同上的复数形态。两条都留着，因为 `s?` 在组外意味着复数走的是另一条匹配路径，" +
			"只钉单数会给「把 s 挪进组内」留一个静默的洞",
	},
	{"plug in the cable", false, "第 23 轮 (b)：裸动词短语"},
	{
		"plug in for VS Code remote debugging sessions",
		false,
		"第 23 轮 (b)：动词短语 + 产品名，句中没有任何名词形态。" +
			"`plug[ -]?in` 的空格变体就死在这里",
	},
	{"you can plug in for VSCode users too", false, "第 23 轮 (b)：同上，倒装 for 分支的版本"},
	{
		"- .vscode\n- extension settings\n",
		false,
		"markdown 列表跨行：把 d2GlueEN 写成 [\\s_-]+ 会吞掉换行加下一项的短横，" +
			"于是两条互不相干的 ignore 条目被拼成一次广告",
	},

	// --- 已知漏：扩到这里精度代价过大，记录而不追 ---
	{
		"a third front end for that editor",
		false,
		"已知漏：描述了交付物但产品名与名词从不相邻。" +
			"扩到能抓它就会在无关的编辑器笔记上变红，而变红的门禁会被删掉",
	},
	{
		"install it from the marketplace",
		false,
		"已知漏：只提商店不提产品名。同上，扩到这里等于拿精度换召回",
	},
}

// TestD2MentionShapes runs d2ScanCases against mentionsD2.
//
// Both polarities are required to stay non-empty: a table that drifted to
// all-positive would stop defending the ignore-list path and the commit scope,
// and one that drifted to all-negative would pass while matching nothing.
func TestD2MentionShapes(t *testing.T) {
	var pos, neg int
	for _, c := range d2ScanCases {
		if c.why == "" {
			t.Errorf("d2ScanCases entry %q has no why — record what it guards", c.phrase)
		}
		if c.want {
			pos++
		} else {
			neg++
		}
		got := mentionsD2(c.phrase)
		if got == c.want {
			continue
		}
		if c.want {
			t.Errorf("mentionsD2(%q) = false, want true — this shape must be caught: %s",
				c.phrase, c.why)
			continue
		}
		t.Errorf("mentionsD2(%q) = true, want false — %s\n"+
			"If this was a deliberate widening, update the case (and say so in why) "+
			"rather than deleting it: this row is how the gate's reach stays reviewable.",
			c.phrase, c.why)
	}
	if pos == 0 || neg == 0 {
		t.Errorf("d2ScanCases has %d positive and %d negative cases — both polarities "+
			"are load-bearing; the negative ones are what keeps the gate from "+
			"reddening on .vscode and ide-vscode.", pos, neg)
	}
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
			case isNestedCheckoutDir(rel, d.Name()),
				rel == "docs/archive", rel == "third_party", rel == "reference":
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
