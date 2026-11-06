package main

import (
	"context"
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 延迟与丢包的采集和计算方式参照 komari-agent：每个样本只探测一次（TCP 握手，超时 3 秒），
// 失败记为丢包；握手耗时超过 1 秒时重测，疑似 SYN 重传的样本也记为丢包（见 measureSample）。
// 域名解析也与 komari 一致：取系统解析器返回的第一个地址，不再强行优先 IPv4（见 resolveHost）。
// 丢包率不是在一次探测里算，而是在一段时间的大量样本上统计：komari 由面板统计，
// Lite 的服务端只保存最新值，所以样本留在 Agent 里，按滑动窗口统计后随指标上报。
// 统计口径同样与 komari 面板一致：丢包率 = 失败样本数 ÷ 总样本数，延迟 = 成功样本的平均值。
const (
	pingTimeout        = 3 * time.Second  // 单次连接超时
	pingResolveTimeout = 10 * time.Second // 域名解析超时（komari 解析没有单独的超时，这里只防止卡死）
	pingInterval       = 10 * time.Second // 每个目标的采样间隔
	pingLossWindow     = time.Hour        // 丢包率统计窗口（komari 面板按小时级窗口统计，10 分钟只有 60 个样本，波动太大）
	pingLatencyWindow  = 10 * time.Minute // 延迟（取平均值）统计窗口
	pingMaxSamples     = 400              // 每个目标最多保留的样本数（1 小时 + 余量）
	pingFirstWait      = 15 * time.Second // 第一次上报前最多等多久，让每个目标至少有一个样本

	pingHighMs    = 1000 // 耗时超过这个值（毫秒）就重测
	pingRetries   = 3    // 重测次数上限
	pingRetryDrop = 800  // 重测耗时比首次少这么多（毫秒）以上，认为首次遇到了 SYN 重传：首次 1000ms 的重传超时减去 200ms 抖动
)

// pingDebug 为 true 时（启动参数 -debug）把每个样本的解析结果、耗时和失败原因写进日志，
// 用来和 komari 对比：同一台机器上两边解析到的 IP（尤其是 IPv4 还是 IPv6）和目标端口必须一致。
var pingDebug bool

var (
	errResolve    = errors.New("failed to resolve target")
	errRetransmit = errors.New("suspicious retransmission detected in tcp handshake")
	errStillHigh  = errors.New("latency remains high after retries")
)

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// normalizeTarget 把 host、host:port、[v6]:port 或裸 IPv6 拆成主机和端口；省略端口时用 80。
func normalizeTarget(t string) (host, port string, ok bool) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", "", false
	}
	h, p, err := net.SplitHostPort(t)
	if err != nil {
		h, p = strings.Trim(t, "[]"), "80"
	}
	if h == "" {
		return "", "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	return h, p, true
}

// resolveHost 解析域名，做法与 komari-agent 的 resolveIP 一致：取系统解析器返回的第一个地址。
// Go 的解析器会按 RFC 6724 排序：本机有可用的 IPv6 时 IPv6 排在前面，没有时 IPv4 排在前面。
// 以前这里强行优先 IPv4，在默认目标（*-dualstack 双栈域名）上会和 komari 走不同的网络路径，
// 延迟和丢包自然对不上。
func resolveHost(ctx context.Context, host string) (string, bool) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), true
	}
	rctx, cancel := context.WithTimeout(ctx, pingResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(rctx, host)
	if err != nil || len(addrs) == 0 {
		return "", false
	}
	return addrs[0], true
}

// tcpPing 测一次 TCP 握手耗时。域名先解析，耗时只计算握手；连接失败（包括被拒绝）都返回错误，与 komari 一致。
func tcpPing(ctx context.Context, host, port string) (time.Duration, error) {
	ip, ok := resolveHost(ctx, host)
	if !ok {
		return 0, errResolve
	}
	if pingDebug {
		log.Printf("ping 解析 %s -> %s", host, ip)
	}
	d := net.Dialer{Timeout: pingTimeout}
	t0 := time.Now()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
	elapsed := time.Since(t0)
	if err != nil {
		return 0, err
	}
	_ = c.Close()
	return elapsed, nil
}

// measureSample 得到一个样本，处理方式与 komari-agent 的 NewPingTask 相同：
// 首次测量失败，样本记为丢包；耗时不超过 1000ms，直接采用；
// 超过 1000ms 就重测，最多 3 次：重测失败记为丢包；重测不超过 1000ms 时，
// 如果比首次少了 800ms 以上，说明首次多半是 SYN 丢失后等内核 1 秒重传，不是真实延迟，记为丢包，否则采用重测值；
// 3 次重测都还是超过 1000ms，记为丢包。
func measureSample(measure func() (time.Duration, error)) (time.Duration, error) {
	first, err := measure()
	if err != nil {
		return 0, err
	}
	if first.Milliseconds() <= pingHighMs {
		return first, nil
	}
	for i := 0; i < pingRetries; i++ {
		second, err2 := measure()
		if err2 != nil {
			return 0, err2
		}
		if second.Milliseconds() <= pingHighMs {
			if first.Milliseconds()-second.Milliseconds() > pingRetryDrop {
				return 0, errRetransmit
			}
			return second, nil
		}
	}
	return 0, errStillHigh
}

// pingSample 是一次采样的结果。
type pingSample struct {
	at time.Time
	ms float64 // 握手耗时（毫秒），失败时为 0
	ok bool
}

type slotHist struct {
	target  string
	samples []pingSample
}

// pinger 在后台对四个目标持续采样，把滑动窗口内的统计结果发布到 sharedData，
// HTTP 上报和实时推送都读取同一份结果。
type pinger struct {
	mu      sync.Mutex
	targets map[string]string    // 槽位 -> 当前探测目标（host:port）；空表示未配置
	hist    map[string]*slotHist // 槽位 -> 样本
	sh      *sharedData
	now     func() time.Time
}

func newPinger(sh *sharedData) *pinger {
	return &pinger{targets: map[string]string{}, hist: map[string]*slotHist{}, sh: sh, now: time.Now}
}

// setTarget 记录槽位当前的目标；目标变了（或被清空）就丢掉旧样本，避免新旧目标的数据混在一起统计。
func (p *pinger) setTarget(slot, target string) {
	p.mu.Lock()
	if old, seen := p.targets[slot]; seen && old == target {
		p.mu.Unlock()
		return
	}
	p.targets[slot] = target
	delete(p.hist, slot)
	out := p.buildLocked()
	p.mu.Unlock()
	p.sh.setPing(out)
}

// record 保存一个样本，并发布最新的统计结果。目标已经变了的过期样本直接丢弃。
func (p *pinger) record(slot, target string, at time.Time, d time.Duration, ok bool) {
	p.mu.Lock()
	if p.targets[slot] != target {
		p.mu.Unlock()
		return
	}
	h := p.hist[slot]
	if h == nil {
		h = &slotHist{target: target}
		p.hist[slot] = h
	}
	s := pingSample{at: at, ok: ok}
	if ok {
		s.ms = round1(float64(d) / float64(time.Millisecond))
		if s.ms < 0.1 {
			s.ms = 0.1
		}
	}
	h.samples = append(h.samples, s)
	cut := at.Add(-pingLossWindow)
	drop := 0
	for drop < len(h.samples) && h.samples[drop].at.Before(cut) {
		drop++
	}
	if over := len(h.samples) - drop - pingMaxSamples; over > 0 {
		drop += over
	}
	h.samples = append([]pingSample(nil), h.samples[drop:]...)
	out := p.buildLocked()
	p.mu.Unlock()
	p.sh.setPing(out)
}

// buildLocked 生成要上报的 ping_* 和 loss_*（调用时必须持有 p.mu）。结果的约定与 Worker 一致：
// ping_* 为数字（毫秒）、null（窗口内全部失败）、false（未配置）；loss_* 为丢包百分比。
// 已配置但还没有样本的槽位不放进结果，由 pingReady 判断是否还在等第一个样本。
func (p *pinger) buildLocked() map[string]any {
	now := p.now()
	out := map[string]any{}
	for _, k := range slotKeys {
		if p.targets[k] == "" {
			out["ping_"+k] = false
			out["loss_"+k] = false
			continue
		}
		h := p.hist[k]
		if h == nil {
			continue
		}
		if lat, loss, ok := summarize(h.samples, now); ok {
			out["ping_"+k] = lat
			out["loss_"+k] = loss
		}
	}
	return out
}

// summarize 统计一个目标的样本：丢包率 = 丢包窗口内失败数 ÷ 总数；
// 延迟 = 延迟窗口内成功样本的平均值（komari 面板的延迟统计也是平均值），窗口内没有成功样本时为 nil（超时）。
// 丢包窗口内没有任何样本时 ok 为 false。
func summarize(samples []pingSample, now time.Time) (latency any, loss float64, ok bool) {
	lossCut := now.Add(-pingLossWindow)
	latCut := now.Add(-pingLatencyWindow)
	total, failed := 0, 0
	var sum float64
	nOK := 0
	for _, s := range samples {
		if s.at.Before(lossCut) {
			continue
		}
		total++
		if !s.ok {
			failed++
		} else if !s.at.Before(latCut) {
			sum += s.ms
			nOK++
		}
	}
	if total == 0 {
		return nil, 0, false
	}
	loss = round1(float64(failed) * 100 / float64(total))
	if nOK == 0 {
		return nil, loss, true
	}
	return round1(sum / float64(nOK)), loss, true
}

// run 为四个槽位各起一个协程，持续采样直到 ctx 结束。
func (p *pinger) run(ctx context.Context, box *cfgBox) {
	var wg sync.WaitGroup
	for _, k := range slotKeys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			p.runSlot(ctx, k, box)
		}(k)
	}
	wg.Wait()
}

func (p *pinger) runSlot(ctx context.Context, slot string, box *cfgBox) {
	for {
		started := time.Now()
		host, port, ok := normalizeTarget(box.get().Targets[slot])
		if !ok {
			p.setTarget(slot, "")
		} else {
			target := net.JoinHostPort(host, port)
			p.setTarget(slot, target)
			d, err := measureSample(func() (time.Duration, error) { return tcpPing(ctx, host, port) })
			if ctx.Err() != nil {
				return
			}
			if pingDebug {
				if err != nil {
					log.Printf("ping[%s] %s 记为丢包: %v", slot, target, err)
				} else {
					log.Printf("ping[%s] %s %.1fms", slot, target, float64(d)/float64(time.Millisecond))
				}
			}
			p.record(slot, target, started, d, err == nil)
		}
		wait := pingInterval - time.Since(started)
		if wait < time.Second {
			wait = time.Second
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// pingReady 判断每个已配置的目标是否都已经有了统计结果。
func pingReady(m map[string]any, cfg config) bool {
	for _, k := range slotKeys {
		if _, _, ok := normalizeTarget(cfg.Targets[k]); !ok {
			continue
		}
		if _, has := m["ping_"+k]; !has {
			return false
		}
	}
	return true
}

// waitPing 等到每个已配置的目标至少有一个样本（最多等 max），返回当前的统计结果；
// 只在刚启动的第一轮上报时才会真的等待。
func waitPing(ctx context.Context, sh *sharedData, cfg config, max time.Duration) map[string]any {
	deadline := time.Now().Add(max)
	for {
		m := sh.getPing()
		if pingReady(m, cfg) || !time.Now().Before(deadline) {
			return m
		}
		if !sleepCtx(ctx, 100*time.Millisecond) {
			return sh.getPing()
		}
	}
}
