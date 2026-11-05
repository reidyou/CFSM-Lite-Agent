package main

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type staticInfo struct {
	OS, Arch, Kernel, CPUInfo string
	Cores                     int
}

var (
	staticOnce sync.Once
	staticVal  staticInfo
)

func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// loadStatic 读取不会变化的主机信息，只读一次。
func loadStatic() staticInfo {
	staticOnce.Do(func() {
		osName := osPrettyName(readFileString("/etc/os-release"))
		if osName == "" {
			osName = "Linux"
		}
		arch := runtime.GOARCH
		switch arch {
		case "amd64":
			arch = "x86_64"
		case "arm64":
			arch = "aarch64"
		}
		cpu := cpuModel(readFileString("/proc/cpuinfo"))
		if cpu == "" {
			cpu = runtime.GOARCH + " CPU"
		}
		staticVal = staticInfo{
			OS:      osName,
			Arch:    arch,
			Kernel:  strings.TrimSpace(readFileString("/proc/sys/kernel/osrelease")),
			CPUInfo: cpu,
			Cores:   runtime.NumCPU(),
		}
	})
	return staticVal
}

type snapshot struct {
	at                time.Time
	cpuTotal, cpuIdle uint64
	btime             int64
	rx, tx            uint64
}

func readSnapshot(only map[string]bool) snapshot {
	total, idle, bt := parseProcStat(readFileString("/proc/stat"))
	rx, tx := parseNetDev(readFileString("/proc/net/dev"), only)
	return snapshot{at: time.Now(), cpuTotal: total, cpuIdle: idle, btime: bt, rx: rx, tx: tx}
}

func countProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n
}

// reachable 用 UDP「连接」判断本机是否有到公网的 IPv4 / IPv6 路由（不会真的发送数据包）。
func reachable(network, addr string) string {
	c, err := net.DialTimeout(network, addr, 1500*time.Millisecond)
	if err != nil {
		return "0"
	}
	_ = c.Close()
	return "1"
}

// diskTotals 统计本机所有磁盘的总容量和已用量（MB）：只算本地块设备，网络挂载和虚拟文件系统不计入，
// 同一块设备被挂载多次只算一次。如果一个都没识别出来（例如某些容器环境），退回到只统计根目录。
func diskTotals() (totalMB, usedMB int64) {
	mounts := selectDiskMounts(parseMounts(readFileString("/proc/self/mounts")))
	totalMB, usedMB = sumDisks(mounts, statfsBytes)
	if totalMB == 0 {
		return sumDisks([]mountEntry{{Device: "/", Point: "/", FSType: "root"}}, statfsBytes)
	}
	return totalMB, usedMB
}

// sharedData 在 HTTP 上报循环和实时推送协程之间共享最近一次的 Ping 结果。
type sharedData struct {
	mu   sync.Mutex
	ping map[string]any
}

func (s *sharedData) setPing(m map[string]any) {
	s.mu.Lock()
	s.ping = m
	s.mu.Unlock()
}

// getPing 返回最近一次 Ping 结果；返回的 map 只能读取，不能修改。
func (s *sharedData) getPing() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ping
}

// readHeavy 读取变化慢、读取成本相对高的字段：磁盘、负载、进程与连接数、IP 可用性、系统信息。
func readHeavy() map[string]any {
	diskTotal, diskUsed := diskTotals()
	info := loadStatic()
	tcp := countSockets(readFileString("/proc/net/tcp"), true) + countSockets(readFileString("/proc/net/tcp6"), true)
	udp := countSockets(readFileString("/proc/net/udp"), false) + countSockets(readFileString("/proc/net/udp6"), false)
	return map[string]any{
		"disk_total":     diskTotal,
		"disk_used":      diskUsed,
		"load_avg":       parseLoadavg(readFileString("/proc/loadavg")),
		"os":             info.OS,
		"arch":           info.Arch,
		"kernel_version": info.Kernel,
		"cpu_info":       info.CPUInfo,
		"cpu_cores":      info.Cores,
		"processes":      countProcesses(),
		"tcp_conn":       tcp,
		"udp_conn":       udp,
		"ip_v4":          reachable("udp4", "1.1.1.1:53"),
		"ip_v6":          reachable("udp6", "[2606:4700:4700::1111]:53"),
	}
}

// buildMetrics 用两次读数（a 到 b）的差值算出 CPU 占用和网速，再加上变化慢的字段和 Ping 结果，
// 组成一份完整的指标。Worker 和推送中心都按「完整指标」处理，所以实时帧也必须带上所有字段。
func buildMetrics(a, b snapshot, cfg config, st *state, heavy, ping map[string]any) map[string]any {
	secs := b.at.Sub(a.at).Seconds()
	st.advance(b.rx, b.tx, b.at, cfg.ResetDay, cfg.Interface)
	rxMonthly, txMonthly := st.monthly()
	mem := parseMeminfo(readFileString("/proc/meminfo"))
	m := map[string]any{
		"cpu":            cpuPercent(a.cpuTotal, a.cpuIdle, b.cpuTotal, b.cpuIdle),
		"ram_total":      mem.RAMTotal,
		"ram_used":       mem.RAMUsed,
		"swap_total":     mem.SwapTotal,
		"swap_used":      mem.SwapUsed,
		"boot_time":      b.btime * 1000,
		"net_rx":         b.rx,
		"net_tx":         b.tx,
		"net_rx_monthly": rxMonthly,
		"net_tx_monthly": txMonthly,
		"net_in_speed":   ratePerSec(b.rx, a.rx, secs),
		"net_out_speed":  ratePerSec(b.tx, a.tx, secs),
	}
	for key, v := range heavy {
		m[key] = v
	}
	for key, v := range ping {
		m[key] = v
	}
	return m
}

// collectMetrics 采集一轮指标（HTTP 上报用）：Ping 探测与 1 秒的 CPU、网速采样同时进行，一轮大约耗时 1 到 8 秒。
func collectMetrics(ctx context.Context, cfg config, st *state, sh *sharedData) map[string]any {
	pingCh := startPing(ctx, cfg)
	only := splitIfaces(cfg.Interface)
	a := readSnapshot(only)
	sleepCtx(ctx, time.Second)
	b := readSnapshot(only)
	heavy := readHeavy()
	ping := <-pingCh
	sh.setPing(ping)
	return buildMetrics(a, b, cfg, st, heavy, ping)
}

// fastSampler 用于实时推送：不睡眠，直接用「上一帧到这一帧」的差值算 CPU 和网速；
// 变化慢的字段每 10 秒才重新读取一次。
type fastSampler struct {
	prev    snapshot
	iface   string
	heavy   map[string]any
	heavyAt time.Time
}

func (f *fastSampler) reset(cfg config) {
	f.prev = readSnapshot(splitIfaces(cfg.Interface))
	f.iface = cfg.Interface
}

func (f *fastSampler) next(cfg config, st *state, sh *sharedData) map[string]any {
	b := readSnapshot(splitIfaces(cfg.Interface))
	if f.iface != cfg.Interface { // 统计网卡变了：重新建立基线，避免算出离谱的速度
		f.prev = b
		f.iface = cfg.Interface
	}
	if f.heavy == nil || time.Since(f.heavyAt) > 10*time.Second {
		f.heavy = readHeavy()
		f.heavyAt = time.Now()
	}
	m := buildMetrics(f.prev, b, cfg, st, f.heavy, sh.getPing())
	f.prev = b
	return m
}
