package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// 最小的 WebSocket 客户端（RFC 6455），只用标准库：握手、文本帧收发、ping/pong、close。
// Go 标准库没有 WebSocket，而 Agent 又不想引入第三方依赖，所以自己写这一小部分。

const (
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxFrame = 1 << 20 // 单个帧或单条消息的上限（1MB），避免对端异常时撑爆内存

	wsDialTimeout = 10 * time.Second // 建立 TCP 连接的超时
)

// 下面两个用变量，只是为了让测试可以缩短等待时间；正常运行时不会修改。
var (
	wsWriteTimeout = 8 * time.Second // 写一个帧的超时：超时就关闭底层连接，不会一直卡在写上
	wsCloseTimeout = 2 * time.Second // 关闭连接时发送关闭帧的最长等待
)

type wsConn struct {
	rwc      io.ReadWriteCloser
	br       *bufio.Reader
	wmu      sync.Mutex   // 写帧需要互斥：主循环发数据，读取协程回 pong
	lastRecv atomic.Int64 // 最近一次收到任何帧（包括 ping、pong）的时间，UnixNano；0 表示还没有
	closeOne sync.Once
}

// touch 记录刚刚收到了数据。
func (c *wsConn) touch() { c.lastRecv.Store(time.Now().UnixNano()) }

// sinceRecv 返回距离上次收到数据过了多久；还没收到过任何数据时返回 0，由调用方自己用连接时间兜底。
func (c *wsConn) sinceRecv() time.Duration {
	v := c.lastRecv.Load()
	if v == 0 {
		return 0
	}
	return time.Since(time.Unix(0, v))
}

// handshakeError 带着服务端返回的 HTTP 状态码，调用方据此决定多久之后重试。
type handshakeError struct{ Status int }

func (e *handshakeError) Error() string {
	return fmt.Sprintf("WebSocket 握手失败：HTTP %d", e.Status)
}

// closeError 表示对端主动关闭了连接，Code 是关闭码。
type closeError struct{ Code int }

func (e *closeError) Error() string {
	return fmt.Sprintf("连接被服务端关闭（代码 %d）", e.Code)
}

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// dialWS 向 rawURL（http:// 或 https://）发起 WebSocket 升级请求。
// 用标准库 http.Client 完成 TCP、TLS、代理和握手：状态码 101 时，resp.Body 就是可读写的连接。
func dialWS(ctx context.Context, rawURL string, hdr http.Header) (*wsConn, error) {
	var kb [16]byte
	if _, err := rand.Read(kb[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(kb[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// 不设置 DialContext 的话建立 TCP 连接没有超时，网络不通时每次重连要等系统默认的两分钟
		DialContext: (&net.Dialer{Timeout: wsDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		// 协议升级只能走 HTTP/1.1，所以关闭 HTTP/2
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
		// 每次重连都新建 Transport：握手没成功时不要把连接留在空闲池里
		DisableKeepAlives: true,
	}
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return nil, &handshakeError{Status: resp.StatusCode}
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		_ = resp.Body.Close()
		return nil, errors.New("WebSocket 握手校验失败")
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errors.New("连接不支持协议升级")
	}
	c := &wsConn{rwc: rwc, br: bufio.NewReader(rwc)}
	c.touch() // 握手成功算作第一次收到数据
	return c, nil
}

// writeFrame 发送一个带掩码的完整帧（客户端发出的帧必须带掩码）。
// timeout 大于 0 时，超时还没写完就关闭底层连接让写入失败：连接拿到的是 http 升级后的读写器，设置不了写超时，
// 对端网络卡死时写入会一直阻塞，同时占着写锁，Close 也会跟着卡住。
func (c *wsConn) writeFrame(opcode byte, payload []byte, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if timeout > 0 {
		t := time.AfterFunc(timeout, func() { _ = c.rwc.Close() })
		defer t.Stop()
	}
	n := len(payload)
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|opcode)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		hdr = append(hdr, 0x80|127)
		hdr = append(hdr, l[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)
	buf := make([]byte, len(hdr)+n)
	copy(buf, hdr)
	for i := 0; i < n; i++ {
		buf[len(hdr)+i] = payload[i] ^ mask[i%4]
	}
	_, err := c.rwc.Write(buf)
	return err
}

// WriteText 发送一条文本消息。
func (c *wsConn) WriteText(s string) error { return c.writeFrame(0x1, []byte(s), wsWriteTimeout) }

// Close 先尽力发一个正常关闭帧（代码 1000），再关闭底层连接；对正在阻塞的读取也会立即生效。
// 关闭帧最多等 wsCloseTimeout：对端卡死时不能让 Close 一直等下去，否则重连逻辑会跟着卡住。
func (c *wsConn) Close() {
	c.closeOne.Do(func() {
		t := time.AfterFunc(wsCloseTimeout, func() { _ = c.rwc.Close() })
		_ = c.writeFrame(0x8, []byte{0x03, 0xE8}, 0)
		t.Stop()
		_ = c.rwc.Close()
	})
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	c.touch()
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > wsMaxFrame {
		err = errors.New("WebSocket 帧过大")
		return
	}
	var mk [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mk[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	return
}

// ReadText 返回下一条完整的文本消息：自动回应 ping，合并分片，忽略 pong。
// 对端关闭连接时返回 *closeError。
func (c *wsConn) ReadText() (string, error) {
	var msg []byte
	for {
		fin, op, p, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch op {
		case 0x8:
			code := 1005
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p[:2]))
			}
			_ = c.writeFrame(0x8, p, wsCloseTimeout)
			return "", &closeError{Code: code}
		case 0x9:
			if werr := c.writeFrame(0xA, p, wsWriteTimeout); werr != nil {
				return "", werr
			}
		case 0xA:
			// pong：不需要处理
		case 0x0, 0x1, 0x2:
			msg = append(msg, p...)
			if len(msg) > wsMaxFrame {
				return "", errors.New("WebSocket 消息过大")
			}
			if fin {
				return string(msg), nil
			}
		}
	}
}
