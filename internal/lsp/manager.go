package lsp

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config 配置 Manager。WorkRoot 是工作区根(server 的 rootUri);Languages 把
// 语言名映射到启动命令。空 Languages → disabled。Dial 非空时绕过 exec(测试注入)。
type Config struct {
	WorkRoot  string
	Languages map[string]LanguageServer
	Timeout   time.Duration // 诊断等待上限,0 用默认 800ms
	// Dial 非空时,clientFor 用它取代 exec.Command 拿到 server 的读写端 + closer。
	// 生产留 nil(走 exec);测试注入 net.Pipe 一端连 fake server(评审 #20)。
	Dial func(lang string) (io.Reader, io.Writer, func() error, error)
}

// LanguageServer 描述如何启动一个语言服务器。
type LanguageServer struct {
	Command string
	Args    []string
}

// DefaultLanguages 是 MVP 内置的语言→命令表。命令缺失时由 New 探测并剔除
// 该语言(软降级)。
func DefaultLanguages() map[string]LanguageServer {
	return map[string]LanguageServer{
		"go":     {Command: "gopls"},
		"python": {Command: "pyright-langserver", Args: []string{"--stdio"}},
	}
}

// Manager 管理一个工作区的语言服务器。Enabled()==false 时是 no-op:
// DidChange/Diagnostics 都是空操作,让 edit 工具无条件调用而无需判空。
//
// 并发(评审 #6):mu 保护 clients/cmds/closers;clientFor 在锁内 check-then-spawn。
type Manager struct {
	cfg     Config
	mu      sync.Mutex
	clients map[string]*Client   // 语言 → 已握手 Client
	cmds    map[string]*exec.Cmd // exec 路径的进程句柄(Kill+Wait 用)
	closers []func() error       // Dial 路径的资源 closer
	enabled bool

	// openDocs 是本进程通过 DidChange 告知过 server 的文件,按最近优先排序,
	// 上限 openDocsLimit。这就是 LSP 语义下的"打开的文档":didOpen/didChange
	// 通知过的那批,而不是编辑器窗口里的那批(本进程没有编辑器)。
	// diagnostics 工具据此计算 open_diagnostics_count——在它存在之前那个字段
	// 恒为 0,因为文件列表来自一个恒返回 nil 的桩。
	openDocs []string
}

// openDocsLimit 给最近文档列表封顶。诊断查询是每文件一次 LSP 往返,共享一个
// 总预算,所以无界列表会让 diagnostics 随会话长度线性变慢。
const openDocsLimit = 32

// New 构造 Manager。剔除命令不在 PATH 上的语言(Dial 非空时跳过此剪枝——测试
// 用 fake 命令名);若剔除后无可用语言,返回 disabled Manager。不在此处 spawn——
// spawn 推迟到第一次 DidChange(按文件语言),避免启动慢的 server 拖慢 bootstrap。
func New(cfg Config) *Manager {
	m := &Manager{
		cfg:     cfg,
		clients: map[string]*Client{},
		cmds:    map[string]*exec.Cmd{},
	}
	if cfg.Timeout == 0 {
		m.cfg.Timeout = 800 * time.Millisecond
	}
	usable := map[string]LanguageServer{}
	for lang, ls := range cfg.Languages {
		if cfg.Dial != nil || commandAvailable(ls.Command) {
			usable[lang] = ls
		}
	}
	m.cfg.Languages = usable
	m.enabled = len(usable) > 0 && cfg.WorkRoot != ""
	return m
}

func commandAvailable(cmd string) bool {
	if cmd == "" {
		return false
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

// Enabled 报告是否有至少一个可用语言 server。false 时 Manager 是 no-op。
func (m *Manager) Enabled() bool { return m != nil && m.enabled }

// detectLanguage 按扩展名判语言;无匹配返回空串。
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".c", ".cc", ".cpp", ".h", ".hpp":
		return "cpp"
	case ".rs":
		return "rust"
	}
	return ""
}

// DidChange 告知某文件内容变更。disabled 或无对应语言的文件 → no-op。
func (m *Manager) DidChange(path, content string) {
	if !m.Enabled() {
		return
	}
	lang := detectLanguage(path)
	if lang == "" {
		return
	}
	m.rememberOpen(path)
	c, err := m.clientFor(lang)
	if err != nil || c == nil {
		return
	}
	_ = c.notifyChange(pathToURL(path), content)
}

// rememberOpen 把 path 移到最近文档列表首位(已存在则去重后前移)。
func (m *Manager) rememberOpen(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]string, 0, len(m.openDocs)+1)
	kept = append(kept, path)
	for _, p := range m.openDocs {
		if p == path {
			continue
		}
		if len(kept) == openDocsLimit {
			break
		}
		kept = append(kept, p)
	}
	m.openDocs = kept
}

// OpenDocuments 返回本会话通知过 server 的文件,最近的在前。
//
// 返回副本:调用方(diagnostics 工具)会在不持锁的情况下遍历它并对每一项做一次
// LSP 往返,而 DidChange 可能在此期间并发改写这个切片。
func (m *Manager) OpenDocuments() []string {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.openDocs))
	copy(out, m.openDocs)
	return out
}

// Diagnostics 返回 path 的最新诊断;disabled/无语言/超时 → nil。锁内仅取 client
// 指针(短暂),c.Diagnostics 不持 Manager 锁。
func (m *Manager) Diagnostics(path string, timeout time.Duration) []Diagnostic {
	if !m.Enabled() {
		return nil
	}
	lang := detectLanguage(path)
	if lang == "" {
		return nil
	}
	m.mu.Lock()
	c := m.clients[lang]
	m.mu.Unlock()
	if c == nil {
		return nil
	}
	if timeout == 0 {
		timeout = m.cfg.Timeout
	}
	return c.Diagnostics(pathToURL(path), timeout)
}

// clientFor 懒启动某语言的 server 并握手;失败返回 error(调用方静默降级)。
// 评审 #6:mu 锁内 check-then-spawn,杜绝并发 double-spawn。锁在 spawn+initialize
// 期间持有(每语言 one-time,可接受)。
// 评审 #7:initialize 失败时 Close client + Kill + Wait(cmd) reap 子进程。
func (m *Manager) clientFor(lang string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c, ok := m.clients[lang]; ok {
		return c, nil
	}
	ls, ok := m.cfg.Languages[lang]
	if !ok {
		return nil, os.ErrNotExist
	}

	c, cleanup, err := m.spawnLocked(lang, ls)
	if err != nil {
		return nil, err
	}
	c.Start()
	if err := c.initialize(m.cfg.WorkRoot); err != nil {
		c.Close()
		cleanup() // Kill+Wait(exec) 或 closer(Dial), reap/释放
		return nil, err
	}
	m.clients[lang] = c
	return c, nil
}

// spawnLocked 在已持锁前提下构造 Client(不经 initialize)。返回 Client + 一个
// cleanup(失败/Close 时调:exec 路径关 stdin+Kill+Wait;Dial 路径调 closer)。
// 成功路径下 exec 的 cmd 记入 m.cmds 供 Manager.Close 批量清理。
func (m *Manager) spawnLocked(lang string, ls LanguageServer) (*Client, func(), error) {
	if m.cfg.Dial != nil {
		r, w, closer, err := m.cfg.Dial(lang)
		if err != nil {
			return nil, nil, err
		}
		c := newClient(r, w, m.cfg.Timeout)
		m.closers = append(m.closers, closer)
		return c, func() { _ = closer() }, nil
	}

	cmd := exec.Command(ls.Command, ls.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	c := newClient(stdout, stdin, 5*time.Second) // 握手超时 5s
	m.cmds[lang] = cmd
	cleanup := func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait() // 评审 #7:reap,避免僵尸
	}
	return c, cleanup, nil
}

// Close 关闭所有 client 与底层进程/连接(评审 #16):对每个 client 发标准
// shutdown+exit 序列(best-effort),再 Kill+Wait(exec)/调 closer(Dial)。
// 幂等(enabled Manager 第二次调见 clients 已空)。disabled Manager 是 no-op。
func (m *Manager) Close() error {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	clients := m.clients
	cmds := m.cmds
	closers := m.closers
	m.clients = nil
	m.cmds = nil
	m.closers = nil
	m.enabled = false
	m.mu.Unlock()

	for lang, c := range clients {
		_ = c.Shutdown(context.Background())
		_ = c.Close()
		if cmd := cmds[lang]; cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	for _, closer := range closers {
		_ = closer()
	}
	return nil
}
