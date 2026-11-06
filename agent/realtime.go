package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// cfgBox 让 HTTP 上报循环和实时推送协程共用同一份运行配置。
// config 里的 Targets 在 parseConfig 中总是复制后再修改，所以读取方拿到的副本可以安全地只读使用。
type cfgBox struct {
	mu sync.RWMutex
	c  config
}

func newCfgBox() *cfgBox { return &cfgBox{c: defaultConfig()} }

func (b *cfgBox) get() config {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.c
}

func (b *cfgBox) set(c config) {
	b.mu.Lock()
	b.c = c
	b.mu.Unlock()
}

const (
	rtKeepalive  = 25 * time.Second // 空闲时每隔多久发一次 ping，防止连接被中间设备判定为空闲而断开
	rtModeCheck  = 5 * time.Second  // 多久检查一次「连接模式」是否被改成了仅 HTTP
	rtMaxBackoff = 5 * time.Minute
)

// rtInterval 把服务端通知的秒数限制在 1 到 5 秒；不合法时用 fallback。
func rtInterval(secs float64, fallback time.Duration) time.Duration {
	if secs >= 1 && secs <= 5 {
		return time.Duration(secs * float64(time.Second))
	}
	return fallback
}

// runRealtime 维护一条到 Worker 的 WebSocket 连接（连接模式为「自动」时）：
// 平时连接是空闲的，只发 ping 保活；有人打开 Monito 或网页时，服务端通知 Agent「激活」，
// Agent 就每隔 1 到 5 秒推送一次完整的实时指标；没人看了再通知「停止」。
// 连接失败时不影响 HTTP 上报：HTTP 循环一直在独立工作，这里只是让实时数据更快。
func runRealtime(ctx context.Context, s settings, box *cfgBox, st *state, sh *sharedData) {
	backoff := 5 * time.Second
	var lastLog time.Time
	for ctx.Err() == nil {
		if box.get().ConnMode == "http" {
			if !sleepCtx(ctx, rtModeCheck) {
				return
			}
			continue
		}
		started := time.Now()
		err := realtimeSession(ctx, s, box, st, sh)
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		var he *handshakeError
		var ce *closeError
		switch {
		case err == nil:
			continue // 连接模式被改成了仅 HTTP，回到循环开头等待
		case errors.As(err, &he) && (he.Status == http.StatusServiceUnavailable || he.Status == http.StatusForbidden):
			wait = rtMaxBackoff // 服务端没有启用实时推送，或这个节点被设成仅 HTTP：很久之后再试
		case errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusUnauthorized || he.Status == http.StatusNotFound):
			wait = 120 * time.Second // ID 或密钥有问题：HTTP 上报同样会被拒绝，不必频繁重试
		case errors.As(err, &ce) && (ce.Code == 1008 || ce.Code == 4404):
			wait = 120 * time.Second // 节点已被删除或未通过校验
		}
		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second
		} else if backoff < rtMaxBackoff {
			backoff *= 2
		}
		if time.Since(lastLog) > time.Minute {
			log.Printf("实时推送连接断开：%v，%s 后重试（HTTP 上报不受影响）", err, wait)
			lastLog = time.Now()
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

type rtMessage struct {
	Type     string  `json:"type"`
	Active   bool    `json:"active"`
	Interval float64 `json:"interval"`
}

// realtimeSession 建立一次连接并一直处理到出错或被关闭。正常因为「连接模式改成 http」而结束时返回 nil。
func realtimeSession(ctx context.Context, s settings, box *cfgBox, st *state, sh *sharedData) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+s.Secret)
	hdr.Set("X-Agent-Id", s.ID)
	hdr.Set("X-Agent-Version", "lite-go-"+version)
	conn, err := dialWS(ctx, s.URL, hdr)
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)
	msgs := make(chan string, 8)
	rerr := make(chan error, 1)
	go func() {
		for {
			m, err := conn.ReadText()
			if err != nil {
				rerr <- err
				return
			}
			select {
			case msgs <- m:
			case <-done:
				return
			}
		}
	}()

	keepalive := time.NewTicker(rtKeepalive)
	defer keepalive.Stop()
	modeCheck := time.NewTicker(rtModeCheck)
	defer modeCheck.Stop()

	var fs fastSampler
	var timer *time.Timer
	var frameC <-chan time.Time
	interval := box.get().WSSInterval
	active := false
	setActive := func(on bool, secs float64) {
		interval = rtInterval(secs, box.get().WSSInterval)
		if timer != nil {
			timer.Stop()
			timer = nil
			frameC = nil
		}
		active = on
		if on {
			fs.reset(box.get()) // 以此刻为基线，第一帧的 CPU 与网速就是这个间隔内的平均值
			timer = time.NewTimer(interval)
			frameC = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-rerr:
			return err
		case <-keepalive.C:
			if err := conn.WriteText("ping"); err != nil {
				return err
			}
		case <-modeCheck.C:
			if box.get().ConnMode == "http" {
				return nil
			}
		case raw := <-msgs:
			var in rtMessage
			if raw == "pong" || json.Unmarshal([]byte(raw), &in) != nil {
				continue
			}
			switch in.Type {
			case "ready":
				setActive(in.Active, in.Interval)
			case "activate":
				setActive(true, in.Interval)
			case "deactivate":
				setActive(false, 0)
			}
		case <-frameC:
			payload, err := json.Marshal(map[string]any{"type": "metrics", "metrics": fs.next(box.get(), st, sh)})
			if err != nil {
				return err
			}
			if err := conn.WriteText(string(payload)); err != nil {
				return err
			}
			if active && timer != nil {
				timer.Reset(interval)
			}
		}
	}
}
