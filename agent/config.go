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
	Interface      string
	PingMode       string
	ConnMode       string        // auto：有人在看时实时推送，其余时间 HTTP POST；http：只用 HTTP POST
	WSSInterval    time.Duration // 实时推送的间隔（1 到 5 秒）；实际以服务端通知的为准
	Targets        map[string]string // ct、cu、cm、bd 四个探测目标
}

var slotKeys = []string{"ct", "cu", "cm", "bd"}

// defaultConfig 是第一次连上 Worker 之前使用的默认值，与 Worker 里的内置默认一致。
func defaultConfig() config {
	return config{
		ReportInterval: 60 * time.Second,
		ResetDay:       1,
		PingMode:       "tcp",
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
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)
	ifaceRe = regexp.MustCompile(`^[A-Za-z0-9._:@,-]{0,128}$`)
)

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseConfig 解析 Worker 下发的 QueryParam 配置串（例如 report_interval=60&reset_day=1&...）。
// 只认下面列出的键，其余（connection_mode、wss_report_interval、schema_version 等）一律忽略。
// 任何一项不合法就整体拒绝并保持当前配置，避免半套用。
func parseConfig(body string, cur config) (config, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > 512 {
		return cur, errors.New("配置为空或超过 512 字节")
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
	if q.Has("interface") {
		v := q.Get("interface")
		if !ifaceRe.MatchString(v) {
			return cur, fmt.Errorf("interface 无效: %q", v)
		}
		next.Interface = v
	}
	if v := q.Get("ping_mode"); v != "" {
		if v != "tcp" && v != "icmp" {
			return cur, fmt.Errorf("ping_mode 无效: %q", v)
		}
		next.PingMode = v
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
	return next, nil
}
