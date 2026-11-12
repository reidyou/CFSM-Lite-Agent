package main

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// config 是 Worker 通过 /update 响应下发的运行参数。本地只需要 URL、ID、密钥，其余都来自这里。
type config struct {
	ReportInterval time.Duration
	ResetDay       int
	ResetHour      int            // 流量重置的时刻：时（0 到 23）
	ResetMinute    int            // 流量重置的时刻：分（0 到 59）
	TZName         string         // 重置时刻使用的时区（IANA 名称），由服务端「系统」设置统一下发
	Loc            *time.Location // TZName 对应的时区
	CalID          string         // 手动校准的编号；同一个编号只应用一次
	CalRX, CalTX   uint64         // 手动校准的本期下行、上行字节数
	CalPeriod      string         // 校准所属的统计周期；周期已经过去的校准不再应用
	Interface      string
	ConnMode       string            // auto：有人在看时实时推送，其余时间 HTTP POST；http：只用 HTTP POST
	WSSInterval    time.Duration     // 实时推送的间隔（1 到 5 秒）；实际以服务端通知的为准
	Targets        map[string]string // ct、cu、cm、bd 四个探测目标
	// Synced 表示这份配置来自服务端（刚收到的，或上次保存在状态文件里的）。默认配置里是 false：
	// 此时重置日、时区、统计网卡都可能和服务端不一致，不能拿来累计当月流量，否则会把已有的累计误清零。
	Synced         bool
}

var slotKeys = []string{"ct", "cu", "cm", "bd"}

// defaultTZ 是第一次连上 Worker 之前使用的时区，与服务端的默认值一致。
const defaultTZ = "Asia/Shanghai"

// loadZone 加载时区；程序内嵌了时区数据库（见 main.go），所以不依赖服务器上的 tzdata。
// 万一失败就退回固定的东八区，保证不会因此 panic。
func loadZone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

// defaultConfig 是第一次连上 Worker 之前使用的默认值，与 Worker 里的内置默认一致。
func defaultConfig() config {
	return config{
		ReportInterval: 60 * time.Second,
		ResetDay:       1,
		TZName:         defaultTZ,
		Loc:            loadZone(defaultTZ),
		ConnMode:       "auto",
		WSSInterval:    2 * time.Second,
		Targets: map[string]string{
			"ct": "gd-ct-dualstack.ip.zstaticcdn.com",
			"cu": "gd-cu-dualstack.ip.zstaticcdn.com",
			"cm": "gd-cm-dualstack.ip.zstaticcdn.com",
			"bd": "ip.zstaticcdn.com",
		},
	}
}

var (
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)
	tzRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)
	calIDRe  = regexp.MustCompile(`^[0-9]{1,20}$`)
	calPerRe = regexp.MustCompile(`^(none|[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2})?)$`)
	ifaceRe  = regexp.MustCompile(`^[A-Za-z0-9._:@,-]{0,128}$`)
)

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseConfig 解析 Worker 下发的 QueryParam 配置串（例如 report_interval=60&reset_day=1&...）。
// 只认下面列出的键：report_interval、reset_day、reset_hour、reset_minute、timezone、cal_*（流量校准）、interface、
// connection_mode、wss_report_interval 和 custom_*（Ping 目标）；其余（schema_version 等）一律忽略。Ping 固定用 TCP，不需要配置。
// 任何一项不合法就整体拒绝并保持当前配置，避免半套用。
func parseConfig(body string, cur config) (config, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > 1024 {
		return cur, errors.New("配置为空或超过 1024 字节")
	}
	q, err := url.ParseQuery(body)
	if err != nil {
		return cur, err
	}
	next := cur
	next.Targets = make(map[string]string, len(cur.Targets))
	for k, v := range cur.Targets {
		next.Targets[k] = v
	}
	if v := q.Get("report_interval"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 30 && n != 60 && n != 120 && n != 180) {
			return cur, fmt.Errorf("report_interval 无效: %q", v)
		}
		next.ReportInterval = time.Duration(n) * time.Second
	}
	if v := q.Get("reset_day"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 31 {
			return cur, fmt.Errorf("reset_day 无效: %q", v)
		}
		next.ResetDay = n
	}
	if v := q.Get("reset_hour"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 23 {
			return cur, fmt.Errorf("reset_hour 无效: %q", v)
		}
		next.ResetHour = n
	}
	if v := q.Get("reset_minute"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 59 {
			return cur, fmt.Errorf("reset_minute 无效: %q", v)
		}
		next.ResetMinute = n
	}
	if v := q.Get("timezone"); v != "" {
		if !tzRe.MatchString(v) {
			return cur, fmt.Errorf("timezone 无效: %q", v)
		}
		loc, err := time.LoadLocation(v)
		if err != nil {
			return cur, fmt.Errorf("timezone 无效: %q", v)
		}
		next.TZName, next.Loc = v, loc
	}
	// 手动校准：每次下发都以服务端为准，没有 cal_id 就表示当前没有待应用的校准
	next.CalID, next.CalRX, next.CalTX, next.CalPeriod = "", 0, 0, ""
	if v := q.Get("cal_id"); v != "" {
		rx, errRX := strconv.ParseUint(q.Get("cal_rx"), 10, 64)
		tx, errTX := strconv.ParseUint(q.Get("cal_tx"), 10, 64)
		per := q.Get("cal_period")
		if !calIDRe.MatchString(v) || errRX != nil || errTX != nil || !calPerRe.MatchString(per) {
			return cur, fmt.Errorf("校准参数无效: cal_id=%q", v)
		}
		next.CalID, next.CalRX, next.CalTX, next.CalPeriod = v, rx, tx, per
	}
	if q.Has("interface") {
		v := q.Get("interface")
		if !ifaceRe.MatchString(v) {
			return cur, fmt.Errorf("interface 无效: %q", v)
		}
		next.Interface = v
	}
	if v := q.Get("connection_mode"); v != "" {
		if v != "auto" && v != "http" {
			return cur, fmt.Errorf("connection_mode 无效: %q", v)
		}
		next.ConnMode = v
	}
	if v := q.Get("wss_report_interval"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5 {
			return cur, fmt.Errorf("wss_report_interval 无效: %q", v)
		}
		next.WSSInterval = time.Duration(n) * time.Second
	}
	for _, k := range slotKeys {
		if v := q.Get("custom_" + k); v != "" {
			if !hostRe.MatchString(v) {
				return cur, fmt.Errorf("custom_%s 无效: %q", k, v)
			}
			next.Targets[k] = v
		}
	}
	next.Synced = true
	return next, nil
}
