// cfsm-lite-agent：CFSM Lite 的极简探针。只向 Worker 单向上报，不监听端口，不执行任何远程命令。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：流量重置时刻按服务端设置的时区计算，不依赖服务器自带的 tzdata
)

// version 在构建时用 -ldflags "-X main.version=v0.1.0" 注入。
var version = "dev"

type settings struct {
	URL       string
	ID        string
	Secret    string
	StatePath string
	AllowHTTP bool
	Debug     bool
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseSettings 读取启动参数；每个参数都可以用环境变量提供（密钥建议用 CFSM_SECRET，避免出现在进程列表里）。
func parseSettings(args []string) (s settings, showVersion bool, err error) {
	fs := flag.NewFlagSet("cfsm-lite-agent", flag.ContinueOnError)
	fs.StringVar(&s.URL, "url", envOr("CFSM_URL", ""), "Worker 上报地址，例如 https://example.com/update（环境变量 CFSM_URL）")
	fs.StringVar(&s.ID, "id", envOr("CFSM_ID", ""), "节点 ID（环境变量 CFSM_ID）")
	fs.StringVar(&s.Secret, "secret", envOr("CFSM_SECRET", ""), "上报密钥（环境变量 CFSM_SECRET）")
	fs.StringVar(&s.StatePath, "state", envOr("CFSM_STATE", "/opt/cfsm-lite/state.json"), "状态文件路径（环境变量 CFSM_STATE）")
	fs.BoolVar(&s.AllowHTTP, "allow-http", false, "允许使用 http:// 上报（密钥会明文传输，不推荐）")
	fs.BoolVar(&s.Debug, "debug", false, "输出调试日志")
	fs.BoolVar(&showVersion, "version", false, "打印版本号并退出")
	if err = fs.Parse(args); err != nil {
		return s, false, err
	}
	if showVersion {
		return s, true, nil
	}
	if s.URL == "" || s.ID == "" || s.Secret == "" {
		return s, false, errors.New("必须提供 -url、-id、-secret（或环境变量 CFSM_URL、CFSM_ID、CFSM_SECRET）")
	}
	if !idRe.MatchString(s.ID) {
		return s, false, errors.New("节点 ID 格式无效")
	}
	u, perr := url.Parse(s.URL)
	if perr != nil || u.Host == "" {
		return s, false, errors.New("上报地址无效")
	}
	if u.Scheme != "https" && !(s.AllowHTTP && u.Scheme == "http") {
		return s, false, errors.New("上报地址必须以 https:// 开头（调试时可加 -allow-http）")
	}
	return s, false, nil
}

func main() {
	s, showVersion, err := parseSettings(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(2)
	}
	if showVersion {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("cfsm-lite-agent %s 启动，节点 %s", version, s.ID)
	run(ctx, s)
	log.Printf("已退出")
}

// run 是主循环：采集 -> 上报 -> 按 Worker 下发的间隔等待。
func run(ctx context.Context, s settings) {
	st := loadState(s.StatePath)
	box := newCfgBox()
	// 先恢复上次保存的服务端配置：重置日、时区、统计网卡与服务端一致，重启后才能接着累计当月流量
	if body := st.cachedCfg(); body != "" {
		if c, perr := parseConfig(body, defaultConfig()); perr == nil {
			box.set(c)
		} else {
			log.Printf("忽略状态文件里保存的配置: %v", perr)
		}
	}
	// 退出（重启、升级时的 SIGTERM）前再写一次，实时推送期间累计的流量也不会丢
	defer func() { _ = st.save(s.StatePath) }()
	go saveLoop(ctx, st, s.StatePath, 30*time.Second)
	sh := &sharedData{}
	pingDebug = s.Debug
	go newPinger(sh).run(ctx, box) // 后台持续采样延迟和丢包，上报时只取统计结果
	rep := newReporter(s)
	rep.syncClock(ctx) // 先校准本机时钟差，第一份指标的开机时间才是准的
	cfgMD5 := "none"
	failures := 0

	// 实时推送：平时只保持一条空闲连接，有人在看时才高频推送；它失败不影响下面的 HTTP 上报
	go runRealtime(ctx, s, box, st, sh)

	for {
		started := time.Now()
		cfg := box.get()
		metrics := collectMetrics(ctx, cfg, st, sh)
		if ctx.Err() != nil {
			return
		}
		// 状态文件由 saveLoop 每 30 秒（有变化时）和退出时保存，这里不再每轮都写一次：
		// 少一次无谓的写入，对 SD 卡、eMMC 这类闪存的开发板更友好。收到新配置时仍会立即保存（见下）。

		wait := cfg.ReportInterval
		res, err := rep.send(ctx, metrics, cfgMD5)
		switch {
		case err != nil:
			failures++
			log.Printf("上报失败（连续第 %d 次）: %v", failures, err)
		case res.Status == http.StatusNoContent:
			failures = 0
		case res.Status == http.StatusOK:
			failures = 0
			if strings.Contains(res.Body, "report_interval=") {
				next, perr := parseConfig(res.Body, cfg)
				if perr != nil {
					log.Printf("忽略无效的配置: %v", perr)
				} else {
					cfg = next
					box.set(cfg)
					st.setCfg(strings.TrimSpace(res.Body))
					if err := st.save(s.StatePath); err != nil && s.Debug {
						log.Printf("保存状态文件失败: %v", err)
					}
					cfgMD5 = md5Hex(res.Body) // Worker 对完整响应体计算 MD5，这里原样回传
					log.Printf("已应用新配置：上报间隔 %s，重置 每月%d日 %02d:%02d（%s），连接模式 %s", cfg.ReportInterval, cfg.ResetDay, cfg.ResetHour, cfg.ResetMinute, cfg.TZName, cfg.ConnMode)
					wait = cfg.ReportInterval
				}
			}
		case res.Status == http.StatusBadRequest || res.Status == http.StatusUnauthorized ||
			res.Status == http.StatusForbidden || res.Status == http.StatusNotFound:
			failures++
			log.Printf("上报被服务端拒绝（HTTP %d），请检查节点 ID 与上报密钥，120 秒后重试", res.Status)
			if wait < 120*time.Second {
				wait = 120 * time.Second
			}
		default:
			failures++
			log.Printf("服务端返回 HTTP %d", res.Status)
		}

		if s.Debug {
			log.Printf("本轮 cpu=%v ping_ct=%v loss_ct=%v 月流量下行=%v", metrics["cpu"], metrics["ping_ct"], metrics["loss_ct"], metrics["net_rx_monthly"])
		}

		pause := wait - time.Since(started)
		if pause < time.Second {
			pause = time.Second
		}
		if !sleepCtx(ctx, pause) {
			return
		}
	}
}
