package main

import (
	"log"
	"sync"
	"time"
)

// statfs 遇到卡死的挂载（ntfs-3g 等 FUSE 进程挂掉、坏盘、内核 D 状态）会一直不返回。
// 磁盘容量是在 HTTP 上报循环和实时推送循环里同步读取的，一旦卡住，整个上报就停了：节点离线，
// 实时帧和保活 ping 也发不出去，收到 SIGTERM 也退不出来（要等 systemd 超时后强杀）。
// 所以每个挂载点的 statfs 都放到独立协程里，等不到结果就沿用上一次的数值；
// 那次调用还没返回时不再重复发起，也就不会因为反复尝试而堆积协程和线程。
const statTimeout = 3 * time.Second

type statResult struct {
	total, used uint64
	ok          bool
}

type statEntry struct {
	last    statResult
	have    bool          // last 是否有效（至少成功返回过一次）
	busy    chan struct{} // 非 nil：有一次 statfs 还没返回，返回时关闭
	hung    bool          // 上一次等待已经超时：之后的调用直接用缓存，不再等
	lastLog time.Time
}

type statGuard struct {
	mu      sync.Mutex
	entries map[string]*statEntry
	fn      func(path string) (total, used uint64, ok bool)
	timeout time.Duration
}

func newStatGuard(fn func(string) (uint64, uint64, bool), timeout time.Duration) *statGuard {
	return &statGuard{entries: map[string]*statEntry{}, fn: fn, timeout: timeout}
}

// diskStat 是磁盘统计实际使用的 statfs：带超时保护的 statfsBytes。
var diskStat = newStatGuard(statfsBytes, statTimeout).stat

func (g *statGuard) stat(path string) (total, used uint64, ok bool) {
	g.mu.Lock()
	e := g.entries[path]
	if e == nil {
		e = &statEntry{}
		g.entries[path] = e
	}
	done := e.busy
	if done != nil && e.hung { // 上一次调用已经卡住：直接用缓存的数值
		r, have := e.last, e.have
		g.mu.Unlock()
		return r.total, r.used, have && r.ok
	}
	if done == nil { // 没有进行中的调用：发起一次
		done = make(chan struct{})
		e.busy = done
		go func() {
			t, u, k := g.fn(path)
			g.mu.Lock()
			e.last, e.have = statResult{t, u, k}, true
			e.busy, e.hung = nil, false
			g.mu.Unlock()
			close(done)
		}()
	}
	g.mu.Unlock()

	timer := time.NewTimer(g.timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		g.mu.Lock()
		if e.busy == done { // 仍然没返回（可能刚好在这一刻返回了，那就不标记）
			e.hung = true
			if time.Since(e.lastLog) > time.Minute {
				e.lastLog = time.Now()
				log.Printf("读取磁盘 %s 的用量超过 %s 没有返回（挂载可能卡死），暂时沿用上一次的数值", path, g.timeout)
			}
		}
		g.mu.Unlock()
	}
	g.mu.Lock()
	r, have := e.last, e.have
	g.mu.Unlock()
	return r.total, r.used, have && r.ok
}
