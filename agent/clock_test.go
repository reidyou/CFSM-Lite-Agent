package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// useClock 换上一个全新的校准状态（不打日志），测试结束后恢复，避免测试之间互相影响。
func useClock(t *testing.T) *clockSync {
	t.Helper()
	old := clk
	c := &clockSync{}
	clk = c
	t.Cleanup(func() { clk = old })
	return c
}

func dateHdr(t time.Time) http.Header {
	h := http.Header{}
	h.Set("Date", t.UTC().Format(http.TimeFormat))
	return h
}

var clockT0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestClockObserveApplies(t *testing.T) {
	c := useClock(t)
	// 本机 12:00:00.000 发出、12:00:00.200 收到；Worker 的 Date 是 14:00:00 → 取中点：本机 12:00:00.100，Worker 约 14:00:00.500
	c.observe(dateHdr(clockT0.Add(2*time.Hour)), clockT0, clockT0.Add(200*time.Millisecond))
	want := 2*time.Hour + 400*time.Millisecond
	if got := c.get(); got != want {
		t.Fatalf("applied = %v, want %v", got, want)
	}
}

func TestClockObserveNegative(t *testing.T) {
	c := useClock(t)
	// 本机时钟比 Worker 快 3 小时
	c.observe(dateHdr(clockT0.Add(-3*time.Hour)), clockT0, clockT0)
	if got := c.get(); got > -3*time.Hour+time.Second || got < -3*time.Hour {
		t.Fatalf("applied = %v, want about -3h", got)
	}
}

func TestClockSmallSkewIgnored(t *testing.T) {
	c := useClock(t)
	// 相差 2 秒（加上 Date 取整的 0.5 秒）：小于 5 秒，视为准确
	c.observe(dateHdr(clockT0.Add(2*time.Second)), clockT0, clockT0)
	if got := c.get(); got != 0 {
		t.Fatalf("small skew must not be applied, got %v", got)
	}
	c.observe(dateHdr(clockT0), clockT0, clockT0)
	if got := c.get(); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestClockHysteresis(t *testing.T) {
	c := useClock(t)
	c.observe(dateHdr(clockT0.Add(10*time.Second)), clockT0, clockT0) // 10.5 秒：开始校准
	if c.get() == 0 {
		t.Fatal("10s skew must be applied")
	}
	c.observe(dateHdr(clockT0.Add(3*time.Second)), clockT0, clockT0) // 3.5 秒：已在校准，介于 2 到 5 秒之间，保持
	if got := c.get(); got != 3*time.Second+500*time.Millisecond {
		t.Fatalf("applied should follow the new value while above the keep threshold, got %v", got)
	}
	c.observe(dateHdr(clockT0), clockT0, clockT0) // 0.5 秒：低于 2 秒，取消
	if got := c.get(); got != 0 {
		t.Fatalf("skew must be cleared, got %v", got)
	}
	// 取消后，3.5 秒不足以重新开始校准
	c.observe(dateHdr(clockT0.Add(3*time.Second)), clockT0, clockT0)
	if got := c.get(); got != 0 {
		t.Fatalf("3.5s must not restart calibration, got %v", got)
	}
}

func TestClockRejectsBadSamples(t *testing.T) {
	c := useClock(t)
	c.observe(http.Header{}, clockT0, clockT0)                                                                    // 没有 Date
	bad := http.Header{}
	bad.Set("Date", "not a date")
	c.observe(bad, clockT0, clockT0)                                                                              // 解析失败
	c.observe(dateHdr(clockT0.Add(time.Hour)), clockT0, clockT0.Add(6*time.Second))                               // 往返太久
	c.observe(dateHdr(clockT0.Add(time.Hour)), clockT0.Add(time.Second), clockT0)                                 // 时间倒流
	if c.have || c.get() != 0 {
		t.Fatalf("bad samples must be ignored: have=%v applied=%v", c.have, c.get())
	}
}

func TestClockKeepsLowestRTTSample(t *testing.T) {
	c := useClock(t)
	c.observe(dateHdr(clockT0.Add(time.Hour)), clockT0, clockT0.Add(100*time.Millisecond)) // 往返 0.1 秒：最佳样本
	best := c.get()
	// 往返 3 秒的样本给出的差值略有不同，但不应该替换更准的样本
	c.observe(dateHdr(clockT0.Add(time.Hour+2*time.Second)), clockT0, clockT0.Add(3*time.Second))
	if got := c.get(); got != best {
		t.Fatalf("a slower sample must not replace the best one: %v vs %v", got, best)
	}
}

func TestClockFollowsClockStep(t *testing.T) {
	c := useClock(t)
	c.observe(dateHdr(clockT0.Add(time.Hour)), clockT0, clockT0.Add(50*time.Millisecond))
	// 本机时钟被 NTP 调整：差值突变超过 5 秒，即使往返更长也要采用新值
	c.observe(dateHdr(clockT0), clockT0, clockT0.Add(2*time.Second))
	if got := c.get(); got != 0 {
		t.Fatalf("after the local clock was fixed the skew must drop to 0, got %v", got)
	}
}

func TestClockLogsOnlyOnChange(t *testing.T) {
	c := useClock(t)
	n := 0
	c.logf = func(string, ...any) { n++ }
	for i := 0; i < 5; i++ {
		c.observe(dateHdr(clockT0.Add(time.Hour)), clockT0, clockT0)
	}
	if n != 1 {
		t.Fatalf("expected one log line for a steady skew, got %d", n)
	}
	c.observe(dateHdr(clockT0), clockT0, clockT0)
	if n != 2 {
		t.Fatalf("expected a log line when calibration is cancelled, got %d", n)
	}
}

func TestPeriodKeyWithSkew(t *testing.T) {
	// 本机时钟慢了 2 分钟：本机显示 10 月 31 日 23:59，实际已是 11 月 1 日 00:01，应当进入新周期
	now := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	rule := resetRule{Day: 1, Loc: time.UTC}
	if got := periodKey(now, rule); got != "2026-10-01" {
		t.Fatalf("no skew: %s", got)
	}
	rule.Skew = 2 * time.Minute
	if got := periodKey(now, rule); got != "2026-11-01" {
		t.Fatalf("with skew: %s", got)
	}
}

func TestBuildMetricsBootTimeCalibrated(t *testing.T) {
	c := useClock(t)
	c.applied = time.Hour
	a := snapshot{at: clockT0, btime: 1700000000}
	b := snapshot{at: clockT0.Add(time.Second), btime: 1700000000}
	m := buildMetrics(a, b, defaultConfig(), &state{}, map[string]any{}, map[string]any{})
	if m["boot_time"] != int64(1700000000000+3600000) {
		t.Fatalf("boot_time = %v", m["boot_time"])
	}
	// 读不到 btime（为 0）时不要加校准量，否则会变成一个看似有效的假时间
	a.btime, b.btime = 0, 0
	m = buildMetrics(a, b, defaultConfig(), &state{}, map[string]any{}, map[string]any{})
	if m["boot_time"] != int64(0) {
		t.Fatalf("boot_time without btime = %v", m["boot_time"])
	}
}

func TestBuildMetricsTrafficPeriodUsesSkew(t *testing.T) {
	c := useClock(t)
	c.applied = 2 * time.Minute
	cfg, err := parseConfig("report_interval=60&reset_day=1&timezone=UTC", defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	st := &state{Period: "2026-10-01", Iface: "", RX: 5000, TX: 6000, LastRX: 100, LastTX: 100, Init: true,
		Ifs: one(100, 100)}
	a := snapshot{at: time.Date(2026, 10, 31, 23, 58, 59, 0, time.UTC), ifs: one(100, 100)}
	b := snapshot{at: time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC), ifs: one(200, 200)}
	buildMetrics(a, b, cfg, st, map[string]any{}, map[string]any{})
	if st.Period != "2026-11-01" {
		t.Fatalf("period must follow the calibrated time, got %s", st.Period)
	}
}

func TestSyncClockUsesRootPathAndDate(t *testing.T) {
	c := useClock(t)
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Date", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)) // 模拟 Worker 比本机快 1 小时
		_, _ = w.Write([]byte("CFSM Lite is running."))
	}))
	defer srv.Close()
	rep := newReporter(settings{URL: srv.URL + "/update?x=1", ID: "n1", Secret: "s"})
	rep.syncClock(context.Background())
	if gotPath != "/" || gotQuery != "" {
		t.Fatalf("sync must request the site root, got %q ? %q", gotPath, gotQuery)
	}
	if got := c.get(); got < time.Hour-3*time.Second || got > time.Hour+3*time.Second {
		t.Fatalf("applied = %v, want about 1h", got)
	}
}

func TestSyncClockNetworkErrorIsHarmless(t *testing.T) {
	c := useClock(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // 连接会被拒绝
	rep := newReporter(settings{URL: url + "/update", ID: "n1", Secret: "s"})
	rep.syncClock(context.Background())
	if c.get() != 0 {
		t.Fatalf("applied = %v", c.get())
	}
}
