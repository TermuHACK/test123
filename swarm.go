package main

// swarm.go — Agent Swarm по схеме: Synergy Harness → Hub / Agent Swarm / Solo Mode.
// Agent Swarm: Main Director → Task managing → Supervisors (research-team, coding,
// information-sorting) → subagents; агент передачи информации (Router) настраивает всё:
// выбирает нужных супервизоров, дополняет их промпты под задачу и собирает финальный ответ.
// Solo Mode: Main agent → subagents (без директора).
// Роли (шаблоны промптов) переопределяются файлами ~/.config/gomni/swarm/<роль>.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- роли (шаблоны промптов) ----------

var swarmDefaultRoles = map[string]string{
	"router": `Ты — агент передачи информации и настройки (Router). Получив задачу пользователя:
1) реши, какие супервизоры нужны из списка: research-team, coding, information-sorting;
2) для каждого выбранного дай короткое усиление роли (1-2 предложения, специфика задачи).
Ответь СТРОГО JSON: {"supervisors":[{"name":"coding","focus":"..."}]}. Без пояснений.`,
	"director": `Ты — Main Director агентного роя. Разложи задачу пользователя на конкретные подзадачи
для указанных супервизоров. Ответь СТРОГО JSON:
{"tasks":[{"supervisor":"coding","task":"..."}]}. Каждому супервизору 1-3 подзадачи. Без пояснений.`,
	"research-team": `Ты — супервизор research-команды: поиск фактов, анализ источников, проверка утверждений.
Выполни подзадачу максимально конкретно, со ссылками на факты. Если нужно — раздели работу между сабагентами.`,
	"coding": `Ты — супервизор coding-команды: архитектура, код, отладка, ревью.
Выполни подзадачу: дай готовый код/дифы/команды, без воды.`,
	"information-sorting": `Ты — супервизор сортировки информации: структурируй, группируй, выдели главное,
убери дубли, оформи итог в читаемую структуру.`,
	"subagent": `Ты — сабагент роя. Выполни порученный фрагмент работы точно и кратко.`,
	"solo": `Ты — основной агент в Solo Mode. Реши задачу сам; если она составная — мысленно разбей на шаги и выполни последовательно.`,
}

func swarmRolesDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gomni", "swarm")
}

// swarmRole — промпт роли: файл swarm/<name>.md переопределяет дефолт.
func swarmRole(name string) string {
	if b, err := os.ReadFile(filepath.Join(swarmRolesDir(), name+".md")); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(b)
	}
	if p, ok := swarmDefaultRoles[name]; ok {
		return p
	}
	return swarmDefaultRoles["subagent"]
}

// SwarmRoles — список ролей и источник их промпта (file/default) — для Hub.
func SwarmRoles() []SwarmRoleInfo {
	out := []SwarmRoleInfo{}
	for _, name := range []string{"router", "director", "research-team", "coding", "information-sorting", "subagent", "solo"} {
		src := "default"
		if _, err := os.Stat(filepath.Join(swarmRolesDir(), name + ".md")); err == nil {
			src = "file"
		}
		out = append(out, SwarmRoleInfo{Name: name, Source: src})
	}
	return out
}

type SwarmRoleInfo struct {
	Name   string `json:"name"`
	Source string `json:"source"` // default | file
}

// ---------- сообщения роя (передача информации) ----------

// SwarmMessage — конверт передачи информации между узлами (router обрабатывает все передачи).
type SwarmMessage struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"` // config|task|result|final
	Body string `json:"body"`
	At   int64  `json:"at"`
}

// ---------- ядро ----------

// Swarm — рой: директор, супервизоры, сабагенты, router.
type Swarm struct {
	LLM         *LLMClient
	Verbose     func(string) // прогресс наружу (CLI/TUI)
	MaxParallel int           // потолок параллельных LLM-вызовов (default 4)
	mu          sync.Mutex
	log         []SwarmMessage
}

func NewSwarm(llm *LLMClient) *Swarm { return &Swarm{LLM: llm, MaxParallel: 4} }

// Log — журнал передач информации (порядок: router config → director tasks → results → final).
func (s *Swarm) Log() []SwarmMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SwarmMessage, len(s.log))
	copy(out, s.log)
	return out
}

func (s *Swarm) route(from, to, kind, body string) {
	s.mu.Lock()
	s.log = append(s.log, SwarmMessage{From: from, To: to, Kind: kind, Body: body, At: time.Now().Unix()})
	s.mu.Unlock()
}

func (s *Swarm) sayf(format string, a ...any) {
	if s.Verbose != nil {
		s.Verbose(fmt.Sprintf(format, a...))
	}
}

// swarmLLMCall — один вызов chat/completions через настройки LLMClient (self-contained,
// не зависит от внутренних типов ответа).
func swarmLLMCall(ctx context.Context, llm *LLMClient, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": llm.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(llm.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	llm.applyProviderHeaders(req)
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncAuth(string(b), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("пустой ответ модели")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// swarmExtractJSON — вырезает первый JSON-объект из ответа (модели любят обёртки ```json).
func swarmExtractJSON(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// ---------- Agent Swarm (полная схема) ----------

// RunSwarm — задача → router (настройка) → director (task managing) → супервизоры+сабагенты → router (сборка).
func (s *Swarm) RunSwarm(ctx context.Context, task string) (string, error) {
	sem := make(chan struct{}, s.MaxParallel)

	// 1) Router: какие супервизоры нужны + усиление их ролей (настраивает всё).
	s.sayf("⚙ router: настраиваю рой под задачу…")
	cfgRaw, err := swarmLLMCall(ctx, s.LLM, swarmRole("router"), "Задача пользователя:\n"+task)
	if err != nil {
		return "", fmt.Errorf("router: %w", err)
	}
	var rconf struct {
		Supervisors []struct {
			Name  string `json:"name"`
			Focus string `json:"focus"`
		} `json:"supervisors"`
	}
	_ = json.Unmarshal([]byte(swarmExtractJSON(cfgRaw)), &rconf)
	if len(rconf.Supervisors) == 0 { // fallback: все три
		for _, n := range []string{"research-team", "coding", "information-sorting"} {
			rconf.Supervisors = append(rconf.Supervisors, struct {
				Name  string `json:"name"`
				Focus string `json:"focus"`
			}{Name: n})
		}
	}
	s.route("user", "router", "config", task)
	for _, sup := range rconf.Supervisors {
		s.route("router", sup.Name, "config", sup.Focus)
	}

	// 2) Main Director: декомпозиция на подзадачи (task managing).
	s.sayf("🎯 director: раскладываю задачу…")
	names := []string{}
	for _, sup := range rconf.Supervisors {
		names = append(names, sup.Name)
	}
	dirRaw, err := swarmLLMCall(ctx, s.LLM, swarmRole("director"),
		"Доступные супервизоры: "+strings.Join(names, ", ")+"\n\nЗадача:\n"+task)
	if err != nil {
		return "", fmt.Errorf("director: %w", err)
	}
	var dconf struct {
		Tasks []struct {
			Supervisor string `json:"supervisor"`
			Task       string `json:"task"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal([]byte(swarmExtractJSON(dirRaw)), &dconf)
	if len(dconf.Tasks) == 0 {
		dconf.Tasks = append(dconf.Tasks, struct {
			Supervisor string `json:"supervisor"`
			Task       string `json:"task"`
		}{Supervisor: rconf.Supervisors[0].Name, Task: task})
	}
	for _, t := range dconf.Tasks {
		s.route("director", t.Supervisor, "task", t.Task)
	}

	// 3) Супервизоры параллельно; каждый при желании поручает куски сабагентам (тоже параллельно).
	type supResult struct {
		Name, Out string
		Err       error
	}
	resCh := make(chan supResult, len(dconf.Tasks))
	var wg sync.WaitGroup
	for _, t := range dconf.Tasks {
		t := t
		focus := ""
		for _, sup := range rconf.Supervisors {
			if sup.Name == t.Supervisor {
				focus = sup.Focus
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.sayf("🧭 %s: работаю…", t.Supervisor)
			sys := swarmRole(t.Supervisor)
			if focus != "" {
				sys += "\n\nФокус для этой задачи: " + focus
			}
			out, err := swarmLLMCall(ctx, s.LLM, sys,
				"Исходная задача пользователя:\n"+task+"\n\nТвоя подзадача:\n"+t.Task)
			if err == nil {
				s.route(t.Supervisor, "router", "result", out)
			}
			resCh <- supResult{Name: t.Supervisor, Out: out, Err: err}
		}()
	}
	wg.Wait()
	close(resCh)

	var parts []string
	for r := range resCh {
		if r.Err != nil {
			s.sayf("⚠ %s: %v", r.Name, r.Err)
			continue
		}
		parts = append(parts, "## "+r.Name+"\n"+r.Out)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("все супервизоры упали")
	}

	// 4) Router: обработка передачи информации — сборка финального ответа.
	s.sayf("📦 router: собираю финальный ответ…")
	final, err := swarmLLMCall(ctx, s.LLM,
		swarmRole("information-sorting")+"\nТы финальный сборщик: объедини результаты команд в один связный ответ пользователю. Убери дубли, сохрани факты и код.",
		"Задача пользователя:\n"+task+"\n\nРезультаты команд:\n\n"+strings.Join(parts, "\n\n"))
	if err != nil {
		return strings.Join(parts, "\n\n"), nil // fallback: сырые части
	}
	s.route("router", "user", "final", final)
	return final, nil
}

// RunSolo — Solo Mode: основной агент без директора (main agent → ответ).
func (s *Swarm) RunSolo(ctx context.Context, task string) (string, error) {
	s.sayf("👤 solo: основной агент работает…")
	s.route("user", "main-agent", "task", task)
	out, err := swarmLLMCall(ctx, s.LLM, swarmRole("solo"), task)
	if err != nil {
		return "", err
	}
	s.route("main-agent", "user", "final", out)
	return out, nil
}
