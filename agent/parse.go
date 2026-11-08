package main

import (
	"bufio"
	"math"
	"path/filepath"
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

// excludedIfacePrefixes 是自动汇总时不计入流量的网卡名前缀，和 Komari Agent 的 loopbackNames 保持一致：
// 这些网卡（回环、容器、虚拟网桥、虚拟机的 tap 口等）上的流量，要么本来就会从物理网卡再走一遍（算进去就是重复计数，
// 例如 Proxmox 的 vmbr0 + 物理网卡、KVM 的 br0 + vnet/tap），要么根本不出本机。
// 前缀匹配，所以 "br" 会同时排除 br0、br-xxxx、bridge0。kube、cali 是本项目额外保留的（Kubernetes 的虚拟网卡，同样会重复计数）。
var excludedIfacePrefixes = []string{
	"lo", "br", "cni", "docker", "podman", "flannel", "veth", "virbr", "vmbr", "tap", "fwbr", "fwpr",
	"kube", "cali",
}

// ifaceExcluded 判断网卡是否属于回环或容器、虚拟网桥（自动汇总时不计入流量）。
func ifaceExcluded(name string) bool {
	for _, p := range excludedIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ifaceSelected 判断网卡是否在「统计网卡」白名单里；白名单支持 * ? 通配符（和 Komari 一致），例如 "eth*"。
// 管理员明确写出来的网卡不受自动排除规则限制。
func ifaceSelected(name string, only map[string]bool) bool {
	if only[name] {
		return true
	}
	for pat := range only {
		if ok, err := filepath.Match(pat, name); err == nil && ok {
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

// ifCounters 是一块网卡自开机（或自创建）以来的累计接收、发送字节数。
type ifCounters struct {
	RX uint64 `json:"rx"`
	TX uint64 `json:"tx"`
}

// parseNetDev 解析 /proc/net/dev，返回累计接收和发送字节数（所有统计网卡之和）。only 非空时只统计指定网卡。
func parseNetDev(text string, only map[string]bool) (rx, tx uint64) {
	for _, c := range parseNetDevIfaces(text, only) {
		rx += c.RX
		tx += c.TX
	}
	return rx, tx
}

// parseNetDevIfaces 解析 /proc/net/dev，返回每块统计网卡各自的累计值。
// 累计流量必须按网卡分别算增量：只看合计值的话，一块网卡消失（虚拟机、容器、VPN 隧道停止）时合计值会变小，
// 会被误当成「计数器被重置」，把剩下所有网卡的整个累计值当成新增流量。
func parseNetDevIfaces(text string, only map[string]bool) map[string]ifCounters {
	out := map[string]ifCounters{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if len(only) > 0 {
			if !ifaceSelected(name, only) {
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
		out[name] = ifCounters{RX: r, TX: t}
	}
	return out
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

// osReleaseValue 取 /etc/os-release 里某个键的值（去掉引号）。
func osReleaseValue(text, key string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, key+"=") {
			return strings.Trim(strings.TrimPrefix(line, key+"="), `"'`)
		}
	}
	return ""
}

// isDottedVersion 判断是不是 13、13.1 这样的纯数字版本号（testing、sid 的 debian_version 是 trixie/sid，不算）。
func isDottedVersion(v string) bool {
	if v == "" || v[0] < '0' || v[0] > '9' || v[len(v)-1] == '.' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return !strings.Contains(v, "..")
}

// osDisplayName 生成要显示的系统名称。Debian 的 PRETTY_NAME 只有大版本（Debian GNU/Linux 13 (trixie)），
// 小版本在 /etc/debian_version 里，所以 Debian 显示为「Debian 13.1」（不带代号）；
// 其他发行版、或 debian_version 不是数字版本号时，仍用 PRETTY_NAME。
func osDisplayName(osRelease, debianVersion string) string {
	pretty := osPrettyName(osRelease)
	if osReleaseValue(osRelease, "ID") != "debian" {
		return pretty
	}
	v := strings.TrimSpace(debianVersion)
	if !isDottedVersion(v) {
		return pretty
	}
	return "Debian " + v
}

// cpuModel 取 CPU 型号。x86 的 /proc/cpuinfo 有 "model name"；ARM 开发板（Armbian、树莓派等）没有，
// 依次退而求其次：Hardware（SoC 名）、Processor（32 位 ARM）、Model（开发板型号）。
func cpuModel(text string) string {
	want := []string{"model name", "hardware", "processor", "model"}
	found := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.TrimSpace(line[i+1:])
		// "processor : 0" 是 x86 / arm64 里的核心编号，不是型号；32 位 ARM 的 "Processor : ARMv7 Processor rev 4 (v7l)" 才是
		if key == "processor" && isAllDigits(val) {
			continue
		}
		if _, ok := found[key]; !ok && val != "" {
			found[key] = val
		}
	}
	for _, k := range want {
		if v, ok := found[k]; ok {
			return v
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// mountEntry 是 /proc/self/mounts 里的一行：设备、挂载点、文件系统类型。
type mountEntry struct {
	Device, Point, FSType string
}

// unescapeMount 还原 /proc/self/mounts 里的八进制转义，例如挂载点里的空格会写成 \040。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseMounts(text string) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		out = append(out, mountEntry{Device: unescapeMount(f[0]), Point: unescapeMount(f[1]), FSType: f[2]})
	}
	return out
}

// localDiskFS 是会被计入磁盘容量的「本机磁盘」文件系统类型。
// 网络文件系统（nfs、cifs、smb3、sshfs、9p、ceph、glusterfs 等）、虚拟文件系统（proc、sysfs、tmpfs、overlay 等）
// 和只读镜像（squashfs、iso9660）都不在其中。
var localDiskFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"jfs": true, "reiserfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
}

// isLocalDiskMount 判断一个挂载是否是本机的块设备磁盘：文件系统类型要在白名单里，
// 设备必须是 /dev/ 下的真实块设备；loop（snap 等镜像）、ram、zram 和网络块设备 nbd 不算。
func isLocalDiskMount(m mountEntry) bool {
	if !localDiskFS[m.FSType] || !strings.HasPrefix(m.Device, "/dev/") {
		return false
	}
	base := strings.TrimPrefix(m.Device, "/dev/")
	for _, p := range []string{"loop", "ram", "zram", "nbd"} {
		if strings.HasPrefix(base, p) {
			return false
		}
	}
	return true
}

// isZFSMount 判断是不是 ZFS 数据集的挂载（设备名是「存储池/数据集」，不在 /dev/ 下）。
func isZFSMount(m mountEntry) bool {
	return m.FSType == "zfs" && m.Device != "" && !strings.HasPrefix(m.Device, "/")
}

// diskKey 是「同一块磁盘」的标识，用来去重：ZFS 的多个数据集共用一个存储池，按池名（设备名里第一个 / 之前的部分）算一块；
// 其他文件系统按设备算（bind 挂载、btrfs 子卷、systemd 的只读保护会让同一个设备出现多次）。
func diskKey(m mountEntry) string {
	if m.FSType == "zfs" {
		if i := strings.IndexByte(m.Device, '/'); i > 0 {
			return "zfs:" + m.Device[:i]
		}
		return "zfs:" + m.Device
	}
	return m.Device
}

// selectDiskMounts 挑出要统计的挂载点：
//   - 本机块设备（见 isLocalDiskMount）和 ZFS 数据集；
//   - 根目录 / 始终算上（和 Komari 一致）：LXC 等容器的根文件系统可能是 loop 设备、ZFS 子卷甚至 overlay，不在上面的规则里，
//     不算的话磁盘会显示成 0。旧内核的 /proc/self/mounts 里可能先出现一行 rootfs 再出现真正的根，所以只取最后一个 /；
//   - 同一个设备被挂载多次只取第一次出现的，避免重复累加。ZFS 的数据集全部保留，由 sumDisks 按存储池取容量最大的那个。
func selectDiskMounts(entries []mountEntry) []mountEntry {
	lastRoot := -1
	for i, m := range entries {
		if m.Point == "/" {
			lastRoot = i
		}
	}
	seen := map[string]bool{}
	var out []mountEntry
	for i, m := range entries {
		if m.Point == "/" && i != lastRoot {
			continue
		}
		if !(isLocalDiskMount(m) || isZFSMount(m) || i == lastRoot) {
			continue
		}
		if m.FSType != "zfs" {
			k := diskKey(m)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, m)
	}
	return out
}

// sumDisks 把各块磁盘的容量和已用量累加，返回 MB。stat 返回字节数。
// 同一块磁盘（见 diskKey）出现多次时只算容量最大的那一个，和 Komari 一致：ZFS 一个存储池下的多个数据集就只算一份，不会重复累加。
func sumDisks(mounts []mountEntry, stat func(path string) (total, used uint64, ok bool)) (totalMB, usedMB int64) {
	type usage struct{ total, used uint64 }
	best := map[string]usage{}
	for _, m := range mounts {
		tt, uu, ok := stat(m.Point)
		if !ok {
			continue
		}
		k := diskKey(m)
		if cur, seen := best[k]; !seen || tt > cur.total {
			best[k] = usage{tt, uu}
		}
	}
	var t, u uint64
	for _, v := range best {
		t += v.total
		u += v.used
	}
	return int64(t >> 20), int64(u >> 20)
}
