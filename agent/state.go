package main

import (
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
	stMu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
func (s *state) advance(rawRX, rawTX uint64, now time.Time, rule resetRule, iface string) {
	stMu.Lock()
	defer stMu.Unlock()
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
}

// monthly 返回当前统计周期内累计的下行、上行字节数。
func (s *state) monthly() (rx, tx uint64) {
	stMu.Lock()
	defer stMu.Unlock()
	return s.RX, s.TX
}
