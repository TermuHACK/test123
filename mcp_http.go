package main

// mcp_http.go — MCP по HTTP/SSE-транспорту (rewrite по образцу anomalyco/opencode,
// где есть StreamableHTTPClientTransport/SSEClientTransport наряду со stdio).
//
// Позволяет подключать удалённые MCP-серверы без локального процесса:
//   {"servers": {"exa": {"url": "https://mcp.exa.ai/mcp"}}}
// Протокол: JSON-RPC 2.0 POST; ответ — application/json или text/event-stream.
// Заголовки из конфига поддерживаются (напр. Authorization для приватных серверов).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type mcpHTTPServerCfg struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// mcpHTTPClient — клиент удалённого MCP-сервера.
type mcpHTTPClient struct {
	name    string
	url     string
	headers map[string]string
	mu      sync.Mutex
	nextID  int
	tools   []mcpToolInfo
	session string // Mcp-Session-Id если сервер его выдаёт
}

// startMCPHTTP — handshake + tools/list с удалённым сервером.
func startMCPHTTP(name string, sc mcpHTTPServerCfg) (*mcpHTTPClient, error) {
	cl := &mcpHTTPClient{name: name, url: sc.URL, headers: sc.Headers}
	initRes, err := cl.call(context.Background(), "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "synergy", "version": "3.0"},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	_ = initRes
	// notifications/initialized — fire-and-forget POST (сервер может ответить 202)
	cl.notifyHTTP("notifications/initialized")

	res, err := cl.call(context.Background(), "tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var tl struct {
		Tools []mcpToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &tl); err != nil {
		return nil, err
	}
	cl.tools = tl.Tools
	return cl, nil
}

// call — синхронный JSON-RPC вызов по HTTP (45 сек таймаут).
func (cl *mcpHTTPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	cl.mu.Lock()
	cl.nextID++
	id := cl.nextID
	cl.mu.Unlock()

	reqBody := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		reqBody["params"] = params
	}
	data, _ := json.Marshal(reqBody)

	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", cl.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range cl.headers {
		req.Header.Set(k, v)
	}
	if cl.session != "" {
		req.Header.Set("Mcp-Session-Id", cl.session)
	}

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		cl.session = sid
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("MCP %s HTTP %d: %s", cl.name, resp.StatusCode, trunc(string(body), 200))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return extractRPCResult(string(body), id)
}

// notifyHTTP — уведомление без id (ответ не ждём).
func (cl *mcpHTTPClient) notifyHTTP(method string) {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	req, err := http.NewRequest("POST", cl.url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cl.headers {
		req.Header.Set(k, v)
	}
	if cl.session != "" {
		req.Header.Set("Mcp-Session-Id", cl.session)
	}
	resp, err := sharedHTTPClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// extractRPCResult парсит ответ: чистый JSON или SSE-поток (data: {...} строки).
func extractRPCResult(body string, wantID int) (json.RawMessage, error) {
	parse := func(line string) (json.RawMessage, bool, error) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			return nil, false, nil
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return nil, false, nil
		}
		if resp.ID != wantID {
			return nil, false, nil
		}
		if resp.Error != nil {
			return nil, true, fmt.Errorf("MCP %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, true, nil
	}
	if r, ok, err := parse(body); ok || err != nil {
		return r, err
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			if r, ok, err := parse(strings.TrimPrefix(line, "data:")); ok || err != nil {
				return r, err
			}
		}
	}
	return nil, fmt.Errorf("MCP: ответ без результата для id=%d", wantID)
}

// registerHTTPTools вешает тулы удалённого сервера на агента (mcp_<server>_<tool>).
func (cl *mcpHTTPClient) registerHTTPTools(a *Agent) {
	for _, t := range cl.tools {
		toolName := "mcp_" + sanitizeToolName(cl.name) + "_" + sanitizeToolName(t.Name)
		spec := ToolSpec{Type: "function"}
		spec.Function.Name = toolName
		spec.Function.Description = fmt.Sprintf("[MCP:%s] %s", cl.name, t.Description)
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(t.InputSchema) > 0 {
			if m := map[string]any{}; json.Unmarshal(t.InputSchema, &m) == nil && len(m) > 0 {
				params = m
			}
		}
		spec.Function.Parameters = params
		mcpTool := t.Name
		a.RegisterTool(spec, func(ctx context.Context, args map[string]any) (string, error) {
			res, err := cl.call(ctx, "tools/call", map[string]any{"name": mcpTool, "arguments": args})
			if err != nil {
				return "", err
			}
			var out struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(res, &out); err != nil {
				return trunc(string(res), 8000), nil
			}
			var sb strings.Builder
			for _, c := range out.Content {
				if c.Type == "text" {
					sb.WriteString(c.Text)
					sb.WriteString("\n")
				}
			}
			txt := strings.TrimSpace(sb.String())
			if out.IsError {
				return "", fmt.Errorf("%s", trunc(txt, 2000))
			}
			if txt == "" {
				txt = "(пустой результат)"
			}
			return trunc(txt, 8000), nil
		})
	}
}

// ---------- конфиг: stdio + http серверы в одном mcp.json ----------

// mcpServersFile — общий формат mcp.json: у сервера либо command (stdio), либо url (HTTP).
type mcpServersFile struct {
	Servers map[string]struct {
		Command string            `json:"command,omitempty"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		URL     string            `json:"url,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	} `json:"servers"`
}

// mcpHTTPClients — живые HTTP-клиенты MCP (для attachMCP в чатах TUI).
var globalMCPHTTP []*mcpHTTPClient

// LoadMCPAll читает mcp.json, запускает stdio-серверы и подключает HTTP-серверы.
// Возвращает число подключённых серверов; ошибки отдельных серверов не фатальны.
func LoadMCPAll() (stdioClients []*mcpClient, httpClients []*mcpHTTPClient) {
	data, err := os.ReadFile(mcpConfigPath())
	if err != nil {
		return nil, nil
	}
	var cfg mcpServersFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, col(cYellow, "⚠ mcp.json: "+err.Error()))
		return nil, nil
	}
	for name, sc := range cfg.Servers {
		switch {
		case sc.URL != "":
			cl, err := startMCPHTTP(name, mcpHTTPServerCfg{URL: sc.URL, Headers: sc.Headers})
			if err != nil {
				fmt.Fprintln(os.Stderr, col(cYellow, fmt.Sprintf("⚠ MCP %s (http): %v", name, err)))
				continue
			}
			httpClients = append(httpClients, cl)
		case sc.Command != "":
			cl, err := startMCPServer(name, mcpServerCfg{Command: sc.Command, Args: sc.Args, Env: sc.Env})
			if err != nil {
				fmt.Fprintln(os.Stderr, col(cYellow, fmt.Sprintf("⚠ MCP %s (stdio): %v", name, err)))
				continue
			}
			stdioClients = append(stdioClients, cl)
		}
	}
	return stdioClients, httpClients
}

// attachMCPHTTP вешает тулы HTTP MCP-серверов на агента.
func attachMCPHTTP(a *Agent) int {
	n := 0
	for _, c := range globalMCPHTTP {
		before := len(a.Tools)
		c.registerHTTPTools(a)
		n += len(a.Tools) - before
	}
	return n
}
