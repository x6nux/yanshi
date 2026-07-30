package tui

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFrecency_RecordAndTopN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frec.json")
	f, err := LoadFrecency(path)
	if err != nil {
		t.Fatalf("LoadFrecency: %v", err)
	}

	// 显式 for-loop 逐条 Record。
	for i := 0; i < 3; i++ {
		if err := f.Record("/proj/main.go"); err != nil {
			t.Fatalf("Record main.go: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := f.Record("/proj/util.go"); err != nil {
			t.Fatalf("Record util.go: %v", err)
		}
	}
	if err := f.Record("/proj/rare.go"); err != nil {
		t.Fatalf("Record rare.go: %v", err)
	}

	top := f.TopN(2)
	if len(top) != 2 {
		t.Fatalf("TopN(2) 应返回 2 项,得到 %d", len(top))
	}
	if top[0] != "/proj/main.go" {
		t.Errorf("Top1 应是 main.go(访问 3 次),得到 %q", top[0])
	}
	if top[1] != "/proj/util.go" {
		t.Errorf("Top2 应是 util.go(访问 2 次),得到 %q", top[1])
	}
}

func TestFrecency_PersistAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frec.json")
	f1, _ := LoadFrecency(path)
	f1.Record("/a")
	f1.Record("/a")
	f1.Record("/b")
	if err := f1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	f2, err := LoadFrecency(path)
	if err != nil {
		t.Fatalf("re-LoadFrecency: %v", err)
	}
	top := f2.TopN(2)
	if len(top) != 2 || top[0] != "/a" || top[1] != "/b" {
		t.Fatalf("重载后排序应保持 [a, b],得到 %v", top)
	}
}

func TestFrecency_DecayOldEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frec.json")
	f, _ := LoadFrecency(path)
	// 旧条目(8 天前):count=10 但已"过期"
	old := frecencyEntry{Path: "/old.go", Count: 10, FirstSeen: time.Now().Add(-8 * 24 * time.Hour), LastSeen: time.Now().Add(-8 * 24 * time.Hour)}
	// 新条目(刚刚):count=1
	f.mu.Lock()
	f.entries = append(f.entries, old)
	f.mu.Unlock()
	f.Record("/new.go")

	top := f.TopN(1)
	if top[0] != "/new.go" {
		t.Errorf("新条目应排在旧条目前(decay),Top1=%q", top[0])
	}
}

// TestFrecency_ConcurrentRecord 验证 sync.Mutex 真的存在(review 发现原计划没加)。
func TestFrecency_ConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frec.json")
	f, _ := LoadFrecency(path)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Record("/concurrent.go")
		}()
	}
	wg.Wait()
	top := f.TopN(1)
	if len(top) != 1 || top[0] != "/concurrent.go" {
		t.Fatalf("并发后应得到 1 项,得到 %v", top)
	}
	if got := f.entries[0].Count; got != 100 {
		t.Errorf("100 次并发 Record 后 Count 应为 100,race detector 应报警无 mutex 的情况,得到 %d", got)
	}
}

// TestFrecency_SaveAtomicTempSuffix 验证 temp 文件名带随机后缀(防多 worker 冲突)。
func TestFrecency_SaveAtomicTempSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frec.json")
	f, _ := LoadFrecency(path)
	if err := f.Record("/a"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := f.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// temp 文件应被 rename 成最终名,不应残留
	matches, _ := filepath.Glob(filepath.Join(dir, "frec.json.tmp.*"))
	if len(matches) != 0 {
		t.Errorf("temp 文件应被 rename 掉,仍存在: %v", matches)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("最终文件应存在: %v", err)
	}
}

// TestModel_SaveQueueHandshake 证明:
//  1. enqueueSave 非阻塞入队;
//  2. Init 同款 waitSave listener 取出第一条 saveCmd;
//  3. Update 执行 fn 并返回下一条 waitSave Cmd;
//  4. 第二条也按 FIFO 执行,且 Update 再次 re-arm listener。
//
// 测试只执行已知存在的两次 handoff;绝不在队列为空时调用 listener,
// 因而无需 sleep/timeout,也不会因第三次阻塞读而挂死。
func TestModel_SaveQueueHandshake(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	var order []int
	for _, id := range []int{1, 2} {
		id := id
		m.enqueueSave(func() error {
			order = append(order, id)
			return nil
		})
	}

	listener := waitSave(m.saveQueue) // 模拟 Init 挂入的首个 listener
	if listener == nil {
		t.Fatal("非 nil saveQueue 应返回 listener")
	}
	first := listener()
	if _, ok := first.(saveCmd); !ok {
		t.Fatalf("首个 listener 应返回 saveCmd,得到 %T", first)
	}
	updated, nextListener := m.Update(first)
	m = updated.(model)
	if nextListener == nil {
		t.Fatal("处理第一条 saveCmd 后必须 re-arm waitSave")
	}
	if got := append([]int(nil), order...); len(got) != 1 || got[0] != 1 {
		t.Fatalf("第一条应先执行,得到 %v", got)
	}

	second := nextListener() // queue 中已知有第二条,不会阻塞
	if _, ok := second.(saveCmd); !ok {
		t.Fatalf("re-arm listener 应返回第二条 saveCmd,得到 %T", second)
	}
	updated, rearmedAgain := m.Update(second)
	m = updated.(model)
	if rearmedAgain == nil {
		t.Fatal("处理第二条 saveCmd 后仍必须 re-arm waitSave")
	}
	// 不执行 rearmedAgain:此时 queue 为空,执行会按设计阻塞等待未来 writer。
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("两次 save 应按 FIFO 串行执行为 [1 2],得到 %v", order)
	}
}

// TestModel_FirstFrecencyRecordEnqueuesSave 证明第一次成功 fs 写入也走
// saveQueue,不是只更新内存后等待第二次操作。队列里已知有一条任务再读取,
// 无 sleep、无竞态窗口。
func TestModel_FirstFrecencyRecordEnqueuesSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frec.json")
	f, err := LoadFrecency(path)
	if err != nil {
		t.Fatalf("LoadFrecency: %v", err)
	}
	m := newModel(&fakeSession{}, "/proj")
	m.frecency = f
	m.saveQueue = make(chan saveCmd, 1)

	m.recordToolFrecency("fs_write", `{"path":"/proj/first.go"}`)
	listener := waitSave(m.saveQueue)
	msg := listener() // recordToolFrecency 必须已经入队,因此不会阻塞
	save, ok := msg.(saveCmd)
	if !ok || save.fn == nil {
		t.Fatalf("第一次 Record 应入队有效 saveCmd,得到 %T", msg)
	}
	if err := save.fn(); err != nil {
		t.Fatalf("执行第一次 frecency save: %v", err)
	}

	reloaded, err := LoadFrecency(path)
	if err != nil {
		t.Fatalf("重载第一次 save: %v", err)
	}
	got := reloaded.TopN(1)
	if len(got) != 1 || got[0] != "/proj/first.go" {
		t.Fatalf("第一次成功 fs 写入应持久化,得到 %v", got)
	}
}
