package main

// server.go — Go-сервер Synergy Harness: HTTP API + встроенная веб-панель.
// Запуск: synergy-harness -serve [-port 8787]. Работает без TUI — подходит
// как headless-режим на сервере и как «хаб» с доступом из браузера.
//
// API (JSON):
//   GET  /api/status     — версия, провайдер, модель, аптайм
//   GET  /api/agents     — живые чаты/субагенты (имя, статус, таски)
//   POST /api/chat       — {text, session?} → запуск агента, ответ когда готов
//   GET  /api/plugins    — подключённые плагины и их тулы
//   GET  /api/mcp        — MCP-серверы и количество тулов
//   GET  /api/cron       — cron-записи; POST /api/cron {text, every_min, delay_min}
//   GET  /api/providers  — провайдеры; GET /api/models?provider= — живой фетч /models
//   GET  /               — веб-панель (один HTML, без внешних зависимостей)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// serverState — разделяемое состояние сервера.
type serverState struct {
	cfg     Config
	llm     *LLMClient
	workdir string
	plugins []*Plugin
	started time.Time

	mu       sync.Mutex
	sessions map[string]*srvSession
	nextID   int
}

type srvSession struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	agent   *Agent
}

func runServer(cfg Config, llm *LLMClient, workdir string, plugins []*Plugin, port int) int {
	st := &serverState{
		cfg: cfg, llm: llm, workdir: workdir, plugins: plugins,
		started: time.Now(), sessions: map[string]*srvSession{},
	}
	go StartCronLoop(func(e CronEntry) {
		notifySystem("SYNERGY", "⏰ cron: "+trunc(e.Text, 60))
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", st.handleIndex)
	mux.HandleFunc("/api/status", st.handleStatus)
	mux.HandleFunc("/api/agents", st.handleAgents)
	mux.HandleFunc("/api/chat", st.handleChat)
	mux.HandleFunc("/api/plugins", st.handlePlugins)
	mux.HandleFunc("/api/mcp", st.handleMCP)
	mux.HandleFunc("/api/cron", st.handleCron)
	mux.HandleFunc("/api/providers", st.handleProviders)
	mux.HandleFunc("/api/models", st.handleModels)

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("⌘ Synergy Harness server: http://localhost%s  (API: /api/status)\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println("server error:", err)
		return 1
	}
	return 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

func (st *serverState) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"name":     "Synergy Harness",
		"provider": st.cfg.Provider,
		"model":    st.llm.Model,
		"uptime_s": int(time.Since(st.started).Seconds()),
		"sessions": len(st.sessions),
		"plugins":  len(st.plugins),
		"mcp":      len(globalMCPServers),
	})
}

func (st *serverState) handleAgents(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	out := make([]srvSession, 0, len(st.sessions))
	for _, s := range st.sessions {
		out = append(out, srvSession{ID: s.ID, Name: s.Name, Running: s.Running})
	}
	st.mu.Unlock()
	writeJSON(w, map[string]any{"agents": out, "tasks": ListTasks()})
}

func (st *serverState) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text    string `json:"text"`
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, `{"error":"нужен JSON {"text":"…"}"}`, http.StatusBadRequest)
		return
	}
	st.mu.Lock()
	sid := req.Session
	if sid == "" {
		sid = "default"
	}
	s := st.sessions[sid]
	if s == nil {
		st.nextID++
		s = &srvSession{ID: sid, Name: fmt.Sprintf("chat%d", st.nextID)}
		s.agent = NewAgent(st.llm, st.workdir, st.plugins...)
		attachMCP(s.agent)
		st.sessions[sid] = s
	}
	if s.Running {
		st.mu.Unlock()
		http.Error(w, `{"error":"сессия занята — дождись ответа или используй другую session"}`, http.StatusConflict)
		return
	}
	s.Running = true
	st.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.agent.Run(ctx, req.Text)

	st.mu.Lock()
	s.Running = false
	st.mu.Unlock()

	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "session": sid})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "session": sid, "answer": out})
}

func (st *serverState) handlePlugins(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, p := range st.plugins {
		var tools []string
		for _, t := range p.Tools {
			tools = append(tools, t.Spec.Function.Name)
		}
		out = append(out, map[string]any{"name": p.Name, "version": p.Version, "tools": tools})
	}
	writeJSON(w, map[string]any{"plugins": out})
}

func (st *serverState) handleMCP(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, c := range globalMCPServers {
		c.mu.Lock()
		names := make([]string, 0, len(c.tools))
		for _, t := range c.tools {
			names = append(names, t.Name)
		}
		c.mu.Unlock()
		out = append(out, map[string]any{"name": c.name, "tools": names})
	}
	writeJSON(w, map[string]any{"mcp": out})
}

func (st *serverState) handleCron(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Text     string `json:"text"`
			EveryMin int    `json:"every_min"`
			DelayMin int    `json:"delay_min"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			http.Error(w, `{"error":"нужен JSON {"text":"…","every_min":N}"}`, http.StatusBadRequest)
			return
		}
		e := AddCron(req.Text, req.DelayMin, req.EveryMin)
		writeJSON(w, map[string]any{"ok": true, "entry": e})
		return
	}
	writeJSON(w, map[string]any{"cron": ListCron()})
}

func (st *serverState) handleProviders(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, p := range AllProviders() {
		out = append(out, map[string]any{
			"name": p.Name, "base": p.Base, "free": p.Free,
			"custom": p.Custom, "active": p.Name == st.cfg.Provider,
		})
	}
	writeJSON(w, map[string]any{"providers": out})
}

func (st *serverState) handleModels(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	p := ProviderByName(name)
	if p == nil {
		p = ProviderByName(st.cfg.Provider)
	}
	if p == nil {
		http.Error(w, `{"error":"провайдер не найден"}`, http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		InvalidateModelsCache()
	}
	infos, err := FetchModels(p, st.cfg, false)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "provider": p.Name, "models": infos})
}

// handleIndex — веб-панель: один HTML-файл, vanilla JS, без внешних зависимостей.
func (st *serverState) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, hubPageHTML)
}

const hubPageHTML = `<!DOCTYPE html>
<html lang="ru"><head><meta charset="utf-8"><title>Synergy Harness — hub</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--bg:#0d1117;--fg:#c9d1d9;--mut:#8b949e;--acc:#a371f7;--brd:#30363d;--grn:#3fb950}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,monospace}
header{display:flex;gap:12px;align-items:center;padding:12px 16px;border-bottom:1px solid var(--brd)}
header b{color:var(--acc)}.pill{border:1px solid var(--brd);border-radius:20px;padding:3px 12px;color:var(--mut)}
.tabs{display:flex;gap:8px;padding:12px 16px}
.tab{border:1px solid var(--brd);border-radius:20px;padding:4px 14px;cursor:pointer;color:var(--mut)}
.tab.on{background:var(--acc);color:#0d1117;border-color:var(--acc)}
main{padding:0 16px 80px}.card{border:1px solid var(--brd);border-radius:12px;padding:10px 14px;margin:8px 0}
.run{color:#d29922}.done{color:var(--grn)}
#chat{position:fixed;left:0;right:0;bottom:0;background:var(--bg);border-top:1px solid var(--brd);padding:12px 16px;display:flex;gap:8px}
#txt{flex:1;background:#161b22;border:1px solid var(--brd);border-radius:12px;color:var(--fg);padding:10px 14px;outline:none}
#send{background:var(--acc);border:0;border-radius:12px;color:#0d1117;padding:10px 18px;cursor:pointer;font-weight:700}
#log{white-space:pre-wrap}
</style></head><body>
<header><b>SYNERGY HARNESS</b><span class="pill" id="st-model">…</span><span class="pill" id="st-up">…</span></header>
<div class="tabs">
<div class="tab on" data-t="agents">агенты</div><div class="tab" data-t="plugins">плагины</div>
<div class="tab" data-t="mcp">mcp</div><div class="tab" data-t="cron">cron</div><div class="tab" data-t="providers">провайдеры</div>
</div>
<main id="view"></main>
<div id="chat"><input id="txt" placeholder="Сообщение агенту… (Enter — отправить)"><button id="send">➤</button></div>
<script>
const view=document.getElementById('view');let tab='agents';
async function j(u,o){const r=await fetch(u,o);return r.json()}
async function status(){const s=await j('/api/status');
document.getElementById('st-model').textContent=(s.provider||'?')+' | '+(s.model||'?');
document.getElementById('st-up').textContent='uptime '+s.uptime_s+'s • агентов '+s.sessions}
async function render(){
if(tab==='agents'){const d=await j('/api/agents');
view.innerHTML=(d.agents||[]).map(a=>'<div class="card">● <b>'+a.name+'</b> <span class="'+(a.running?'run">в процессе…':'done">готов')+'</span></div>').join('')||'<div class="card">нет живых агентов — спроси что-нибудь внизу</div>';
if(d.tasks&&d.tasks.length)view.innerHTML+='<h3>таски</h3>'+d.tasks.map(t=>'<div class="card">#'+t.id+' '+t.title+' — '+t.status+'</div>').join('')}
else if(tab==='plugins'){const d=await j('/api/plugins');
view.innerHTML=(d.plugins||[]).map(p=>'<div class="card">⚙ <b>'+p.name+'</b> v'+p.version+' — '+(p.tools||[]).join(', ')+'</div>').join('')||'<div class="card">нет плагинов</div>'}
else if(tab==='mcp'){const d=await j('/api/mcp');
view.innerHTML=(d.mcp||[]).map(c=>'<div class="card">◈ <b>'+c.name+'</b> — '+(c.tools||[]).join(', ')+'</div>').join('')||'<div class="card">нет MCP-серверов</div>'}
else if(tab==='cron'){const d=await j('/api/cron');
view.innerHTML=(d.cron||[]).map(c=>'<div class="card">⏰ '+c.id+' — '+c.text+' ('+(c.every_min?c.every_min+'мин':'разово')+')</div>').join('')||'<div class="card">пусто</div>'}
else if(tab==='providers'){const d=await j('/api/providers');
view.innerHTML=(d.providers||[]).map(p=>'<div class="card">'+(p.active?'▸ ':'')+'<b>'+p.name+'</b> '+(p.free?'(no-key)':'')+'<br><small>'+p.base+'</small></div>').join('')}}
document.querySelectorAll('.tab').forEach(t=>t.onclick=()=>{tab=t.dataset.t;
document.querySelectorAll('.tab').forEach(x=>x.classList.toggle('on',x===t));render()});
const txt=document.getElementById('txt');
async function send(){const text=txt.value.trim();if(!text)return;txt.value='';
view.innerHTML='<div class="card run">◐ думаю…</div>'+view.innerHTML;
const d=await j('/api/chat',{method:'POST',body:JSON.stringify({text})});
view.innerHTML='<div class="card '+(d.ok?'done':'run')+'">'+(d.ok?('✓ '+(d.answer||'').replace(/</g,'&lt;').slice(0,2000)):('✗ '+d.error))+'</div>'+view.innerHTML}
document.getElementById('send').onclick=send;
txt.addEventListener('keydown',e=>{if(e.key==='Enter'&&(e.ctrlKey||!e.shiftKey)){e.preventDefault();send()}});
status();render();setInterval(status,10000);
</script></body></html>`
