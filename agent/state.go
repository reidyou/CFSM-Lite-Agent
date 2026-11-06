package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stMu 保护 state：HTTP 上报循环和实时推送协程都会更新当月累计流量。
var stMu sync.Mutex

// state 保存「当月累计流量」所需的最少信息，写在本地文件里，Agent 重启后可以接着累计。
type state struct {
	Period string `json:"period"`  // 当前统计周期的起点，例如 2026-10-01；重置时刻不是 0:00 时带上时刻，例如 2026-10-01T08:30
	Iface  string `json:"iface"`   // 统计所用的网卡配置；变更后重新建立基线
	RX     uint64 `json:"rx"`      // 本周期累计下行字节
	TX     uint64 `json:"tx"`      // 本周期累计上行字节
	LastRX uint64 `json:"last_rx"` // 上次读到的网卡原始累计值
	LastTX uint64 `json:"last_tx"`
	Init   bool   `json:"init"`
	CalID  string `json:"cal_id"` // 最近一次应用的手动校准编号，避免同一次校准重复应用
	Cfg    string `json:"cfg"`    // 最近一次应用的服务端配置原文；重启后先用它，不用默认配置，否则重置日、时区、网卡不同的节点会被误清零

	lastAt time.Time // 上一次累计所用读数的采集时间（只在内存里，用来丢弃过期的读数）
	dirty  bool      // 内存里有尚未写入文件的变化（只在内存里）
}

func loadState(path string) *state {
	s := &state{}
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(b, s); err != nil {
		return &state{}
	}
	return s
}

// save 先写临时文件再改名，避免写到一半断电留下损坏的文件。
func (s *state) save(path string) error {
	if path == "" {
		return nil
	}
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

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// delta 计算两次读数之间的增量；当前值小于上次说明计数器被重置（重启、网卡重建），此时整个当前值都是新增量。
func delta(cur, last uint64) uint64 {
	if cur >= last {
		return cur - last
	}
	return cur
}

// resetRule 描述流量何时重置：每月 Day 日的 Hour:Minute（按 Loc 时区）；Day 为 0 表示不重置。
type resetRule struct {
	Day, Hour, Minute int
	Loc               *time.Location
}

// rule 取出配置里的重置规则。
func (c config) rule() resetRule {
	return resetRule{Day: c.ResetDay, Hour: c.ResetHour, Minute: c.ResetMinute, Loc: c.Loc}
}

// advance 用最新的网卡累计值更新当月累计流量。
// now 是这组读数的采集时间。HTTP 上报循环采完读数后还要等 Ping 结束才会调用这里，期间实时推送协程可能已经用更新的读数累计过了；
// 这时再用较早的读数，会被当成「计数器被重置」而把整个网卡累计值加进当月流量，所以比上次更早的读数直接丢弃。
func (s *state) advance(rawRX, rawTX uint64, now time.Time, rule resetRule, iface string) {
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
		s.RX += delta(rawRX, s.LastRX)
		s.TX += delta(rawTX, s.LastTX)
	}
	s.LastRX, s.LastTX, s.Init = rawRX, rawTX, true
	s.dirty = true
}

func daysIn(y int, m time.Month, loc *time.Location) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
}

// periodKey 返回 now 所在统计周期的起点。rule.Day 为 0 表示不按月重置；
// 重置日大于当月天数时取当月最后一天（例如 31 日在 2 月取 28 日或 29 日）。
// 重置时刻是 0:00 时返回 2006-01-02 格式（与旧版本一致，升级时不会误清零），否则返回 2006-01-02T15:04。
// 服务端有一份算法相同的实现（periodStart），两边的结果必须一致。
func periodKey(now time.Time, rule resetRule) string {
	if rule.Day <= 0 {
		return "none"
	}
	loc := rule.Loc
	if loc == nil {
		loc = now.Location()
	}
	now = now.In(loc)
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
