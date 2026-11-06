package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	text := "cpu  100 20 30 400 50 6 7 8 0 0\ncpu0 1 2 3 4 5 6 7 8 9 10\nbtime 1700000000\n"
	total, idle, bt := parseProcStat(text)
	if total != 100+20+30+400+50+6+7+8 {
		t.Fatalf("total = %d", total)
	}
	if idle != 450 {
		t.Fatalf("idle = %d", idle)
	}
	if bt != 1700000000 {
		t.Fatalf("btime = %d", bt)
	}
}

func TestCPUPercent(t *testing.T) {
	if got := cpuPercent(1000, 800, 1100, 850); got != 50 {
		t.Fatalf("got %v, want 50", got)
	}
	if got := cpuPercent(1000, 800, 1000, 800); got != 0 {
		t.Fatalf("no progress: got %v", got)
	}
	if got := cpuPercent(1000, 800, 900, 800); got != 0 {
		t.Fatalf("total went backwards: got %v", got)
	}
}

func TestRatePerSec(t *testing.T) {
	if got := ratePerSec(2048, 1024, 1); got != 1024 {
		t.Fatalf("got %v", got)
	}
	if got := ratePerSec(100, 5000, 1); got != 0 {
		t.Fatalf("counter reset must give 0, got %v", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	text := "MemTotal:        8192000 kB\nMemFree:          100000 kB\nMemAvailable:    4096000 kB\nSwapTotal:       2048000 kB\nSwapFree:        2000000 kB\n"
	m := parseMeminfo(text)
	if m.RAMTotal != 8000 || m.RAMUsed != 4000 || m.SwapTotal != 2000 || m.SwapUsed != 46 {
		t.Fatalf("unexpected: %+v", m)
	}
	old := "MemTotal: 1024000 kB\nMemFree: 24000 kB\nBuffers: 100000 kB\nCached: 200000 kB\n"
	if m2 := parseMeminfo(old); m2.RAMUsed != 683 {
		t.Fatalf("fallback without MemAvailable: %+v", m2)
	}
}

const netDevSample = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999 10 0 0 0 0 0 0 999999 10 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 2000 10 0 0 0 0 0 0
  ens3: 300 10 0 0 0 0 0 0 400 10 0 0 0 0 0 0
docker0: 5000 10 0 0 0 0 0 0 6000 10 0 0 0 0 0 0
 veth1a: 7000 10 0 0 0 0 0 0 8000 10 0 0 0 0 0 0
`

func TestParseNetDev(t *testing.T) {
	rx, tx := parseNetDev(netDevSample, map[string]bool{})
	if rx != 1300 || tx != 2400 {
		t.Fatalf("auto: rx=%d tx=%d", rx, tx)
	}
	rx, tx = parseNetDev(netDevSample, splitIfaces("eth0, docker0"))
	if rx != 6000 || tx != 8000 {
		t.Fatalf("only: rx=%d tx=%d", rx, tx)
	}
}

func TestCountSockets(t *testing.T) {
	text := "  sl  local_address rem_address   st\n   0: 0100007F:1F90 00000000:0000 0A x\n   1: 0100007F:1F91 0100007F:C350 01 x\n   2: 0100007F:1F92 0100007F:C351 01 x\n"
	if got := countSockets(text, true); got != 2 {
		t.Fatalf("established = %d", got)
	}
	if got := countSockets(text, false); got != 3 {
		t.Fatalf("all = %d", got)
	}
}

func TestOSAndCPU(t *testing.T) {
	if got := osPrettyName("NAME=Ubuntu\nPRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\n"); got != "Ubuntu 22.04.4 LTS" {
		t.Fatalf("os = %q", got)
	}
	deb := "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\nVERSION_CODENAME=trixie\nID=debian\n"
	for _, c := range []struct{ rel, ver, want string }{
		{deb, "13.1\n", "Debian 13.1 (trixie)"},
		{deb, "13.0", "Debian 13.0 (trixie)"},
		// testing/sid：没有数字版本号，沿用 PRETTY_NAME
		{deb, "trixie/sid\n", "Debian GNU/Linux 13 (trixie)"},
		// 读不到 debian_version
		{deb, "", "Debian GNU/Linux 13 (trixie)"},
		{"PRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\nID=ubuntu\n", "13.1", "Ubuntu 22.04.4 LTS"},
	} {
		if got := osDisplayName(c.rel, c.ver); got != c.want {
			t.Fatalf("osDisplayName(%q, %q) = %q, want %q", c.rel, c.ver, got, c.want)
		}
	}
	if got := cpuModel("processor : 0\nmodel name\t: Intel Xeon\n"); got != "Intel Xeon" {
		t.Fatalf("cpu = %q", got)
	}
	if got := parseLoadavg("0.10 0.20 0.30 1/200 999\n"); got != "0.10 0.20 0.30" {
		t.Fatalf("load = %q", got)
	}
}

func TestParseConfig(t *testing.T) {
	cur := defaultConfig()
	body := "collect_interval=0&report_interval=120&reset_day=15&schema_version=6&custom_ct=example.com:443&custom_cu=1.1.1.1&custom_cm=gd-cm.example.com&custom_bd=ip.example.com&interface=eth0,ens3&connection_mode=http&wss_report_interval=2&ping_mode=icmp"
	next, err := parseConfig(body, cur)
	if err != nil {
		t.Fatal(err)
	}
	if next.ReportInterval != 120*time.Second || next.ResetDay != 15 || next.Interface != "eth0,ens3" || next.PingMode != "icmp" {
		t.Fatalf("unexpected: %+v", next)
	}
	if next.Targets["ct"] != "example.com:443" || next.Targets["cu"] != "1.1.1.1" {
		t.Fatalf("targets: %+v", next.Targets)
	}
	if cur.Targets["ct"] == "example.com:443" {
		t.Fatal("parseConfig must not modify the original config")
	}
	for _, bad := range []string{
		"report_interval=45",
		"reset_day=40",
		"ping_mode=udp",
		"custom_ct=bad host",
		"interface=eth0;rm",
		"",
		strings.Repeat("a=b&", 300),
	} {
		if _, err := parseConfig(bad, cur); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestMD5Hex(t *testing.T) {
	if got := md5Hex("abc"); got != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("md5 = %s", got)
	}
}

func TestPeriodKey(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		now      time.Time
		resetDay int
		want     string
	}{
		{time.Date(2026, 10, 31, 12, 0, 0, 0, utc), 1, "2026-10-01"},
		{time.Date(2026, 10, 10, 12, 0, 0, 0, utc), 15, "2026-09-15"},
		{time.Date(2026, 10, 15, 0, 0, 1, 0, utc), 15, "2026-10-15"},
		{time.Date(2026, 3, 5, 12, 0, 0, 0, utc), 31, "2026-02-28"},
		{time.Date(2026, 2, 28, 12, 0, 0, 0, utc), 31, "2026-02-28"},
		{time.Date(2026, 1, 5, 12, 0, 0, 0, utc), 10, "2025-12-10"},
		{time.Date(2026, 10, 31, 12, 0, 0, 0, utc), 0, "none"},
	}
	for _, c := range cases {
		if got := periodKey(c.now, resetRule{Day: c.resetDay}); got != c.want {
			t.Errorf("periodKey(%v, %d) = %s, want %s", c.now, c.resetDay, got, c.want)
		}
	}
}

func TestStateAdvance(t *testing.T) {
	day1 := resetRule{Day: 1}
	s := &state{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.advance(one(1000, 2000), t0, day1, "")
	if s.RX != 0 || s.TX != 0 {
		t.Fatalf("first sample only sets the baseline: %+v", s)
	}
	s.advance(one(1500, 2600), t0.Add(time.Minute), day1, "")
	if s.RX != 500 || s.TX != 600 {
		t.Fatalf("after delta: %+v", s)
	}
	s.advance(one(100, 50), t0.Add(2*time.Minute), day1, "") // 计数器被重置
	if s.RX != 600 || s.TX != 650 {
		t.Fatalf("after reset: %+v", s)
	}
	s.advance(one(200, 80), time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC), day1, "") // 新周期，累计清零后再加增量
	if s.Period != "2026-11-01" || s.RX != 100 || s.TX != 30 {
		t.Fatalf("new period: %+v", s)
	}
	s.advance(one(999999, 999999), time.Date(2026, 11, 1, 0, 6, 0, 0, time.UTC), day1, "eth0") // 网卡配置变化：重建基线，不产生巨大增量
	if s.RX != 100 || s.TX != 30 {
		t.Fatalf("iface change must not add traffic: %+v", s)
	}
}

func TestStateSaveLoad(t *testing.T) {
	path := t.TempDir() + "/sub/state.json"
	s := &state{Period: "2026-10-01", RX: 5, TX: 6, LastRX: 7, LastTX: 8, Init: true}
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	got := loadState(path)
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("got %+v want %+v", got, s)
	}
	if bad := loadState(t.TempDir() + "/missing.json"); bad.Init {
		t.Fatal("missing file must give empty state")
	}
}

func TestNormalizeTarget(t *testing.T) {
	cases := []struct {
		in, host, port string
		ok             bool
	}{
		{"example.com", "example.com", "80", true},
		{"example.com:443", "example.com", "443", true},
		{"1.1.1.1:53", "1.1.1.1", "53", true},
		{"[2001:db8::1]:443", "2001:db8::1", "443", true},
		{"2001:db8::1", "2001:db8::1", "80", true},
		{"example.com:99999", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		h, p, ok := normalizeTarget(c.in)
		if ok != c.ok || h != c.host || p != c.port {
			t.Errorf("normalizeTarget(%q) = %q,%q,%v want %q,%q,%v", c.in, h, p, ok, c.host, c.port, c.ok)
		}
	}
}

func TestTCPPing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d, err := tcpPing(context.Background(), "127.0.0.1", port)
	if err != nil || d <= 0 {
		t.Fatalf("listening port: d=%v err=%v", d, err)
	}
	_ = ln.Close()
	// 端口已关闭：与 komari 一致，连接被拒绝记为失败
	if _, err := tcpPing(context.Background(), "127.0.0.1", port); err == nil {
		t.Fatal("refused connection must be a failed sample")
	}
}

// 逐次返回预设结果的 measure，用来验证 measureSample 与 komari 的处理一致。
func scripted(steps ...any) (func() (time.Duration, error), *int) {
	n := 0
	return func() (time.Duration, error) {
		if n >= len(steps) {
			panic("measure called more often than scripted")
		}
		s := steps[n]
		n++
		if err, ok := s.(error); ok {
			return 0, err
		}
		return s.(time.Duration), nil
	}, &n
}

func TestMeasureSample(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	boom := errors.New("boom")
	cases := []struct {
		name  string
		steps []any
		want  time.Duration
		err   error
		calls int
	}{
		{"正常：只测一次", []any{ms(180)}, ms(180), nil, 1},
		{"刚好 1000ms 不重测", []any{ms(1000)}, ms(1000), nil, 1},
		{"首次失败：不重测，记丢包", []any{boom}, 0, boom, 1},
		{"SYN 重传：首次 1200，重测 200，差 1000 > 800 -> 丢包", []any{ms(1200), ms(200)}, 0, errRetransmit, 2},
		{"确实偏高：首次 1100，重测 900，差 200 -> 采用重测值", []any{ms(1100), ms(900)}, ms(900), nil, 2},
		{"差值刚好 800 不算重传", []any{ms(1500), ms(700)}, ms(700), nil, 2},
		{"重测失败 -> 丢包", []any{ms(1500), boom}, 0, boom, 2},
		{"三次重测都偏高 -> 丢包", []any{ms(1500), ms(1400), ms(1300), ms(1200)}, 0, errStillHigh, 4},
	}
	for _, c := range cases {
		m, n := scripted(c.steps...)
		got, err := measureSample(m)
		if err != c.err || got != c.want || *n != c.calls {
			t.Errorf("%s: got (%v, %v) after %d calls, want (%v, %v) after %d calls", c.name, got, err, *n, c.want, c.err, c.calls)
		}
	}
	// 前两次重测仍偏高、第三次才回落到 1000ms 以内：只比首次少 600ms，不算重传，采用回落后的值
	m, n := scripted(ms(1500), ms(1400), ms(1300), ms(900))
	if got, err := measureSample(m); err != nil || got != ms(900) || *n != 4 {
		t.Errorf("third retry: got (%v, %v) after %d calls", got, err, *n)
	}
}

func TestSummarize(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	at := func(secAgo int) time.Time { return now.Add(-time.Duration(secAgo) * time.Second) }
	ok := func(secAgo int, ms float64) pingSample { return pingSample{at: at(secAgo), ms: ms, ok: true} }
	bad := func(secAgo int) pingSample { return pingSample{at: at(secAgo)} }

	if _, _, has := summarize(nil, now); has {
		t.Fatal("no samples must report ok=false")
	}
	// 10 个样本里 2 个失败：丢包 20%（1 小时窗口）；最近 10 分钟内的成功样本取平均值（210+190+200+205+195 = 1000，共 5 个）
	samples := []pingSample{ok(2000, 900), bad(1500), ok(1000, 800), ok(900, 700), bad(800), ok(50, 210), ok(40, 190), ok(30, 200), ok(20, 205), ok(10, 195)}
	lat, loss, has := summarize(samples, now)
	if !has || loss != 20 || lat != 200.0 {
		t.Fatalf("lat=%v loss=%v ok=%v", lat, loss, has)
	}
	// 平均值：与 komari 面板一致，异常高的样本会拉高平均值
	lat, _, _ = summarize([]pingSample{ok(30, 100), ok(20, 200), ok(10, 600)}, now)
	if lat != 300.0 {
		t.Fatalf("mean = %v", lat)
	}
	// 最近 10 分钟全部失败：延迟为 nil（超时），丢包率仍按 1 小时窗口算
	lat, loss, _ = summarize([]pingSample{ok(1300, 100), ok(1200, 100), bad(30), bad(20), bad(10), bad(5)}, now)
	if lat != nil || loss != round1(400.0/6) {
		t.Fatalf("lat=%v loss=%v", lat, loss)
	}
	// 窗口外的样本不计入
	lat, loss, has = summarize([]pingSample{bad(4000), ok(10, 50)}, now)
	if !has || loss != 0 || lat != 50.0 {
		t.Fatalf("old samples counted: lat=%v loss=%v", lat, loss)
	}
	if _, _, has := summarize([]pingSample{bad(4000)}, now); has {
		t.Fatal("only samples outside the window must report ok=false")
	}
}

func TestPingerPublish(t *testing.T) {
	sh := &sharedData{}
	p := newPinger(sh)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	cfg := defaultConfig()
	cfg.Targets = map[string]string{"ct": "a.example:80", "cu": "", "cm": "", "bd": ""}

	// 还没有任何样本：未配置的槽位是 false，已配置的槽位暂时没有结果
	for _, k := range slotKeys {
		if k == "ct" {
			p.setTarget(k, "a.example:80")
		} else {
			p.setTarget(k, "")
		}
	}
	m := sh.getPing()
	if m["ping_cu"] != false || m["loss_cu"] != false {
		t.Fatalf("unconfigured slot must be false: %+v", m)
	}
	if _, has := m["ping_ct"]; has || pingReady(m, cfg) {
		t.Fatalf("configured slot without samples must be missing and not ready: %+v", m)
	}
	// 目标不对的样本会被丢弃
	p.record("ct", "other:80", now, 10*time.Millisecond, true)
	if _, has := sh.getPing()["ping_ct"]; has {
		t.Fatal("sample for a stale target must be dropped")
	}
	// 收集样本：一次失败、三次成功
	p.record("ct", "a.example:80", now.Add(-30*time.Second), 0, false)
	p.record("ct", "a.example:80", now.Add(-20*time.Second), 200*time.Millisecond, true)
	p.record("ct", "a.example:80", now.Add(-10*time.Second), 210*time.Millisecond, true)
	p.record("ct", "a.example:80", now, 190*time.Millisecond, true)
	m = sh.getPing()
	if m["ping_ct"] != 200.0 || m["loss_ct"] != 25.0 || !pingReady(m, cfg) {
		t.Fatalf("unexpected: %+v", m)
	}
	// 目标改了：旧样本清掉，要重新等第一个样本
	p.setTarget("ct", "b.example:443")
	m = sh.getPing()
	if _, has := m["ping_ct"]; has {
		t.Fatalf("old samples must be cleared when the target changes: %+v", m)
	}
	// 目标被清空：变成未配置
	p.setTarget("ct", "")
	if m = sh.getPing(); m["ping_ct"] != false {
		t.Fatalf("cleared slot must be false: %+v", m)
	}
	// 窗口内全部失败：延迟为 nil，丢包 100
	p.setTarget("ct", "a.example:80")
	p.record("ct", "a.example:80", now, 0, false)
	m = sh.getPing()
	if v, has := m["ping_ct"]; !has || v != nil || m["loss_ct"] != 100.0 {
		t.Fatalf("all failed: %+v", m)
	}
}

func TestPingerKeepsBoundedSamples(t *testing.T) {
	p := newPinger(&sharedData{})
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	p.setTarget("ct", "a.example:80")
	for i := 0; i < pingMaxSamples+50; i++ {
		p.record("ct", "a.example:80", now, time.Millisecond, true)
	}
	if n := len(p.hist["ct"].samples); n != pingMaxSamples {
		t.Fatalf("samples = %d, want %d", n, pingMaxSamples)
	}
	// 超过丢包窗口的样本会被清掉
	later := now.Add(pingLossWindow + time.Minute)
	p.now = func() time.Time { return later }
	p.record("ct", "a.example:80", later, time.Millisecond, true)
	if n := len(p.hist["ct"].samples); n != 1 {
		t.Fatalf("samples after window = %d, want 1", n)
	}
}

func TestParseSettings(t *testing.T) {
	good := []string{"-url", "https://example.com/update", "-id", "abc-123", "-secret", "s3cret"}
	s, _, err := parseSettings(good)
	if err != nil || s.ID != "abc-123" {
		t.Fatalf("good: %+v %v", s, err)
	}
	for name, args := range map[string][]string{
		"missing secret": {"-url", "https://example.com/update", "-id", "abc"},
		"http url":       {"-url", "http://example.com/update", "-id", "abc", "-secret", "x"},
		"bad id":         {"-url", "https://example.com/update", "-id", "a b", "-secret", "x"},
		"no host":        {"-url", "https:///update", "-id", "abc", "-secret", "x"},
	} {
		if _, _, err := parseSettings(args); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, _, err := parseSettings(append([]string{"-allow-http"}, "-url", "http://127.0.0.1:8787/update", "-id", "abc", "-secret", "x")); err != nil {
		t.Errorf("allow-http: %v", err)
	}
	if _, v, _ := parseSettings([]string{"-version"}); !v {
		t.Error("-version should be recognized")
	}
}

func TestReporterSend(t *testing.T) {
	var gotHeader http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rep := newReporter(settings{URL: srv.URL, ID: "abc", Secret: "s3cret"})
	res, err := rep.send(context.Background(), map[string]any{"cpu": 1.5, "ping_ct": nil, "loss_cm": false}, "none")
	if err != nil || res.Status != http.StatusNoContent {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if gotHeader.Get("X-Agent-Config-Schema") != "7" || gotHeader.Get("X-Agent-Config-Md5") != "none" {
		t.Fatalf("headers: %v", gotHeader)
	}
	if !strings.HasPrefix(gotHeader.Get("X-Agent-Version"), "lite-go-") {
		t.Fatalf("version header: %q", gotHeader.Get("X-Agent-Version"))
	}
	if gotBody["id"] != "abc" || gotBody["secret"] != "s3cret" {
		t.Fatalf("body: %+v", gotBody)
	}
	m, _ := gotBody["metrics"].(map[string]any)
	if m == nil || m["ping_ct"] != nil || m["loss_cm"] != false {
		t.Fatalf("metrics: %+v", gotBody["metrics"])
	}
}

func TestReporterDoesNotFollowRedirect(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	rep := newReporter(settings{URL: srv.URL, ID: "abc", Secret: "s3cret"})
	res, err := rep.send(context.Background(), map[string]any{}, "none")
	if err != nil || res.Status != http.StatusTemporaryRedirect || hit {
		t.Fatalf("redirect must not be followed: res=%+v err=%v hit=%v", res, err, hit)
	}
}

const mountsSample = `/dev/vda1 / ext4 rw,relatime 0 0
/dev/vda1 /usr ext4 ro,relatime 0 0
/dev/vda15 /boot/efi vfat rw 0 0
/dev/vdb1 /data xfs rw 0 0
/dev/vdb1 /var/lib/docker/volumes xfs rw 0 0
/dev/mapper/vg-home /home ext4 rw 0 0
/dev/loop3 /snap/core20/1974 squashfs ro 0 0
/dev/sdc1 /mnt/usb ntfs3 rw 0 0
/dev/sdd1 /mnt/windows fuseblk rw 0 0
10.0.0.2:/export/share /mnt/nfs nfs4 rw 0 0
//nas/share /mnt/cifs cifs rw 0 0
sshfs#user@host:/ /mnt/sshfs fuse.sshfs rw 0 0
/dev/nbd0 /mnt/nbd ext4 rw 0 0
tmpfs /run tmpfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
proc /proc proc rw 0 0
/dev/sde1 /mnt/with\040space ext4 rw 0 0
`

func TestUnescapeMount(t *testing.T) {
	cases := map[string]string{
		`/mnt/with\040space`: "/mnt/with space",
		`/a\134b`:            `/a\b`,
		`/plain`:             "/plain",
		`/bad\9zzx`:          `/bad\9zzx`,
	}
	for in, want := range cases {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelectDiskMounts(t *testing.T) {
	got := selectDiskMounts(parseMounts(mountsSample))
	var devs []string
	for _, m := range got {
		devs = append(devs, m.Device)
	}
	want := []string{"/dev/vda1", "/dev/vda15", "/dev/vdb1", "/dev/mapper/vg-home", "/dev/sdc1", "/dev/sdd1", "/dev/sde1"}
	if strings.Join(devs, ",") != strings.Join(want, ",") {
		t.Fatalf("selected devices = %v, want %v", devs, want)
	}
	if got[0].Point != "/" || got[2].Point != "/data" || got[len(got)-1].Point != "/mnt/with space" {
		t.Fatalf("unexpected mount points: %+v", got)
	}
}

func TestSumDisks(t *testing.T) {
	sizes := map[string][2]uint64{
		"/":         {100 << 30, 40 << 30},
		"/data":     {200 << 30, 50 << 30},
		"/boot/efi": {512 << 20, 10 << 20},
		"/mnt/usb":  {0, 0},
	}
	stat := func(path string) (uint64, uint64, bool) {
		v, ok := sizes[path]
		return v[0], v[1], ok
	}
	total, used := sumDisks(selectDiskMounts(parseMounts(mountsSample)), stat)
	// 其余挂载点没有数据（stat 失败）会被跳过：总量 = 100GB + 200GB + 512MB
	if total != 300*1024+512 || used != 90*1024+10 {
		t.Fatalf("total=%d used=%d", total, used)
	}
}

func TestPeriodKeyResetTime(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, sh) }
	cases := []struct {
		now  time.Time
		rule resetRule
		want string
	}{
		{at(2026, 11, 1, 0, 0), resetRule{1, 0, 0, sh}, "2026-11-01"},
		{at(2026, 11, 1, 8, 29), resetRule{1, 8, 30, sh}, "2026-10-01T08:30"}, // 还没到 8:30
		{at(2026, 11, 1, 8, 30), resetRule{1, 8, 30, sh}, "2026-11-01T08:30"}, // 刚好到点
		{at(2026, 11, 15, 23, 59), resetRule{15, 23, 59, sh}, "2026-11-15T23:59"},
		{at(2026, 3, 31, 5, 0), resetRule{31, 6, 0, sh}, "2026-02-28T06:00"}, // 3 月 31 日 6 点前仍属 2 月周期
		{at(2026, 1, 1, 1, 0), resetRule{1, 2, 0, sh}, "2025-12-01T02:00"},   // 跨年
		{at(2026, 11, 1, 8, 30), resetRule{0, 8, 30, sh}, "none"},
		// 传入的时间用别的时区表示，结果仍按规则里的时区算：UTC 10 月 31 日 16:00 就是上海 11 月 1 日 0:00
		{time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC), resetRule{1, 0, 0, sh}, "2026-11-01"},
		{time.Date(2026, 10, 31, 15, 59, 0, 0, time.UTC), resetRule{1, 0, 0, sh}, "2026-10-01"},
	}
	for _, c := range cases {
		if got := periodKey(c.now, c.rule); got != c.want {
			t.Errorf("periodKey(%v, %+v) = %s, want %s", c.now, c.rule, got, c.want)
		}
	}
}

func TestParseConfigResetAndCalibration(t *testing.T) {
	cur := defaultConfig()
	if cur.TZName != "Asia/Shanghai" || cur.Loc == nil || cur.ResetHour != 0 || cur.ResetMinute != 0 {
		t.Fatalf("defaults: %+v", cur)
	}
	body := "report_interval=60&reset_day=5&reset_hour=8&reset_minute=5&timezone=America/New_York&cal_id=1760000000&cal_rx=1073741824&cal_tx=2147483648&cal_period=2026-11-05T08:05"
	next, err := parseConfig(body, cur)
	if err != nil {
		t.Fatal(err)
	}
	if next.ResetDay != 5 || next.ResetHour != 8 || next.ResetMinute != 5 || next.TZName != "America/New_York" || next.Loc.String() != "America/New_York" {
		t.Fatalf("unexpected: %+v", next)
	}
	if next.CalID != "1760000000" || next.CalRX != 1<<30 || next.CalTX != 2<<30 || next.CalPeriod != "2026-11-05T08:05" {
		t.Fatalf("calibration: %+v", next)
	}
	// 没有 cal_id：待应用的校准被清掉
	again, err := parseConfig("report_interval=60&reset_day=5", next)
	if err != nil {
		t.Fatal(err)
	}
	if again.CalID != "" || again.CalRX != 0 || again.CalTX != 0 || again.CalPeriod != "" {
		t.Fatalf("calibration should be cleared: %+v", again)
	}
	if again.TZName != "America/New_York" || again.ResetHour != 8 {
		t.Fatalf("other fields must be kept: %+v", again)
	}
	for _, bad := range []string{
		"report_interval=60&reset_hour=24",
		"report_interval=60&reset_minute=60",
		"report_interval=60&reset_hour=x",
		"report_interval=60&timezone=Mars/Base",
		"report_interval=60&timezone=+08:00",
		"report_interval=60&cal_id=1&cal_rx=1",                                  // 缺 cal_tx 和 cal_period
		"report_interval=60&cal_id=1&cal_rx=-1&cal_tx=1&cal_period=none",        // 负数
		"report_interval=60&cal_id=1&cal_rx=1&cal_tx=1&cal_period=bad",          // 周期格式不对
		"report_interval=60&cal_id=abc&cal_rx=1&cal_tx=1&cal_period=2026-11-01", // 编号不是数字
	} {
		if _, err := parseConfig(bad, cur); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestStateCalibrate(t *testing.T) {
	rule := resetRule{Day: 1}
	s := &state{}
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.advance(one(1000, 1000), t0, rule, "")
	s.advance(one(1500, 1700), t0.Add(time.Minute), rule, "")
	cfg := defaultConfig()
	cfg.CalID, cfg.CalRX, cfg.CalTX, cfg.CalPeriod = "100", 5000, 9000, "2026-11-01"
	s.calibrate(cfg)
	if s.RX != 5000 || s.TX != 9000 || s.CalID != "100" {
		t.Fatalf("calibration not applied: %+v", s)
	}
	s.advance(one(1600, 1800), t0.Add(2*time.Minute), rule, "") // 校准之后继续累计
	if s.RX != 5100 || s.TX != 9100 {
		t.Fatalf("must keep accumulating from the calibrated value: %+v", s)
	}
	s.calibrate(cfg) // 同一个编号只应用一次
	if s.RX != 5100 || s.TX != 9100 {
		t.Fatalf("same cal_id must not be applied twice: %+v", s)
	}
	cfg.CalID, cfg.CalPeriod = "101", "2026-10-01" // 不是当前周期的校准：忽略
	s.calibrate(cfg)
	if s.RX != 5100 || s.CalID != "100" {
		t.Fatalf("stale-period calibration must be ignored: %+v", s)
	}
	cfg.CalPeriod = "2026-11-01"
	cfg.CalRX, cfg.CalTX = 0, 0 // 新的编号：可以再次校准，填 0 也有效
	s.calibrate(cfg)
	if s.RX != 0 || s.TX != 0 || s.CalID != "101" {
		t.Fatalf("recalibration failed: %+v", s)
	}
}

// 实时协程已经用较新的读数累计过，HTTP 上报循环随后拿着较早采到的读数再来累计：
// 较早的读数必须被丢弃，不能被当成计数器重置而把整个网卡累计值加进当月流量。
func TestStateAdvanceIgnoresStaleSnapshot(t *testing.T) {
	rule := resetRule{Day: 1}
	s := &state{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.advance(one(1_000_000_000, 500_000_000), t0, rule, "")
	s.advance(one(1_000_001_000, 500_000_500), t0.Add(6*time.Second), rule, "") // 实时帧：较新的读数
	s.advance(one(1_000_000_400, 500_000_200), t0.Add(time.Second), rule, "")   // HTTP 循环：几秒前采的读数
	if s.RX != 1000 || s.TX != 500 {
		t.Fatalf("stale snapshot must be ignored, got RX=%d TX=%d", s.RX, s.TX)
	}
	s.advance(one(1_000_001_600, 500_000_900), t0.Add(8*time.Second), rule, "") // 之后的正常读数继续累计
	if s.RX != 1600 || s.TX != 900 {
		t.Fatalf("after stale: RX=%d TX=%d", s.RX, s.TX)
	}
	s.advance(one(100, 50), t0.Add(9*time.Second), rule, "") // 真正的计数器重置（读数是新的）仍按重置处理
	if s.RX != 1700 || s.TX != 950 {
		t.Fatalf("real reset: RX=%d TX=%d", s.RX, s.TX)
	}
}

// 重启后用状态文件里保存的服务端配置接着累计：重置日不是默认值（1 日）、统计网卡不是默认值时，当月流量不能被清零。
func TestRestartKeepsMonthlyTraffic(t *testing.T) {
	path := t.TempDir() + "/state.json"
	body := "report_interval=60&reset_day=5&interface=eth0"
	old := &state{Period: "2026-11-05", Iface: "eth0", RX: 1000, TX: 2000, LastRX: 5000, LastTX: 6000, Init: true}
	old.setCfg(body)
	if err := old.save(path); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新读状态文件，用其中保存的配置，而不是默认配置
	st := loadState(path)
	cfg, err := parseConfig(st.cachedCfg(), defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Synced || cfg.ResetDay != 5 || cfg.Interface != "eth0" {
		t.Fatalf("restored config: %+v", cfg)
	}
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := snapshot{at: t0, rx: 5000, tx: 6000, ifs: one(5000, 6000)}
	b := snapshot{at: t0.Add(time.Second), rx: 5500, tx: 6300, ifs: one(5500, 6300)}
	m := buildMetrics(a, b, cfg, st, map[string]any{}, map[string]any{})
	if m["net_rx_monthly"] != uint64(1500) || m["net_tx_monthly"] != uint64(2300) {
		t.Fatalf("monthly traffic must continue, got rx=%v tx=%v", m["net_rx_monthly"], m["net_tx_monthly"])
	}
}

// 还没拿到服务端配置（例如从没保存过配置的旧版本升级）时，不能用默认配置累计，也不能清零已有的累计。
func TestUnsyncedConfigKeepsState(t *testing.T) {
	st := &state{Period: "2026-11-05", Iface: "eth0", RX: 1000, TX: 2000, LastRX: 5000, LastTX: 6000, Init: true}
	cfg := defaultConfig()
	if cfg.Synced {
		t.Fatal("default config must not count as synced")
	}
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := snapshot{at: t0, rx: 5000, tx: 6000, ifs: one(5000, 6000)}
	b := snapshot{at: t0.Add(time.Second), rx: 5500, tx: 6300, ifs: one(5500, 6300)}
	m := buildMetrics(a, b, cfg, st, map[string]any{}, map[string]any{})
	if m["net_rx_monthly"] != uint64(1000) || m["net_tx_monthly"] != uint64(2000) {
		t.Fatalf("unsynced run must report stored totals, got rx=%v tx=%v", m["net_rx_monthly"], m["net_tx_monthly"])
	}
	if st.Period != "2026-11-05" || st.Iface != "eth0" || !st.Init || st.LastRX != 5000 || st.LastTX != 6000 {
		t.Fatalf("state must be untouched before config sync: %+v", st)
	}
	// 配置同步之后的第一次累计，把这段时间网卡计数器的增量一起补上
	synced, err := parseConfig("report_interval=60&reset_day=5&interface=eth0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = buildMetrics(a, b, synced, st, map[string]any{}, map[string]any{})
	if m["net_rx_monthly"] != uint64(1500) || m["net_tx_monthly"] != uint64(2300) {
		t.Fatalf("after sync got rx=%v tx=%v", m["net_rx_monthly"], m["net_tx_monthly"])
	}
}

// 服务端配置原文随状态文件保存；没有变化时不会重复标记为「待写入」。
func TestStateCfgPersisted(t *testing.T) {
	path := t.TempDir() + "/state.json"
	s := &state{}
	s.setCfg("report_interval=60&reset_day=5")
	if !s.isDirty() {
		t.Fatal("new config must mark state dirty")
	}
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	if s.isDirty() {
		t.Fatal("save must clear dirty")
	}
	s.setCfg("report_interval=60&reset_day=5")
	if s.isDirty() {
		t.Fatal("same config must not mark dirty")
	}
	if got := loadState(path).cachedCfg(); got != "report_interval=60&reset_day=5" {
		t.Fatalf("cached config = %q", got)
	}
}

// one 生成只有一块网卡的读数，给只关心合计值的测试用。
func one(rx, tx uint64) map[string]ifCounters {
	return map[string]ifCounters{"eth0": {RX: rx, TX: tx}}
}

// 网卡增减（虚拟机、容器、VPN 隧道起停）时，合计值会跳变，但累计流量只能按各网卡自己的增量算。
func TestStateAdvancePerInterface(t *testing.T) {
	rule := resetRule{Day: 1}
	s := &state{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	two := func(e, v uint64) map[string]ifCounters {
		return map[string]ifCounters{"eth0": {RX: e, TX: e}, "vnet0": {RX: v, TX: v}}
	}
	s.advance(two(50_000_000_000, 1_000_000_000), t0, rule, "")
	s.advance(two(50_000_001_000, 1_000_000_500), t0.Add(time.Minute), rule, "")
	if s.RX != 1500 || s.TX != 1500 {
		t.Fatalf("两块网卡各自累计: %+v", s)
	}
	// vnet0 消失：合计值从 51GB 变成 50GB，不能被当成计数器重置而把 50GB 加进当月流量
	s.advance(one(50_000_002_000, 50_000_002_000), t0.Add(2*time.Minute), rule, "")
	if s.RX != 2500 || s.TX != 2500 {
		t.Fatalf("网卡消失不能产生假流量: RX=%d TX=%d", s.RX, s.TX)
	}
	// 新网卡出现：它从创建起的流量都是新增的
	s.advance(two(50_000_003_000, 700), t0.Add(3*time.Minute), rule, "")
	if s.RX != 4200 || s.TX != 4200 {
		t.Fatalf("新网卡的流量要算进来: RX=%d TX=%d", s.RX, s.TX)
	}
	// 单块网卡的计数器被重置，仍按重置处理
	s.advance(two(100, 800), t0.Add(4*time.Minute), rule, "")
	if s.RX != 4200+100+100 || s.TX != 4200+100+100 {
		t.Fatalf("单块网卡重置: RX=%d TX=%d", s.RX, s.TX)
	}
}

// 读取 /proc/net/dev 失败得到空读数时必须忽略，不能把基线清成 0。
func TestStateAdvanceIgnoresEmptyReading(t *testing.T) {
	rule := resetRule{Day: 1}
	s := &state{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.advance(one(9_000_000_000, 9_000_000_000), t0, rule, "")
	s.advance(nil, t0.Add(time.Minute), rule, "")
	s.advance(one(9_000_000_100, 9_000_000_200), t0.Add(2*time.Minute), rule, "")
	if s.RX != 100 || s.TX != 200 {
		t.Fatalf("空读数不能破坏基线: RX=%d TX=%d", s.RX, s.TX)
	}
}

// 旧版本的状态文件只有合计值：升级后的第一次累计按合计值算，不丢也不多算，之后自动带上每块网卡的读数。
func TestStateAdvanceMigratesOldState(t *testing.T) {
	rule := resetRule{Day: 1}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s := &state{Period: "2026-10-01", RX: 1000, TX: 2000, LastRX: 5000, LastTX: 6000, Init: true}
	s.advance(one(5400, 6300), t0, rule, "")
	if s.RX != 1400 || s.TX != 2300 || len(s.Ifs) != 1 {
		t.Fatalf("migration: %+v", s)
	}
	s.advance(one(5500, 6400), t0.Add(time.Minute), rule, "")
	if s.RX != 1500 || s.TX != 2400 {
		t.Fatalf("after migration: %+v", s)
	}
}

func TestParseNetDevIfaces(t *testing.T) {
	got := parseNetDevIfaces(netDevSample, map[string]bool{})
	if len(got) != 2 || got["eth0"] != (ifCounters{RX: 1000, TX: 2000}) || got["ens3"] != (ifCounters{RX: 300, TX: 400}) {
		t.Fatalf("auto: %+v", got)
	}
}
