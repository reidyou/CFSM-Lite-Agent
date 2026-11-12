package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stMu 保护 state：HTTP 上报循环和实时推送协程都会更新当月累计流量。
var stMu sync.Mutex

// saveMu 让「生成快照 → 写文件」整体串行：主循环、saveLoop 和退出时的保存可能同时发生，
// 不串行的话它们共用同一个临时文件会互相覆盖，较旧的快照还可能在较新的之后才落盘。
var saveMu sync.Mutex

// bakRefresh 是备份文件的最小更新间隔：.bak 只在主文件损坏时兜底，不必每次保存都更新，省一次目录写入。
const bakRefresh = 10 * time.Minute

// state 保存「当月累计流量」所需的最少信息，写在本地文件里，Agent 重启后可以接着累计。
type state struct {
	Period string `json:"period"`  // 当前统计周期的起点，例如 2026-10-01；重置时刻不是 0:00 时带上时刻，例如 2026-10-01T08:30
	Iface  string `json:"iface"`   // 统计所用的网卡配置；变更后重新建立基线
	RX     uint64 `json:"rx"`      // 本周期累计下行字节
	TX     uint64 `json:"tx"`      // 本周期累计上行字节
	LastRX uint64 `json:"last_rx"` // 上次读到的所有统计网卡的合计值
	LastTX uint64 `json:"last_tx"`
	// 上次读到的每块网卡各自的累计值。增量按网卡分别算，网卡增减（虚拟机、容器、VPN 隧道起停）才不会算出巨大的假流量。
	Ifs   map[string]ifCounters `json:"ifs,omitempty"`
	Init  bool                  `json:"init"`
	CalID string                `json:"cal_id"` // 最近一次应用的手动校准编号，避免同一次校准重复应用
	Cfg   string                `json:"cfg"`    // 最近一次应用的服务端配置原文；重启后先用它，不用默认配置，否则重置日、时区、网卡不同的节点会被误清零

	lastAt time.Time // 上一次累计所用读数的采集时间（只在内存里，用来丢弃过期的读数）
	dirty  bool      // 内存里有尚未写入文件的变化（只在内存里）
}

// readStateFile 读取并解析一个状态文件；文件不存在时 missing 为 true（不算损坏）。
func readStateFile(path string) (st *state, missing bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, os.IsNotExist(err), err
	}
	st = &state{}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, false, err
	}
	return st, false, nil
}

// loadState 读取状态文件。主文件损坏（断电留下的空文件或截断文件）时：记一条日志，先尝试 .bak（最多落后约 10 分钟的流量），
// 都不行才从空状态开始，并把坏文件改名为 .corrupt 留作排查。以前这里是静默清零，当月流量丢了也没有任何痕迹。
func loadState(path string) *state {
	if path == "" {
		return &state{}
	}
	st, missing, err := readStateFile(path)
	if err == nil {
		return st
	}
	if missing {
		// 主文件不存在：多半是全新安装；若上次恰好在换文件的瞬间被中断，.bak 还在，取回它
		if bak, _, berr := readStateFile(path + ".bak"); berr == nil {
			log.Printf("状态文件不存在，已从备份 %s.bak 恢复", path)
			return bak
		}
		return &state{}
	}
	log.Printf("状态文件 %s 损坏（%v），尝试从备份恢复", path, err)
	if bak, _, berr := readStateFile(path + ".bak"); berr == nil {
		_ = os.Rename(path, path+".corrupt")
		log.Printf("已从备份 %s.bak 恢复，最近一小段流量累计可能丢失", path)
		return bak
	}
	_ = os.Rename(path, path+".corrupt")
	log.Printf("没有可用的备份，当月流量累计从 0 重新开始；损坏的文件已保留为 %s.corrupt", path)
	return &state{}
}

// save 把状态写入文件：整个过程串行；先写同目录的临时文件并 fsync，再改名替换，断电也不会留下空文件或半截文件。
func (s *state) save(path string) error {
	if path == "" {
		return nil
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	stMu.Lock()
	b, err := json.Marshal(s)
	s.dirty = false
	stMu.Unlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, b); err != nil {
		stMu.Lock()
		s.dirty = true // 没写成功，下次还要再写
		stMu.Unlock()
		return err
	}
	return nil
}

// writeFileAtomic 先写临时文件并 fsync，再（必要时）把当前主文件硬链接成 .bak，最后改名替换主文件。
// 调用方必须已经持有 saveMu（临时文件名固定）。
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	refreshBackup(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// refreshBackup 在备份不存在或超过 bakRefresh 没更新时，把当前（仍然完好的）主文件硬链接为 .bak。
// 硬链接只改目录项、不复制数据；文件系统不支持时静默跳过，备份只是兜底，失败不影响保存。
func refreshBackup(path string) {
	bak := path + ".bak"
	if fi, err := os.Stat(bak); err == nil && time.Since(fi.ModTime()) < bakRefresh {
		return
	}
	if _, _, err := readStateFile(path); err != nil {
		return // 当前主文件不存在或已损坏，不能拿它覆盖备份
	}
	_ = os.Remove(bak)
	_ = os.Link(path, bak)
}

// syncDir 尽力把目录项落盘（改名本身要靠它才能抵御断电）；失败不影响功能。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// isDirty 返回内存里的状态是否比文件新。
func (s *state) isDirty() bool {
	stMu.Lock()
	defer stMu.Unlock()
	return s.dirty
}

// setCfg 记下最近一次应用的服务端配置原文，随状态文件一起保存。
func (s *state) setCfg(body string) {
	stMu.Lock()
	defer stMu.Unlock()
	if s.Cfg != body {
		s.Cfg = body
		s.dirty = true
	}
}

// cachedCfg 返回上次保存的服务端配置原文；没有则为空串。
func (s *state) cachedCfg() string {
	stMu.Lock()
	defer stMu.Unlock()
	return s.Cfg
}

// saveLoop 定时把有变化的状态写入文件。实时推送协程也会累计流量但不写文件，
// 没有它的话，重启（尤其是升级时重新安装）会丢掉最近一个上报间隔内的流量。
func saveLoop(ctx context.Context, s *state, path string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.isDirty() {
				_ = s.save(path)
			}
		}
	}
}

// delta 计算两次读数之间的增量。当前值小于上次说明计数器被重置（重启、网卡重建）或读数异常，这一段按 0 处理、只重建基线，
// 和 Komari Agent 的 safeDelta 一致。曾经把「整个当前值」当作新增量，计数器每被重置一次（尤其是网卡重建、读数抖动）就会凭空多出
// 一整个计数器的流量，这是当月流量偏大的主要来源。代价只是丢掉重置到下一次读数之间的那一小段真实流量，宁可略少、不能虚高。
func delta(cur, last uint64) uint64 {
	if cur >= last {
		return cur - last
	}
	return 0
}

// resetRule 描述流量何时重置：每月 Day 日的 Hour:Minute（按 Loc 时区）；Day 为 0 表示不重置。
type resetRule struct {
	Day, Hour, Minute int
	Loc               *time.Location
	Skew              time.Duration // 时间校准量（Worker 时间 − 本机时间），划分周期时先加上它；0 表示不校准
}

// rule 取出配置里的重置规则。
func (c config) rule() resetRule {
	return resetRule{Day: c.ResetDay, Hour: c.ResetHour, Minute: c.ResetMinute, Loc: c.Loc}
}

// advance 用最新的网卡累计值更新当月累计流量。
// now 是这组读数的采集时间。HTTP 上报循环采完读数后还要等 Ping 结束才会调用这里，期间实时推送协程可能已经用更新的读数累计过了；
// 这时再用较早的读数，会被当成「计数器被重置」而把整个网卡累计值加进当月流量，所以比上次更早的读数直接丢弃。
// ifs 是每块统计网卡各自的原始累计值；为空（读取 /proc/net/dev 失败）时直接忽略这次读数，
// 否则会被当成「所有计数器归零」，下一次正常读数就会把整个累计值加进当月流量。
func (s *state) advance(ifs map[string]ifCounters, now time.Time, rule resetRule, iface string) {
	if len(ifs) == 0 {
		return
	}
	var rawRX, rawTX uint64
	for _, c := range ifs {
		rawRX += c.RX
		rawTX += c.TX
	}
	stMu.Lock()
	defer stMu.Unlock()
	if !s.lastAt.IsZero() && now.Before(s.lastAt) {
		return
	}
	s.lastAt = now
	if s.Iface != iface {
		s.Iface = iface
		s.Init = false
	}
	if key := periodKey(now, rule); key != s.Period {
		s.Period = key
		s.RX, s.TX = 0, 0
	}
	if s.Init {
		for name, c := range ifs {
			if last, ok := s.Ifs[name]; ok {
				s.RX += delta(c.RX, last.RX)
				s.TX += delta(c.TX, last.TX)
			}
			// 上次读数之后才出现的网卡（含读数抖动后重新出现的）：这次只记下基线，不把它的累计值当作新增流量，
			// 和 Komari Agent 一致（lastCounters 里没有的网卡首次采样不记录）。
		}
		// 上次有、这次没有的网卡直接忽略：它的流量已经累计过了，合计值变小不是流量减少
	}
	s.LastRX, s.LastTX, s.Init = rawRX, rawTX, true
	s.Ifs = make(map[string]ifCounters, len(ifs))
	for name, c := range ifs {
		s.Ifs[name] = c
	}
	s.dirty = true
}

func daysIn(y int, m time.Month, loc *time.Location) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
}

// periodKey 返回 now 所在统计周期的起点。rule.Day 为 0 表示不按月重置；
// 重置日大于当月天数时取当月最后一天（例如 31 日在 2 月取 28 日或 29 日）。
// 重置时刻是 0:00 时返回 2006-01-02 格式，否则返回 2006-01-02T15:04。
// 服务端有一份算法相同的实现（periodStart），两边的结果必须一致。
func periodKey(now time.Time, rule resetRule) string {
	if rule.Day <= 0 {
		return "none"
	}
	loc := rule.Loc
	if loc == nil {
		loc = now.Location()
	}
	now = now.Add(rule.Skew).In(loc)
	y, m, _ := now.Date()
	day := rule.Day
	if dm := daysIn(y, m, loc); day > dm {
		day = dm
	}
	start := time.Date(y, m, day, rule.Hour, rule.Minute, 0, 0, loc)
	if now.Before(start) {
		prev := time.Date(y, m-1, 1, 0, 0, 0, 0, loc)
		py, pm, _ := prev.Date()
		day = rule.Day
		if dm := daysIn(py, pm, loc); day > dm {
			day = dm
		}
		start = time.Date(py, pm, day, rule.Hour, rule.Minute, 0, 0, loc)
	}
	if rule.Hour == 0 && rule.Minute == 0 {
		return start.Format("2006-01-02")
	}
	return start.Format("2006-01-02T15:04")
}

// calibrate 应用服务端下发的手动校准：把本期累计流量直接改成填写的数值，之后继续往上累计。
// 同一个 cal_id 只应用一次；校准所属的周期已经过去（或重置规则改过）时不应用。
func (s *state) calibrate(cfg config) {
	stMu.Lock()
	defer stMu.Unlock()
	if cfg.CalID == "" || cfg.CalID == s.CalID || cfg.CalPeriod != s.Period {
		return
	}
	s.RX, s.TX, s.CalID = cfg.CalRX, cfg.CalTX, cfg.CalID
	s.dirty = true
}

// monthly 返回当前统计周期内累计的下行、上行字节数。
func (s *state) monthly() (rx, tx uint64) {
	stMu.Lock()
	defer stMu.Unlock()
	return s.RX, s.TX
}
