package main

// MCP-клиент: подключение внешних tool-серверов по протоколу MCP (JSON-RPC 2.0 over stdio).
// Серверы описываются в ~/.config/synergy/mcp.json:
//   {"servers": {"filesystem": {"command": "npx", "args": ["-y","@modelcontextprotocol/server-filesystem","/sdcard"], "env": {}}}}
// Тулы MCP регистрируются у агента как mcp_<server>_<tool>.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type mcpServerCfg struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type mcpConfig struct {
	Servers map[string]mcpServerCfg `json:"servers"`
}

type mcpClient struct {
	name   string
	proc   *rawProc
	stdin  io.WriteCloser
	reader *bufio.Reader
	mu     sync.Mutex
	nextID int
	tools  []mcpToolInfo
}

type mcpToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpRPCReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpConfigPath — где лежит конфиг MCP-серверов.
func mcpConfigPath() string { return getConfigDir() + "/mcp.json" }

// LoadMCPServers читает mcp.json, запускает серверы и возвращает живых клиентов.
// Ошибки отдельных серверов не фатальны — пишем в stderr и идём дальше.
func LoadMCPServers() []*mcpClient {
	data, err := os.ReadFile(mcpConfigPath())
	if err != nil {
		return nil // конфига нет — MCP просто выключен
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, col(cYellow, "⚠ mcp.json: "+err.Error()))
		return nil
	}
	var out []*mcpClient
	for name, sc := range cfg.Servers {
		cl, err := startMCPServer(name, sc)
		if err != nil {
			fmt.Fprintln(os.Stderr, col(cYellow, fmt.Sprintf("⚠ MCP %s: %v", name, err)))
			continue
		}
		out = append(out, cl)
	}
	return out
}

func startMCPServer(name string, sc mcpServerCfg) (*mcpClient, error) {
	if sc.Command == "" {
		return nil, fmt.Errorf("пустая команда")
	}
	bin, err := LookPathSafe(sc.Command)
	if err != nil {
		return nil, err
	}
	// rawStart вместо os/exec: обход pidfd_open (SIGSYS на 32-бит Android, Go 1.24+).
	var env []string
	if len(sc.Env) > 0 {
		env = os.Environ()
		for k, v := range sc.Env {
			env = append(env, k+"="+v)
		}
	}
	rp, err := rawStart(bin, sc.Args, rawProcSpec{Env: env, MergeStderr: false})
	if err != nil {
		return nil, err
	}
	if rp.Stderr != nil {
		go io.Copy(io.Discard, rp.Stderr) // логи сервера не мешают TUI
	}
	cl := &mcpClient{name: name, proc: rp, stdin: rp.Stdin, reader: bufio.NewReader(rp.Stdout)}

	// initialize handshake
	initRes, err := cl.call(map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "synergy-harness", "version": "3.0"},
	}, "initialize")
	if err != nil {
		rp.Kill()
		rp.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	_ = initRes
	// notifications/initialized (без id)
	cl.notify("notifications/initialized")

	// tools/list
	res, err := cl.call(nil, "tools/list")
	if err != nil {
		rp.Kill()
		rp.Close()
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var tl struct {
		Tools []mcpToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &tl); err != nil {
		rp.Kill()
		rp.Close()
		return nil, err
	}
	cl.tools = tl.Tools
	return cl, nil
}

// call — синхронный JSON-RPC вызов (30 сек таймаут через горутину).
func (cl *mcpClient) call(params any, method string) (json.RawMessage, error) {
	cl.mu.Lock()
	cl.nextID++
	id := cl.nextID
	req := mcpRPCReq{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, _ := json.Marshal(req)
	if _, err := cl.stdin.Write(append(data, '\n')); err != nil {
		cl.mu.Unlock()
		return nil, err
	}
	cl.mu.Unlock()

	type res struct {
		msg json.RawMessage
		err error
	}
	ch := make(chan res, 1)
	go func() {
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			line, err := cl.reader.ReadBytes('\n')
			if err != nil {
				ch <- res{nil, err}
				return
			}
			var resp mcpRPCResp
			if err := json.Unmarshal(line, &resp); err != nil {
				continue // не-JSON строки (логи) пропускаем
			}
			if resp.ID != id {
				continue // нотификации/чужие ответы
			}
			if resp.Error != nil {
				ch <- res{nil, fmt.Errorf("MCP %d: %s", resp.Error.Code, resp.Error.Message)}
				return
			}
			ch <- res{resp.Result, nil}
			return
		}
		ch <- res{nil, fmt.Errorf("таймаут MCP %s", cl.name)}
	}()
	r := <-ch
	return r.msg, r.err
}

func (cl *mcpClient) notify(method string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	cl.stdin.Write(append(data, '\n'))
}

func (cl *mcpClient) close() {
	cl.notify("notifications/cancelled")
	if cl.proc != nil {
		cl.proc.Kill()
		cl.proc.Close()
	}
}

// registerMCPTools вешает тулы MCP-сервера на агента (имя mcp_<server>_<tool>).
func (cl *mcpClient) registerMCPTools(a *Agent) {
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
			res, err := cl.call(map[string]any{"name": mcpTool, "arguments": args}, "tools/call")
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

func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// defaultMCPExample — пример конфига, пишется один раз рядом с mcp.json.
func writeMCPExample() {
	p := getConfigDir() + "/mcp.example.json"
	if _, err := os.Stat(p); err == nil {
		return
	}
	os.WriteFile(p, []byte(`{
  "servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/sdcard"]
    },
    "memory": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-memory"]
    }
  }
}
`), 0o644)
}
