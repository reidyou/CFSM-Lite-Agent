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

// collectMetrics 采集一轮指标。Ping 探测与 1 秒的 CPU、网速采样同时进行，所以一轮大约耗时 1 到 8 秒。
func collectMetrics(ctx context.Context, cfg config, st *state) map[string]any {
	pingCh := startPing(ctx, cfg)
	only := splitIfaces(cfg.Interface)

	a := readSnapshot(only)
	sleepCtx(ctx, time.Second)
	b := readSnapshot(only)
	secs := b.at.Sub(a.at).Seconds()

	st.advance(b.rx, b.tx, b.at, cfg.ResetDay, cfg.Interface)

	mem := parseMeminfo(readFileString("/proc/meminfo"))
	diskTotal, diskUsed := diskUsage("/")
	info := loadStatic()

	tcp := countSockets(readFileString("/proc/net/tcp"), true) + countSockets(readFileString("/proc/net/tcp6"), true)
	udp := countSockets(readFileString("/proc/net/udp"), false) + countSockets(readFileString("/proc/net/udp6"), false)

	m := map[string]any{
		"cpu":            cpuPercent(a.cpuTotal, a.cpuIdle, b.cpuTotal, b.cpuIdle),
		"ram_total":      mem.RAMTotal,
		"ram_used":       mem.RAMUsed,
		"swap_total":     mem.SwapTotal,
		"swap_used":      mem.SwapUsed,
		"disk_total":     diskTotal,
		"disk_used":      diskUsed,
		"load_avg":       parseLoadavg(readFileString("/proc/loadavg")),
		"boot_time":      b.btime * 1000,
		"net_rx":         b.rx,
		"net_tx":         b.tx,
		"net_rx_monthly": st.RX,
		"net_tx_monthly": st.TX,
		"net_in_speed":   ratePerSec(b.rx, a.rx, secs),
		"net_out_speed":  ratePerSec(b.tx, a.tx, secs),
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
	for k, v := range <-pingCh {
		m[k] = v
	}
	return m
}
