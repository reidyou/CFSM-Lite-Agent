package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeRWC struct {
	io.Reader
	io.Writer
}

func (f *fakeRWC) Close() error { return nil }

// srvFrame 生成一个服务端帧（服务端发出的帧不带掩码）。
func srvFrame(opcode byte, fin bool, payload []byte) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	n := len(payload)
	switch {
	case n < 126:
		out = append(out, byte(n))
	case n <= 0xFFFF:
		out = append(out, 126, byte(n>>8), byte(n))
	default:
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		out = append(out, 127)
		out = append(out, l[:]...)
	}
	return append(out, payload...)
}

// readClientFrame 解析一个客户端帧（必须带掩码），返回操作码和还原后的内容。
func readClientFrame(r io.Reader) (opcode byte, payload []byte, masked bool, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	opcode = h[0] & 0x0F
	masked = h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(r, mk[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	return
}

func TestParseConfigConnection(t *testing.T) {
	cur := defaultConfig()
	if cur.ConnMode != "auto" || cur.WSSInterval != 2*time.Second {
		t.Fatalf("defaults: %+v", cur)
	}
	next, err := parseConfig("report_interval=60&connection_mode=http&wss_report_interval=4", cur)
	if err != nil || next.ConnMode != "http" || next.WSSInterval != 4*time.Second {
		t.Fatalf("parsed: %+v %v", next, err)
	}
	for _, bad := range []string{"connection_mode=ws", "wss_report_interval=0", "wss_report_interval=6", "wss_report_interval=x"} {
		if _, err := parseConfig("report_interval=60&"+bad, cur); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestWSReadText(t *testing.T) {
	var in bytes.Buffer
	in.Write(srvFrame(0x1, true, []byte("abc")))
	in.Write(srvFrame(0x9, true, []byte("p1")))
	in.Write(srvFrame(0x1, false, []byte("he")))
	in.Write(srvFrame(0x0, true, []byte("llo")))
	in.Write(srvFrame(0x8, true, []byte{0x11, 0x34})) // 关闭码 4404
	var out bytes.Buffer
	rwc := &fakeRWC{Reader: &in, Writer: &out}
	c := &wsConn{rwc: rwc, br: bufio.NewReader(rwc)}

	if m, err := c.ReadText(); err != nil || m != "abc" {
		t.Fatalf("first: %q %v", m, err)
	}
	if m, err := c.ReadText(); err != nil || m != "hello" { // 中间的 ping 被自动回应，分片被合并
		t.Fatalf("second: %q %v", m, err)
	}
	_, err := c.ReadText()
	var ce *closeError
	if !errors.As(err, &ce) || ce.Code != 4404 {
		t.Fatalf("expected close 4404, got %v", err)
	}
	op, p, masked, err := readClientFrame(&out)
	if err != nil || op != 0xA || string(p) != "p1" || !masked {
		t.Fatalf("pong: op=%d p=%q masked=%v err=%v", op, p, masked, err)
	}
	if op, _, _, err = readClientFrame(&out); err != nil || op != 0x8 {
		t.Fatalf("close echo: op=%d err=%v", op, err)
	}
}

func TestWSWriteText(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 300, 65535, 65536, 70000} {
		var buf bytes.Buffer
		c := &wsConn{rwc: &fakeRWC{Reader: strings.NewReader(""), Writer: &buf}}
		payload := strings.Repeat("x", n)
		if err := c.WriteText(payload); err != nil {
			t.Fatal(err)
		}
		op, got, masked, err := readClientFrame(&buf)
		if err != nil || op != 0x1 || !masked || string(got) != payload {
			t.Fatalf("n=%d: op=%d masked=%v err=%v len=%d", n, op, masked, err, len(got))
		}
	}
}

// wsTestServer 启动一个只会做 WebSocket 握手的测试服务器，握手成功后把连接交给 script。
func wsTestServer(script func(conn net.Conn, br *bufio.Reader)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.Header.Get("X-Agent-Id") != "abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			wsAccept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
		_, _ = rw.WriteString(resp)
		_ = rw.Flush()
		script(conn, rw.Reader)
	}))
}

func TestDialWS(t *testing.T) {
	srv := wsTestServer(func(conn net.Conn, br *bufio.Reader) {
		_, _ = conn.Write(srvFrame(0x1, true, []byte(`{"type":"ready","active":false,"interval":2}`)))
		op, p, _, err := readClientFrame(br)
		if err == nil && op == 0x1 && string(p) == "ping" {
			_, _ = conn.Write(srvFrame(0x1, true, []byte("pong")))
		}
	})
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer s3cret")
	hdr.Set("X-Agent-Id", "abc")
	c, err := dialWS(context.Background(), srv.URL, hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if m, err := c.ReadText(); err != nil || !strings.Contains(m, `"ready"`) {
		t.Fatalf("ready: %q %v", m, err)
	}
	if err := c.WriteText("ping"); err != nil {
		t.Fatal(err)
	}
	if m, err := c.ReadText(); err != nil || m != "pong" {
		t.Fatalf("pong: %q %v", m, err)
	}

	_, err = dialWS(context.Background(), srv.URL, http.Header{})
	var he *handshakeError
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("expected handshake 401, got %v", err)
	}
}

func TestBuildMetrics(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a := snapshot{at: t0, cpuTotal: 1000, cpuIdle: 800, btime: 1700000000, rx: 1000, tx: 2000}
	b := snapshot{at: t0.Add(2 * time.Second), cpuTotal: 1100, cpuIdle: 850, btime: 1700000000, rx: 5096, tx: 4048}
	st := &state{}
	heavy := map[string]any{"disk_total": int64(10), "os": "x"}
	ping := map[string]any{"ping_ct": 1.5}
	m := buildMetrics(a, b, defaultConfig(), st, heavy, ping)
	if m["cpu"] != 50.0 {
		t.Errorf("cpu = %v", m["cpu"])
	}
	if m["net_in_speed"] != 2048.0 || m["net_out_speed"] != 1024.0 {
		t.Errorf("speeds = %v %v", m["net_in_speed"], m["net_out_speed"])
	}
	if m["disk_total"] != int64(10) || m["ping_ct"] != 1.5 || m["boot_time"] != int64(1700000000000) {
		t.Errorf("merged fields: %v %v %v", m["disk_total"], m["ping_ct"], m["boot_time"])
	}
	if _, ok := m["ram_total"]; !ok {
		t.Error("missing ram_total")
	}
}

// TestRealtimeSession 把握手、激活、实时帧、服务端关闭整个流程跑一遍。
func TestRealtimeSession(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := wsTestServer(func(conn net.Conn, br *bufio.Reader) {
		_, _ = conn.Write(srvFrame(0x1, true, []byte(`{"type":"ready","active":true,"interval":1}`)))
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
			op, p, _, err := readClientFrame(br)
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var m map[string]any
			if json.Unmarshal(p, &m) == nil && m["type"] == "metrics" {
				got <- m
				_, _ = conn.Write(srvFrame(0x1, true, []byte(`{"type":"deactivate"}`)))
				_, _ = conn.Write(srvFrame(0x8, true, []byte{0x03, 0xE8}))
				return
			}
		}
	})
	defer srv.Close()

	box := newCfgBox()
	sh := &sharedData{}
	sh.setPing(map[string]any{"ping_ct": 12.5, "loss_ct": 0.0})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- realtimeSession(ctx, settings{URL: srv.URL, ID: "abc", Secret: "s3cret"}, box, &state{}, sh)
	}()

	select {
	case m := <-got:
		metrics, _ := m["metrics"].(map[string]any)
		for _, k := range []string{"cpu", "ram_total", "net_in_speed", "net_rx_monthly", "disk_total", "os", "ping_ct"} {
			if _, ok := metrics[k]; !ok {
				t.Errorf("实时帧缺少字段 %s", k)
			}
		}
		if metrics["ping_ct"] != 12.5 {
			t.Errorf("ping_ct = %v", metrics["ping_ct"])
		}
	case <-time.After(9 * time.Second):
		t.Fatal("没有收到实时帧")
	}
	select {
	case err := <-errc:
		var ce *closeError
		if !errors.As(err, &ce) {
			t.Errorf("会话应该因为服务端关闭而结束，得到: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("会话没有结束")
	}
}
