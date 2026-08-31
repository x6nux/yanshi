package tools

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// W-F-11: 按需加载工具 spec。
//
// 工具面约百个（见 F5 报告的实测口径），全量 schema 每轮都进模型请求。这条
// 特性让 orchestrator 按当轮查询检索选取一小批进 schema，其余的「藏起来」——
// 但藏起来不等于删掉：dispatch 列表仍然是全集，工具仍然被 Authorize 照常门禁，
// 只是被模型调用前需要先经 tools_load 显式加载进视野。
//
// 风险在 spec 里写明了：模型看不见的工具就等于不存在，检索选错会让能力静默
// 消失。所以这套机制有三个不可拆的部分：
//
//  1. 检索器（ToolRetriever，BM25）——按查询选 Top-K；
//  2. 逃生门（tools_list / tools_load）——永远可见，列出全部工具名、按名字
//     显式加载；检索 miss 的工具靠它回到视野；
//  3. 过滤只动 state.ToolInfos（模型看到的 schema），从不动 dispatch 集合
//     ——即使模型对没加载的工具产生了幻觉，调用路径与授权路径与全量时逐字
//     节相同。
//
// 默认关闭（零值 = 行为与引入前一致，与 loopguard 同一约定）。
const DefaultMaxVisibleTools = 24

// ToolLoadConfig 是按需加载的开关与参数，由组合根从 config 传入。
type ToolLoadConfig struct {
	// Enabled 打开按需加载。false（默认）时 orchestrator 不装过滤器、
	// 模型看到全量 schema。
	Enabled bool
	// MaxVisible 是除 always + 已加载之外，检索器每轮最多额外放进视野的
	// 工具数。<=0 时取 DefaultMaxVisibleTools。
	MaxVisible int
	// Always 是无论检索结果如何永远可见的工具名（逃生门之外，操作员点名
	// 的「这个部署离不开」清单）。名字必须是注册过的工具；未注册的名字
	// 只是永不命中，无副作用。
	Always []string
}

// ToolsListToolName / ToolsLoadToolName 是逃生门工具的注册名。它们在按需加载
// 打开时必须对模型永远可见，所以 Orchestrator 的过滤器按常量点名而不是按
// 检索分数——「逃生门自己被藏掉」等于整个机制锁死。
const (
	ToolsListToolName = "tools_list"
	ToolsLoadToolName = "tools_load"
)

// ToolMeta 是检索语料里的一条工具记录：注册名 + 模型可见的一句话描述。
type ToolMeta struct {
	Name string
	Desc string
}

// ToolRetriever 是对固定工具语料的 BM25 检索器。不可变：建好即只读，可被
// 并发共享（orchestrator 的 runner 是按 model memoise 的，一个实例服务所有
// 会话）。语料百级规模，检索是一次线性扫描，不需要索引。
type ToolRetriever struct {
	docs   []toolDoc
	df     map[string]int
	avgLen float64
}

type toolDoc struct {
	name   string
	tf     map[string]int
	length int
}

// NewToolRetriever 用全量工具元数据建语料。空语料合法（Select 返回空）。
func NewToolRetriever(metas []ToolMeta) *ToolRetriever {
	r := &ToolRetriever{df: make(map[string]int)}
	total := 0
	for _, m := range metas {
		tokens := tokenizeToolText(m.Name + " " + m.Desc)
		doc := toolDoc{name: m.Name, tf: make(map[string]int), length: len(tokens)}
		for _, t := range tokens {
			doc.tf[t]++
		}
		for t := range doc.tf {
			r.df[t]++
		}
		r.docs = append(r.docs, doc)
		total += doc.length
	}
	if len(r.docs) > 0 {
		r.avgLen = float64(total) / float64(len(r.docs))
	}
	return r
}

// Select 返回按 BM25 得分降序的前 k 个工具名。查询为空或语料为空返回空——
// 没有查询就没有依据，此时视野完全由 always + 已加载决定，这是诚实的退化
// （乱选一个比不选更危险）。
func (r *ToolRetriever) Select(query string, k int) []string {
	if k <= 0 || len(r.docs) == 0 {
		return nil
	}
	qTokens := tokenizeToolText(query)
	if len(qTokens) == 0 {
		return nil
	}
	type scored struct {
		name  string
		score float64
	}
	out := make([]scored, 0, len(r.docs))
	n := float64(len(r.docs))
	for _, doc := range r.docs {
		var s float64
		for _, t := range qTokens {
			tf := float64(doc.tf[t])
			if tf == 0 {
				continue
			}
			df := float64(r.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			norm := 1 - 0.75 + 0.75*float64(doc.length)/r.avgLen
			s += idf * tf * 2.2 / (tf + 1.2*norm)
		}
		if s > 0 {
			out = append(out, scored{name: doc.name, score: s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > k {
		out = out[:k]
	}
	names := make([]string, len(out))
	for i, sc := range out {
		names[i] = sc.name
	}
	return names
}

// tokenizeToolText 小写化后按非字母数字切词。工具名本身按驼峰/下划线断不开
// 没关系——fs_edit 整体与查询里的 fs_edit 相同即命中，描述文本才是检索的
// 主要信号。
func tokenizeToolText(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// toolLoadState 是一轮之内「模型显式加载了哪些工具 spec」的可变状态。
//
// 它按 turn 绑在 context 上（与 loopguard 同一理由：runner 被按 model
// memoise，状态放中间件实例上就是进程级的）。tools_load 工具写它，过滤中间
// 件在每次模型调用前读它；并发工具调用下用锁保护。
type toolLoadState struct {
	mu     sync.Mutex
	loaded map[string]struct{}
}

// WithToolLoadState 绑定一个空的按回合加载状态。每次 turn 都要重新绑定——
// 上一轮加载的工具不自动延续到下一轮（下一轮检索会重新选，模型要跨轮用某个
// 工具就再 load 一次；这样「视野」永远描述的是当前这轮）。
func WithToolLoadState(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolLoadStateKey{}, &toolLoadState{loaded: make(map[string]struct{})})
}

type toolLoadStateKey struct{}

// ToolLoadStateFromContext 读回本回合的加载状态；第二个返回值报告是否绑定
// （未绑定 = 按需加载未开启，tools_load 据此如实报告而不是假装加载了）。
func ToolLoadStateFromContext(ctx context.Context) (*toolLoadState, bool) {
	s, ok := ctx.Value(toolLoadStateKey{}).(*toolLoadState)
	return s, ok && s != nil
}

// Load 把名字并入已加载集合，返回其中**此前不可见**的（新加载的）。已加载
// 的名字幂等。名字按调用方验证过再进来（tools_load 工具负责对照注册全集），
// 这里不二次校验——状态本身不知道全集。
func (s *toolLoadState) Load(names []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newly []string
	for _, n := range names {
		if _, ok := s.loaded[n]; ok {
			continue
		}
		s.loaded[n] = struct{}{}
		newly = append(newly, n)
	}
	return newly
}

// Names 返回已加载名字的快照（排序）。
func (s *toolLoadState) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.loaded))
	for n := range s.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Has 报告名字是否已加载。
func (s *toolLoadState) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.loaded[name]
	return ok
}
