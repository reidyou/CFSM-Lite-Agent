package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// 时间校准：用 Worker 响应里的 HTTP Date 头估算「本机时钟 − Worker 时钟」的差值，
// 只用来校正上报的开机时间（boot_time）和流量统计周期的划分；不会修改系统时间。
//
// 为什么需要：boot_time 来自 /proc/stat 的 btime，是按本机时钟算出来的，本机时钟不准（没开 NTP、虚拟机时钟漂移、
// 主板电池耗尽等）时，面板上的「在线时间」会跟着错；流量重置日、重置时刻也是按「现在几点」划分周期的，
// 本机时钟偏几个小时就会在错误的时间清零。
//
// 精度：Date 头只精确到秒，再加上网络往返，估算误差大约在 1 到 2 秒内，所以差值小于 5 秒时不校准（视为准确）。
const (
	clockApplyMin   = 5 * time.Second // 差值达到这么大才开始校准
	clockKeepMin    = 2 * time.Second // 已经在校准时，差值降到这以下才取消（避免在阈值附近来回切换）
	clockMaxRTT     = 5 * time.Second // 往返超过这么久的样本不可信，丢弃
	clockBestMaxAge = time.Hour       // 保留的最佳样本最多用这么久，之后任何新样本都可以替换它
	clockStepDiff   = 5 * time.Second // 新样本与最佳样本相差超过这么多，说明本机时钟被调整过，直接替换
)

type clockSample struct {
	offset time.Duration // Worker 时钟 − 本机时钟
	rtt    time.Duration
	at     time.Time
}

type clockSync struct {
	mu      sync.Mutex
	best    clockSample
	have    bool
	applied time.Duration // 当前生效的校准量；0 表示不校准
	logged  time.Duration // 上次写进日志的校准量
	logf    func(format string, args ...any)
}

// clk 是全局的校准状态：HTTP 上报、实时推送、采集都读它。
var clk = &clockSync{logf: log.Printf}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// get 返回当前生效的校准量：把本机时间加上它，就是 Worker 的时间。
func (c *clockSync) get() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applied
}

// observe 处理一次 HTTP 响应：sent 是发出请求的时刻，recv 是收到响应头的时刻。
// Worker 在响应里带的 Date 头只精确到秒（实际时刻在 Date 到 Date+1 秒之间），取中间值；
// 本机一侧取请求往返的中点。
func (c *clockSync) observe(h http.Header, sent, recv time.Time) {
	raw := h.Get("Date")
	if raw == "" {
		return
	}
	srv, err := http.ParseTime(raw)
	if err != nil {
		return
	}
	rtt := recv.Sub(sent)
	if rtt < 0 || rtt > clockMaxRTT {
		return
	}
	mid := sent.Round(0).Add(rtt / 2) // Round(0) 去掉单调时钟读数，只比较墙上时间
	c.add(clockSample{offset: srv.Add(500 * time.Millisecond).Sub(mid), rtt: rtt, at: recv})
}

func (c *clockSync) add(s clockSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 往返越短越可信，所以只保留往返最短的样本；但本机时钟被调整过（差值突变）或样本太旧时要更新
	if !c.have || s.rtt <= c.best.rtt || s.at.Sub(c.best.at) > clockBestMaxAge ||
		absDur(s.offset-c.best.offset) > clockStepDiff {
		c.best, c.have = s, true
	}
	off := c.best.offset
	switch {
	case absDur(off) >= clockApplyMin:
		c.applied = off.Round(time.Millisecond)
	case c.applied != 0 && absDur(off) >= clockKeepMin:
		c.applied = off.Round(time.Millisecond)
	default:
		c.applied = 0
	}
	if (c.applied == 0) != (c.logged == 0) || absDur(c.applied-c.logged) >= clockApplyMin {
		if c.logf != nil {
			if c.applied == 0 {
				c.logf("本机时钟与 Worker 已基本一致，取消时间校准")
			} else {
				dir := "慢"
				if c.applied < 0 {
					dir = "快"
				}
				c.logf("本机时钟比 Worker %s了约 %s，已用它校正上报的开机时间和流量统计周期；不会修改系统时间，建议检查本机的 NTP 同步",
					dir, absDur(c.applied).Round(time.Second))
			}
		}
		c.logged = c.applied
	}
}

// syncClock 在启动时先向 Worker 发一次 OPTIONS 请求（Worker 对它直接返回 204，不碰数据库），拿到响应里的 Date 头：这样第一次上报之前就已经完成校准，
// 不会让第一份指标带着未校准的开机时间。失败（网络不通）不影响后面的流程，之后每次 HTTP 上报和实时连接握手也会继续校准。
func (r *reporter) syncClock(ctx context.Context) {
	u, err := url.Parse(r.url)
	if err != nil || u.Host == "" {
		return
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/", "", "", ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, u.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Agent-Version", "Lite-agent-"+version)
	sent := time.Now()
	resp, err := r.client.Do(req)
	recv := time.Now()
	if err != nil {
		return
	}
	defer resp.Body.Close()
	clk.observe(resp.Header, sent, recv)
}
