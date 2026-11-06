package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type reporter struct {
	client *http.Client
	url    string
	id     string
	secret string
}

type reply struct {
	Status int
	Body   string
}

func newReporter(s settings) *reporter {
	return &reporter{
		client: &http.Client{
			Timeout: 15 * time.Second,
			// 不跟随重定向：避免上报内容和密钥被带到别的地址
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		url:    s.URL,
		id:     s.ID,
		secret: s.Secret,
	}
}

// send 向 Worker 上报一次。X-Agent-Config-Md5 是 Agent 当前使用的配置的 MD5（首次为 none），
// Worker 据此决定回 204（配置没变）还是回完整的新配置。
func (r *reporter) send(ctx context.Context, metrics map[string]any, cfgMD5 string) (reply, error) {
	payload, err := json.Marshal(map[string]any{"id": r.id, "secret": r.secret, "metrics": metrics})
	if err != nil {
		return reply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(payload))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Version", "Lite-agent-"+version)
	req.Header.Set("X-Agent-Config-Schema", "7")
	req.Header.Set("X-Agent-Config-Md5", cfgMD5)
	resp, err := r.client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return reply{}, fmt.Errorf("读取响应失败: %w", err)
	}
	return reply{Status: resp.StatusCode, Body: string(body)}, nil
}
