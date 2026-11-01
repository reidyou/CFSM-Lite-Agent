package main

import (
	"bufio"
	"math"
	"strconv"
	"strings"
)

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// parseProcStat 解析 /proc/stat：返回 CPU 总时间、空闲时间（idle+iowait）和开机时间（Unix 秒）。
func parseProcStat(text string) (total, idle uint64, btime int64) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "cpu":
			var vals []uint64
			for _, x := range f[1:] {
				n, err := strconv.ParseUint(x, 10, 64)
				if err != nil {
					break
				}
				vals = append(vals, n)
			}
			// 前 8 项依次是 user nice system idle iowait irq softirq steal（guest 已包含在 user 里）
			for i := 0; i < len(vals) && i < 8; i++ {
				total += vals[i]
			}
			if len(vals) >= 5 {
				idle = vals[3] + vals[4]
			} else if len(vals) >= 4 {
				idle = vals[3]
			}
		case f[0] == "btime" && len(f) >= 2:
			btime, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	return total, idle, btime
}

// cpuPercent 根据两次 /proc/stat 的差值计算 CPU 占用百分比，保留 2 位小数。
func cpuPercent(totalA, idleA, totalB, idleB uint64) float64 {
	if totalB <= totalA {
		return 0
	}
	dt := float64(totalB - totalA)
	di := 0.0
	if idleB > idleA {
		di = float64(idleB - idleA)
	}
	p := (1 - di/dt) * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return round2(p)
}

// ratePerSec 计算字节速率（B/s）；计数器回绕或网卡重置时返回 0，绝不返回负数。
func ratePerSec(cur, last uint64, secs float64) float64 {
	if secs <= 0 || cur < last {
		return 0
	}
	return math.Round(float64(cur-last) / secs)
}

type memInfo struct {
	RAMTotal, RAMUsed, SwapTotal, SwapUsed int64 // 单位 MB
}

// parseMeminfo 解析 /proc/meminfo。已用内存 = 总量 - MemAvailable（老内核没有该项时用 Free+Buffers+Cached 估算）。
func parseMeminfo(text string) memInfo {
	kv := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		kv[line[:i]] = n // 单位 kB
	}
	avail, ok := kv["MemAvailable"]
	if !ok {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	used := kv["MemTotal"] - avail
	if used < 0 {
		used = 0
	}
	swapUsed := kv["SwapTotal"] - kv["SwapFree"]
	if swapUsed < 0 {
		swapUsed = 0
	}
	return memInfo{
		RAMTotal:  kv["MemTotal"] / 1024,
		RAMUsed:   used / 1024,
		SwapTotal: kv["SwapTotal"] / 1024,
		SwapUsed:  swapUsed / 1024,
	}
}

// ifaceExcluded 判断网卡是否属于回环或容器、虚拟网桥（自动汇总时不计入流量）。
func ifaceExcluded(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"docker", "veth", "br-", "virbr", "cni", "flannel", "kube", "cali"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// splitIfaces 把 "eth0,ens3" 拆成集合；空字符串返回空集合（表示自动汇总）。
func splitIfaces(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			m[p] = true
		}
	}
	return m
}

// parseNetDev 解析 /proc/net/dev，返回累计接收和发送字节数。only 非空时只统计指定网卡。
func parseNetDev(text string, only map[string]bool) (rx, tx uint64) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if len(only) > 0 {
			if !only[name] {
				continue
			}
		} else if ifaceExcluded(name) {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return rx, tx
}

func parseLoadavg(text string) string {
	f := strings.Fields(text)
	if len(f) < 3 {
		return "0 0 0"
	}
	return strings.Join(f[:3], " ")
}

// countSockets 统计 /proc/net/{tcp,udp}[6] 里的条目数；establishedOnly 为真时只数 ESTABLISHED（状态码 01）。
func countSockets(text string, establishedOnly bool) int {
	n := 0
	first := true
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if first {
			first = false // 表头
			continue
		}
		f := strings.Fields(sc.Text())
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

func osPrettyName(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"'`)
		}
	}
	return ""
}

func cpuModel(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.ToLower(line), "model name") {
			if i := strings.IndexByte(line, ':'); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}
