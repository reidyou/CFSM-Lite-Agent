package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 状态文件：并发保存、损坏恢复 ----

func TestStateSaveConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &state{Period: "2026-11-01", RX: 1}
	var wg sync.WaitGroup
	var failed atomic.Int32
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				stMu.Lock()
				st.RX = uint64(g*1000 + i)
				stMu.Unlock()
				if err := st.save(path); err != nil {
					failed.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Fatalf("并发保存出现 %d 次失败", n)
	}
	if got := loadState(path); got.Period != "2026-11-01" {
		t.Fatalf("最终文件无法读取: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("临时文件没有被清理: %v", err)
	}
}

func TestLoadStateRecoversFromBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	good := &state{Period: "2026-11-01", RX: 111, TX: 222, Init: true}
	if err := good.save(path); err != nil {
		t.Fatal(err)
	}
	// 第二次保存前把备份视为已过期，让它刷新成「第一次的内容」
	old := time.Now().Add(-2 * bakRefresh)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	good.RX = 333
	if err := good.save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("应该已经生成备份: %v", err)
	}
	// 模拟断电后主文件被截断
	if err := os.WriteFile(path, []byte(`{"period":"2026-11-01","rx":3`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadState(path)
	if got.RX != 111 || got.TX != 222 {
		t.Fatalf("应从备份恢复（RX=111 TX=222），得到 RX=%d TX=%d", got.RX, got.TX)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("损坏的文件应保留为 .corrupt: %v", err)
	}
}

func TestLoadStateCorruptWithoutBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil { // 断电后常见的空文件
		t.Fatal(err)
	}
	got := loadState(path)
	if got.Init || got.RX != 0 {
		t.Fatalf("没有备份时应从空状态开始: %+v", got)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("损坏的文件应保留为 .corrupt: %v", err)
	}
}

func TestBackupNotOverwrittenByCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	good := &state{Period: "p1", RX: 5}
	if err := good.save(path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * bakRefresh)
	_ = os.Chtimes(path, old, old)
	good.RX = 6
	if err := good.save(path); err != nil { // 生成备份（内容 RX=5）
		t.Fatal(err)
	}
	_ = os.Chtimes(path+".bak", old, old)
	_ = os.WriteFile(path, []byte("garbage"), 0o600) // 主文件损坏
	good.RX = 7
	if err := good.save(path); err != nil { // 备份已过期，但不能用损坏的主文件去覆盖它
		t.Fatal(err)
	}
	bak, _, err := readStateFile(path + ".bak")
	if err != nil || bak.RX != 5 {
		t.Fatalf("备份应保持完好（RX=5）: %+v %v", bak, err)
	}
}

func TestLoadStateMissingIsFresh(t *testing.T) {
	if got := loadState(filepath.Join(t.TempDir(), "none.json")); got.Init || got.RX != 0 {
		t.Fatalf("全新安装应得到空状态: %+v", got)
	}
}

// ---- 磁盘 statfs 超时保护 ----

func TestStatGuardHungMount(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func(path string) (uint64, uint64, bool) {
		n := calls.Add(1)
		if path == "/hung" && n > 1 { // 第一次正常，之后卡死
			<-release
		}
		return 100 << 20, 40 << 20, true
	}
	g := newStatGuard(fn, 60*time.Millisecond)

	if tot, used, ok := g.stat("/hung"); !ok || tot != 100<<20 || used != 40<<20 {
		t.Fatalf("第一次应正常返回: %d %d %v", tot, used, ok)
	}
	start := time.Now()
	tot, _, ok := g.stat("/hung") // 卡死：等到超时后沿用上一次的数值
	if !ok || tot != 100<<20 {
		t.Fatalf("超时后应沿用上次的数值: %d %v", tot, ok)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > time.Second {
		t.Fatalf("应当等待约一个超时时间，实际 %s", d)
	}
	before := calls.Load()
	start = time.Now()
	for i := 0; i < 5; i++ { // 已标记卡死：立刻返回，也不再发起新的调用
		g.stat("/hung")
	}
	if time.Since(start) > 30*time.Millisecond {
		t.Fatal("卡死之后的调用应立即返回")
	}
	if calls.Load() != before {
		t.Fatal("卡死期间不应继续发起新的 statfs")
	}
	// 另一个挂载点不受影响
	if _, _, ok := g.stat("/ok"); !ok {
		t.Fatal("其他挂载点应正常")
	}
	close(release) // 卡死的调用返回后恢复正常
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		busy := g.entries["/hung"].busy
		g.mu.Unlock()
		if busy == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, ok := g.stat("/hung"); !ok {
		t.Fatal("恢复后应正常返回")
	}
}

func TestStatGuardHungFromStartIsSkipped(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	g := newStatGuard(func(string) (uint64, uint64, bool) { <-release; return 1, 1, true }, 30*time.Millisecond)
	if _, _, ok := g.stat("/x"); ok {
		t.Fatal("从未成功过的卡死挂载应被跳过（ok=false）")
	}
}

// ---- 连接数流式统计：与旧实现逐行对照 ----

// countSocketsRef 是旧实现（按行 strings.Fields），作为对照基准。
func countSocketsRef(text string, establishedOnly bool) int {
	n, first := 0, true
	for _, line := range strings.Split(text, "\n") {
		if first {
			first = false
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if establishedOnly && f[3] != "01" {
			continue
		}
		n++
	}
	return n
}

func TestCountSocketsMatchesReference(t *testing.T) {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	states := []string{"01", "0A", "06", "02", "01"}
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "%4d: 0100007F:%04X 0100007F:%04X %s 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000000000000000 100 0 0 10 0\n", i, i&0xffff, (i*7)&0xffff, states[i%len(states)], i)
	}
	base := b.String()
	cases := map[string]string{
		"大文件":       base,
		"无结尾换行":     strings.TrimRight(base, "\n"),
		"只有表头":      "  sl  local_address rem_address   st\n",
		"空":         "",
		"空行与短行":     "header\n\n   \nonly three fields\n1: a b 01\n2: a b 0A c\n",
		"制表符分隔":     "h\n1:\ta\tb\t01\tx\n",
		"CRLF":      "h\r\n1: a b 01 x\r\n2: a b 0A x\r\n",
		"第4字段是 010": "h\n1: a b 010 x\n2: a b 01 x\n",
	}
	for name, text := range cases {
		for _, est := range []bool{true, false} {
			if got, want := countSockets(text, est), countSocketsRef(text, est); got != want {
				t.Errorf("%s est=%v: 流式=%d 旧实现=%d", name, est, got, want)
			}
		}
	}
}

func TestCountSocketsOverlongLine(t *testing.T) {
	// 超过读缓冲的异常长行被忽略，不影响前后正常行的计数，也不会死循环
	text := "h\n1: a b 01 x\n" + "2: a b 01 " + strings.Repeat("z", 20000) + "\n3: a b 01 x\n"
	if got := countSockets(text, true); got != 2 {
		t.Fatalf("得到 %d，期望 2（超长行被忽略）", got)
	}
}

func TestCountSocketsFileMissing(t *testing.T) {
	if n := countSocketsFile(filepath.Join(t.TempDir(), "nope"), true, nil); n != 0 {
		t.Fatalf("文件不存在应返回 0，得到 %d", n)
	}
}

// ---- 重连抖动 ----

func TestRtJitterRange(t *testing.T) {
	base := 100 * time.Second
	lo, hi := base, base
	for i := 0; i < 2000; i++ {
		d := rtJitter(base)
		if d < 80*time.Second || d > 120*time.Second {
			t.Fatalf("抖动超出 ±20%%: %s", d)
		}
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if hi-lo < 20*time.Second {
		t.Fatalf("抖动几乎没有随机性: 最小 %s 最大 %s", lo, hi)
	}
}
