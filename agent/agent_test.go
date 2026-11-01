package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
		strings.Repeat("a=b&", 200),
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
		if got := periodKey(c.now, c.resetDay); got != c.want {
			t.Errorf("periodKey(%v, %d) = %s, want %s", c.now, c.resetDay, got, c.want)
		}
	}
}

func TestStateAdvance(t *testing.T) {
	s := &state{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.advance(1000, 2000, t0, 1, "")
	if s.RX != 0 || s.TX != 0 {
		t.Fatalf("first sample only sets the baseline: %+v", s)
	}
	s.advance(1500, 2600, t0.Add(time.Minute), 1, "")
	if s.RX != 500 || s.TX != 600 {
		t.Fatalf("after delta: %+v", s)
	}
	s.advance(100, 50, t0.Add(2*time.Minute), 1, "") // 计数器被重置
	if s.RX != 600 || s.TX != 650 {
		t.Fatalf("after reset: %+v", s)
	}
	s.advance(200, 80, time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC), 1, "") // 新周期，累计清零后再加增量
	if s.Period != "2026-11-01" || s.RX != 100 || s.TX != 30 {
		t.Fatalf("new period: %+v", s)
	}
	s.advance(999999, 999999, time.Date(2026, 11, 1, 0, 6, 0, 0, time.UTC), 1, "eth0") // 网卡配置变化：重建基线，不产生巨大增量
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
	if *got != *s {
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

func TestProbeTarget(t *testing.T) {
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
	avg, ok := probeTarget(context.Background(), "127.0.0.1", port)
	if ok != pingProbes || avg <= 0 {
		t.Fatalf("listening port: avg=%v ok=%d", avg, ok)
	}
	_ = ln.Close()
	// 端口已关闭：连接被拒绝也说明网络是通的，按成功计
	if _, ok := probeTarget(context.Background(), "127.0.0.1", port); ok != pingProbes {
		t.Fatalf("refused connection should count as reachable, ok=%d", ok)
	}
}

func TestStartPingShape(t *testing.T) {
	cfg := defaultConfig()
	cfg.Targets = map[string]string{"ct": "127.0.0.1:1", "cu": "", "cm": "", "bd": ""}
	out := <-startPing(context.Background(), cfg)
	if out["ping_cu"] != false || out["loss_cu"] != false {
		t.Fatalf("unconfigured slot must be false: %+v", out)
	}
	if _, ok := out["loss_ct"].(float64); !ok {
		t.Fatalf("loss_ct must be a number: %+v", out)
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
	if gotHeader.Get("X-Agent-Config-Schema") != "6" || gotHeader.Get("X-Agent-Config-Md5") != "none" {
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
