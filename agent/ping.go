package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	pingProbes  = 3                      // 每个目标每轮探测次数
	pingTimeout = 2 * time.Second        // 单次连接超时
	pingGap     = 300 * time.Millisecond // 两次探测的间隔
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

func resolveHost(ctx context.Context, host string) (string, bool) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), true
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil || len(addrs) == 0 {
		return "", false
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP.String(), true
		}
	}
	return addrs[0].IP.String(), true
}

// probeTarget 对目标做 pingProbes 次 TCP 连接探测，返回平均耗时（毫秒）和成功次数。
// 域名先解析一次，耗时只计算 TCP 握手。对方主动拒绝连接（RST）说明网络是通的，也计为成功。
func probeTarget(ctx context.Context, host, port string) (avgMs float64, ok int) {
	ip, found := resolveHost(ctx, host)
	if !found {
		return 0, 0
	}
	addr := net.JoinHostPort(ip, port)
	var sum time.Duration
	for i := 0; i < pingProbes; i++ {
		d := net.Dialer{Timeout: pingTimeout}
		t0 := time.Now()
		c, err := d.DialContext(ctx, "tcp", addr)
		elapsed := time.Since(t0)
		if err == nil {
			_ = c.Close()
			sum += elapsed
			ok++
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			sum += elapsed
			ok++
		}
		if i < pingProbes-1 && !sleepCtx(ctx, pingGap) {
			break
		}
	}
	if ok == 0 {
		return 0, 0
	}
	v := round1(float64(sum) / float64(ok) / float64(time.Millisecond))
	if v < 0.1 {
		v = 0.1
	}
	return v, ok
}

// startPing 并行探测四个目标，结果通过 channel 返回，可以与系统指标采集同时进行。
// 结果的约定与 Worker 一致：ping_* 为数字（毫秒）、null（本轮全部超时）、false（未配置）；loss_* 为丢包百分比。
func startPing(ctx context.Context, cfg config) <-chan map[string]any {
	ch := make(chan map[string]any, 1)
	go func() {
		out := map[string]any{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, k := range slotKeys {
			host, port, ok := normalizeTarget(cfg.Targets[k])
			if !ok {
				mu.Lock()
				out["ping_"+k] = false
				out["loss_"+k] = false
				mu.Unlock()
				continue
			}
			wg.Add(1)
			go func(k, host, port string) {
				defer wg.Done()
				avg, okN := probeTarget(ctx, host, port)
				loss := round1(float64(pingProbes-okN) * 100 / float64(pingProbes))
				mu.Lock()
				defer mu.Unlock()
				if okN == 0 {
					out["ping_"+k] = nil
				} else {
					out["ping_"+k] = avg
				}
				out["loss_"+k] = loss
			}(k, host, port)
		}
		wg.Wait()
		ch <- out
	}()
	return ch
}
