package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
)

// ProbeResult 连通性测试结果。
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Model     string `json:"model"`
	Reply     string `json:"reply,omitempty"`
	Usage     Usage  `json:"usage"`
	Error     string `json:"error,omitempty"`
}

// DefaultProbeModel 渠道未配置模型时使用的探测模型名。
func DefaultProbeModel(ch model.Channel) string {
	for _, m := range ch.Models {
		if m != "*" {
			return m
		}
	}
	for m := range ch.Alias {
		return m
	}
	return "gpt-4o"
}

// probeModel 挑选探测用的模型名：渠道已配置则取第一个；未配置则先问上游 /models，
// 拿不到再退回通用名，避免因模型名不存在而误判为"连不上"。
func probeModel(ctx context.Context, client *Client, ch model.Channel) string {
	if m := DefaultProbeModel(ch); m != "" {
		if len(ch.Models) > 0 || len(ch.Alias) > 0 {
			return m
		}
	}
	if ids, err := FetchModels(ctx, client, ch); err == nil && len(ids) > 0 {
		return ids[0]
	}
	return DefaultProbeModel(ch)
}

// Probe 对单个渠道发送一次最小对话请求，验证连通性。
func Probe(ctx context.Context, cfg *config.Config, client *Client, ch model.Channel, modelName, prompt string) ProbeResult {
	if prompt == "" {
		prompt = "ping"
	}
	if modelName == "" {
		modelName = probeModel(ctx, client, ch)
	}
	timeout := timeoutFor(cfg, ch, false)
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cand := model.Candidate{Channel: ch, ID: ch.ID, Name: ch.Name, UpstreamModel: modelName}
	payload := map[string]any{
		"model":      modelName,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens": 16,
		"stream":     false,
	}

	h := &Handler{cfg: cfg}
	req, err := h.BuildRequest(ctx, cand, payload, false)
	if err != nil {
		return ProbeResult{Model: modelName, Error: err.Error()}
	}

	start := time.Now()
	resp, err := client.Do(req)
	res := ProbeResult{Model: modelName}
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.LatencyMS = time.Since(start).Milliseconds()
	res.Status = resp.StatusCode
	res.Usage = ParseUsage(data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = truncate(string(data), 500)
		return res
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err == nil && len(out.Choices) > 0 {
		res.Reply = truncate(out.Choices[0].Message.Content, 300)
	}
	res.OK = true
	return res
}

// FetchModels 拉取上游 /models 列表。
func FetchModels(ctx context.Context, client *Client, ch model.Channel) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, JoinURL(ch.BaseURL, ModelsPath), nil)
	if err != nil {
		return nil, err
	}
	applyAuth(req, ch)
	for k, v := range ch.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, fmt.Errorf("上游未提供 %s 接口（%d），请在渠道里手动填写模型名", ModelsPath, resp.StatusCode)
		}
		return nil, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if strings.TrimSpace(d.ID) != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}
