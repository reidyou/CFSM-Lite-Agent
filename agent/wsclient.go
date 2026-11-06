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
	"net/http"
	"sync"
	"time"
)

// 最小的 WebSocket 客户端（RFC 6455），只用标准库：握手、文本帧收发、ping/pong、close。
// Go 标准库没有 WebSocket，而 Agent 又不想引入第三方依赖，所以自己写这一小部分。

const (
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxFrame = 1 << 20 // 单个帧或单条消息的上限（1MB），避免对端异常时撑爆内存
)

type wsConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
	wmu sync.Mutex // 写帧需要互斥：主循环发数据，读取协程回 pong
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
		// 协议升级只能走 HTTP/1.1，所以关闭 HTTP/2
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
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
	return &wsConn{rwc: rwc, br: bufio.NewReader(rwc)}, nil
}

// writeFrame 发送一个带掩码的完整帧（客户端发出的帧必须带掩码）。
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
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
func (c *wsConn) WriteText(s string) error { return c.writeFrame(0x1, []byte(s)) }

// Close 先尽力发一个正常关闭帧（代码 1000），再关闭底层连接；对正在阻塞的读取也会立即生效。
func (c *wsConn) Close() {
	_ = c.writeFrame(0x8, []byte{0x03, 0xE8})
	_ = c.rwc.Close()
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
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
			_ = c.writeFrame(0x8, p)
			return "", &closeError{Code: code}
		case 0x9:
			if werr := c.writeFrame(0xA, p); werr != nil {
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
