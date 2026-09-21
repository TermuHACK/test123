package main

// cli.go — чистый CLI-интерфейс Synergy (bubbletea выпилен). Схема:
//   Synergy Harness → Hub (роли/провайдеры/плагины/MCP/cron) | Agent Swarm | Solo Mode
// Команды: /help /model /models /providers /hub /roles /swarm /solo /login /new /history /quit
// Ввод: readline.go (raw-режим, стрелки ←/→ курсор, ↑/↓ история); пайпы → bufio fallback.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var cliCfg Config

// cliSave — сохранить конфиг (провайдер/модель/ключи).
func cliSave() { saveConfig(cliCfg) }

// jsonMarshalIndent — helper, раньше жил в ui.go (нужен cron.go).
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func cliPrintHelp() {
	fmt.Println(`Synergy Harness — CLI
  /help                эта справка
  /models              список моделей провайдера (живой fetch)
  /model <id|номер>    выбрать модель (номер — из /models)
  /providers           список провайдеров; /provider <имя> — переключить
  /key <провайдер> <ключ>  сохранить API-ключ (имя с пробелами — как есть)
  /hub                 хаб: роли swarm, провайдеры, cron
  /roles               шаблоны и промпты (роли) — файлы в ~/.config/gomni/swarm/
  /swarm <задача>      Agent Swarm: router → director → супервизоры → сборка
  /solo <задача>       Solo Mode: один основной агент
  /login               OAuth device-логин (codex flow)
  /new                 новая сессия (сброс контекста)
  /history             сохранённые сессии
  /quit                выход
  всё остальное — запрос агенту (Solo с инструментами)
  стрелки: ←/→ курсор, ↑/↓ история команд`)
}

func cliCurrentProvider() ProviderDef {
	name := cliCfg.Provider
	if name == "" {
		name = "OpenCode Zen no-key"
	}
	if p := ProviderByName(name); p != nil {
		return *p
	}
	return ProviderDef{Name: name, Base: "https://opencode.ai/zen/v1", Free: true}
}

func cliApplyProvider(llm *LLMClient, p ProviderDef) {
	llm.BaseURL = strings.TrimRight(p.Base, "/")
	k := ""
	if p.KeyFrom != nil {
		k = p.KeyFrom(cliCfg)
	}
	llm.APIKey = k
	cliCfg.Provider = p.Name
	cliSave()
	InvalidateModelsCache()
	tag := ""
	if p.Free && k == "" {
		tag = " [free]"
	}
	fmt.Printf("✓ провайдер: %s (%s)%s\n", p.Name, llm.BaseURL, tag)
}

func cliHub() {
	fmt.Println("── Hub ──────────────────────────────")
	fmt.Println("Роли swarm (~/.config/gomni/swarm/<роль>.md переопределяет дефолт):")
	for _, r := range SwarmRoles() {
		fmt.Printf("  %-20s [%s]\n", r.Name, r.Source)
	}
	fmt.Println("Провайдеры:")
	for _, p := range AllProviders() {
		mark := "  "
		if strings.EqualFold(p.Name, cliCfg.Provider) {
			mark = "→ "
		}
		fmt.Printf("  %s%s  %s\n", mark, p.Name, p.Base)
	}
	if ts := ListTasks(); len(ts) > 0 {
		fmt.Println("Cron/триггеры:")
		for _, t := range ts {
			fmt.Printf("  %v\n", t)
		}
	}
	fmt.Println("──────────────────────────────────────")
}

func cliModels(llm *LLMClient) {
	p := cliCurrentProvider()
	fmt.Printf("… fetch /models (%s)\n", p.Name)
	infos, err := FetchModels(&p, cliCfg, false)
	if err != nil {
		fmt.Println("✗", err)
		return
	}
	for i, mi := range infos {
		cur := "  "
		if mi.ID == llm.Model {
			cur = "→ "
		}
		fmt.Printf("  %s%2d. %s\n", cur, i+1, mi.ID)
	}
	fmt.Println("выбор: /model <id> или /model <номер>")
}

func cliRunSwarm(ctx context.Context, llm *LLMClient, task string, solo bool) {
	sw := NewSwarm(llm)
	sw.Verbose = func(s string) { fmt.Fprintln(os.Stderr, col(cCyan, s)) }
	t0 := time.Now()
	var out string
	var err error
	if solo {
		out, err = sw.RunSolo(ctx, task)
	} else {
		out, err = sw.RunSwarm(ctx, task)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, col(cRed, "✗ "+err.Error()))
		return
	}
	fmt.Println(out)
	mode := "swarm"
	if solo {
		mode = "solo"
	}
	fmt.Fprintln(os.Stderr, col(cGray, fmt.Sprintf("── %s · %s · передач: %d ──", mode, time.Since(t0).Round(time.Second), len(sw.Log()))))
}

// runCLI — основной REPL.
func runCLI(cfg Config, llm *LLMClient, agent *Agent) {
	cliCfg = cfg
	cliPrintHelp()
	fmt.Printf("провайдер: %s · модель: %s\n\n", cliCurrentProvider().Name, llm.Model)

	prompt := col(cGreen, "synergy") + col(cGray, "› ")
	restore, rawOK := cliRawMode()
	if rawOK {
		defer restore()
	}
	var history []string
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for {
		var line string
		if rawOK {
			l, ok := cliReadLine(prompt, &history)
			if !ok {
				fmt.Println()
				return
			}
			line = strings.TrimSpace(l)
		} else {
			fmt.Print(prompt)
			if !sc.Scan() {
				fmt.Println()
				return
			}
			line = strings.TrimSpace(sc.Text())
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if cliCommand(line, llm, agent) {
				return
			}
			continue
		}
		// обычный запрос — основной агент с инструментами (Solo-ветка схемы)
		agent.OnEvent = func(ev AgentEvent) {
			switch ev.Kind {
			case "tool":
				fmt.Fprintf(os.Stderr, "%s %s\n", col(cGray, "  ⚙"), col(cGray, ev.Title))
			case "err":
				fmt.Fprintln(os.Stderr, col(cRed, "  ✗ "+ev.Title))
			}
		}
		out, err := agent.Run(context.Background(), line)
		if err != nil {
			fmt.Fprintln(os.Stderr, col(cRed, "✗ "+err.Error()))
			continue
		}
		if strings.TrimSpace(out) != "" {
			fmt.Println(out)
		}
	}
}

// cliCommand — обработка slash-команд; true = выйти.
func cliCommand(line string, llm *LLMClient, agent *Agent) bool {
	fields := strings.Fields(line)
	cmd := fields[0]
	arg := strings.TrimSpace(strings.TrimPrefix(line, cmd))
	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help", "/h":
		cliPrintHelp()
	case "/new":
		agent.Reset()
		fmt.Println("✓ новая сессия")
	case "/models":
		cliModels(llm)
	case "/model":
		if arg == "" {
			fmt.Println("модель:", llm.Model, "· выбор: /models")
			break
		}
		id := arg
		if n, err := strconv.Atoi(arg); err == nil {
			// номер из последнего /models → id модели
			p := cliCurrentProvider()
			infos, ferr := FetchModels(&p, cliCfg, false)
			if ferr != nil {
				fmt.Println("✗ список моделей:", ferr)
				break
			}
			if n < 1 || n > len(infos) {
				fmt.Printf("✗ номер вне диапазона 1..%d (см. /models)\n", len(infos))
				break
			}
			id = infos[n-1].ID
		}
		llm.Model = id
		cliCfg.Model = id
		cliSave()
		fmt.Println("✓ модель:", id)
	case "/providers":
		for _, p := range AllProviders() {
			mark := "  "
			if strings.EqualFold(p.Name, cliCfg.Provider) {
				mark = "→ "
			}
			fmt.Printf("  %s%s  %s\n", mark, p.Name, p.Base)
		}
		fmt.Println("переключить: /provider <имя>")
	case "/provider":
		if arg == "" {
			fmt.Println("текущий:", cliCurrentProvider().Name)
			break
		}
		p := ProviderByName(arg)
		if p == nil {
			fmt.Println("✗ нет такого провайдера, список: /providers")
			break
		}
		cliApplyProvider(llm, *p)
	case "/key":
		// имя провайдера может содержать пробелы: ключ — последнее слово, имя — всё до него
		f := strings.Fields(arg)
		if len(f) < 2 {
			fmt.Println("использование: /key <провайдер> <ключ>  (напр.: /key OpenCode Zen sk-...)")
			break
		}
		key := f[len(f)-1]
		provName := strings.Join(f[:len(f)-1], " ")
		if cliCfg.Providers == nil {
			cliCfg.Providers = map[string]*ProviderCfg{}
		}
		pc := cliCfg.Providers[provName]
		if pc == nil {
			pc = &ProviderCfg{}
			cliCfg.Providers[provName] = pc
		}
		pc.APIKey = key
		cliSave()
		cliApplyProvider(llm, cliCurrentProvider())
		fmt.Println("✓ ключ сохранён для", provName)
		if llm.APIKey != "" {
			masked := llm.APIKey
			if len(masked) > 10 {
				masked = masked[:6] + "…" + masked[len(masked)-4:]
			}
			fmt.Println("  активный ключ:", masked)
		} else {
			fmt.Println("  ⚠ ключ НЕ применился к провайдеру — проверь имя (/providers)")
		}
	case "/hub":
		cliHub()
	case "/roles":
		for _, r := range SwarmRoles() {
			fmt.Printf("  %-20s [%s]\n", r.Name, r.Source)
		}
		fmt.Println("переопределение: ~/.config/gomni/swarm/<роль>.md")
	case "/swarm":
		if arg == "" {
			fmt.Println("использование: /swarm <задача>")
			break
		}
		cliRunSwarm(context.Background(), llm, arg, false)
	case "/solo":
		if arg == "" {
			fmt.Println("использование: /solo <задача>")
			break
		}
		cliRunSwarm(context.Background(), llm, arg, true)
	case "/login":
		if err := CodexDeviceLogin(context.Background(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, col(cRed, "login: "+err.Error()))
		}
	case "/history":
		files, _ := filepath.Glob(filepath.Join(sessionsDir(), "*.json"))
		if len(files) == 0 {
			fmt.Println("пусто")
			break
		}
		for _, f := range files {
			fmt.Println("  " + strings.TrimSuffix(filepath.Base(f), ".json"))
		}
	default:
		fmt.Println("? неизвестная команда, /help")
	}
	return false
}
