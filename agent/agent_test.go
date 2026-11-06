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
	s.advance(1000, 2000, t0, day1, "")
	if s.RX != 0 || s.TX != 0 {
		t.Fatalf("first sample only sets the baseline: %+v", s)
	}
	s.advance(1500, 2600, t0.Add(time.Minute), day1, "")
	if s.RX != 500 || s.TX != 600 {
		t.Fatalf("after delta: %+v", s)
	}
	s.advance(100, 50, t0.Add(2*time.Minute), day1, "") // 计数器被重置
	if s.RX != 600 || s.TX != 650 {
		t.Fatalf("after reset: %+v", s)
	}
	s.advance(200, 80, time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC), day1, "") // 新周期，累计清零后再加增量
	if s.Period != "2026-11-01" || s.RX != 100 || s.TX != 30 {
		t.Fatalf("new period: %+v", s)
	}
	s.advance(999999, 999999, time.Date(2026, 11, 1, 0, 6, 0, 0, time.UTC), day1, "eth0") // 网卡配置变化：重建基线，不产生巨大增量
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
	s.advance(1000, 1000, t0, rule, "")
	s.advance(1500, 1700, t0.Add(time.Minute), rule, "")
	cfg := defaultConfig()
	cfg.CalID, cfg.CalRX, cfg.CalTX, cfg.CalPeriod = "100", 5000, 9000, "2026-11-01"
	s.calibrate(cfg)
	if s.RX != 5000 || s.TX != 9000 || s.CalID != "100" {
		t.Fatalf("calibration not applied: %+v", s)
	}
	s.advance(1600, 1800, t0.Add(2*time.Minute), rule, "") // 校准之后继续累计
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
	s.advance(1_000_000_000, 500_000_000, t0, rule, "")
	s.advance(1_000_001_000, 500_000_500, t0.Add(6*time.Second), rule, "") // 实时帧：较新的读数
	s.advance(1_000_000_400, 500_000_200, t0.Add(time.Second), rule, "")   // HTTP 循环：几秒前采的读数
	if s.RX != 1000 || s.TX != 500 {
		t.Fatalf("stale snapshot must be ignored, got RX=%d TX=%d", s.RX, s.TX)
	}
	s.advance(1_000_001_600, 500_000_900, t0.Add(8*time.Second), rule, "") // 之后的正常读数继续累计
	if s.RX != 1600 || s.TX != 900 {
		t.Fatalf("after stale: RX=%d TX=%d", s.RX, s.TX)
	}
	s.advance(100, 50, t0.Add(9*time.Second), rule, "") // 真正的计数器重置（读数是新的）仍按重置处理
	if s.RX != 1700 || s.TX != 950 {
		t.Fatalf("real reset: RX=%d TX=%d", s.RX, s.TX)
	}
}
