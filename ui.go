package main

// ui.go — модуль интерфейса: логика (модель, события, ввод, мышь, провайдеры).
// Рендер — в uiview.go. Переписано с нуля (2026) по мотивам anomalyco/opencode:
// зоны мыши считаются от реального layout'а, скругления — настоящий RoundedBorder,
// автодополнение видно во всех состояниях, нумерации нет нигде.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------- палитра (Tokyo Night) ----------
var (
	clAccent = lipgloss.Color("#c099ff")
	clBlue   = lipgloss.Color("#82aaff")
	clCyan   = lipgloss.Color("#86e1fc")
	clGreen  = lipgloss.Color("#c3e88d")
	clYellow = lipgloss.Color("#ffc777")
	clRed    = lipgloss.Color("#ff757f")
	clMuted  = lipgloss.Color("#636da6")
	clFG     = lipgloss.Color("#c8d3f5")
	clBG     = lipgloss.Color("#1a1b26")
	clSel    = lipgloss.Color("#2f334d")
	clOrange = lipgloss.Color("#ff966c")
	clPurple = lipgloss.Color("#c099ff")
	clBorder = lipgloss.Color("#3b4261")
)

// ---------- зоны мыши (своя реализация, без внешних зависимостей) ----------
type uiZone struct{ x0, y0, x1, y1 int }

type zoneMgr struct {
	zones map[string]uiZone
}

func newZoneMgr() *zoneMgr { return &zoneMgr{zones: map[string]uiZone{}} }
func (z *zoneMgr) reset()  { z.zones = map[string]uiZone{} }
func (z *zoneMgr) set(id string, x0, y0, x1, y1 int) {
	z.zones[id] = uiZone{x0, y0, x1, y1}
}
func (z *zoneMgr) hit(id string, m tea.MouseMsg) bool {
	if m.Action != tea.MouseActionPress || m.Button != tea.MouseButtonLeft {
		return false
	}
	r, ok := z.zones[id]
	return ok && m.X >= r.x0 && m.X <= r.x1 && m.Y >= r.y0 && m.Y <= r.y1
}

// ---------- тосты (внутриприложенные уведомления) ----------
type toast struct {
	kind string // ok | err | info
	msg  string
	born time.Time
}

// toastMsg — уведомление, пришедшее извне (тулы, cron, ретраи LLM).
type toastMsg struct{ kind, msg string }

var globalProgram *tea.Program

func init() {
	notifyApp = func(title, text string) {
		if globalProgram != nil {
			globalProgram.Send(toastMsg{kind: "info", msg: title + ": " + text})
		}
	}
}

// ---------- сообщения агента → UI ----------
type chatDeltaMsg struct {
	sid  int
	kind string // stream | tool | result | info
	text string
}

type chatDoneMsg struct {
	sid int
	err error
}

type modelsFetchedMsg struct {
	infos []ModelInfo
	err   error
}

type tickMsg time.Time

type sidebarAnimMsg time.Time

type cronFireMsg struct{ entry CronEntry }

// ---------- сессия чата ----------
type tline struct {
	kind string
	text string
}

type chatSession struct {
	id       int
	name     string
	workdir  string
	agent    *Agent
	cancel   context.CancelFunc
	running  bool
	pinned   bool
	title    string
	titleGen bool // заголовок уже сгенерирован
	mu       sync.Mutex
	lines    []tline
	queue    []string
}

// ---------- модель ----------
type completionState struct {
	visible bool
	items   []string
	sel     int
}

type tuiModel struct {
	width, height int
	bz            *zoneMgr
	sessions      []*chatSession
	cur           int
	input         mlEditor
	vp            viewportModel
	provider      string
	model         string
	models        []ModelInfo
	reasoning     int
	cfg           Config
	workdir       string
	llm           *LLMClient
	plugins       []*Plugin
	settings      bool
	sidebar       bool
	picker        bool
	pickerMode    string
	pickerTitle   string
	pickerItems   []string
	pickerSel     int
	toasts        []toast
	pickY0        int // Y первой строки пикера (для мыши)
	prog          *tea.Program
	nextSID       int
	// настройки
	ctrlEnterNewline bool
	failoverOn       bool
	failoverTarget   string
	sidebarRight     bool
	// автодополнение
	compl completionState
	// ввод ключа / мастер провайдера
	keyEdit  bool
	keyBuf   string
	provStep int
	provName string
	provURL  string
	provBuf  string
	// история ввода
	hist    []string
	histIdx int
	// картинки к следующему сообщению
	pendingImages []string
	pendingCmd    tea.Cmd
	// анимация панели
	sidebarAnim   int
	sidebarTarget bool
}

// viewportModel — минимальный скролл-вьюпорт (самодельный, без bubbles).
type viewportModel struct {
	Width, Height int
	lines         []string
	off           int // 0 = прижат к низу; >0 = прокрутка вверх
}

func (v *viewportModel) SetContent(s string) { v.lines = strings.Split(s, "\n") }
func (v *viewportModel) GotoBottom()         { v.off = 0 }
func (v *viewportModel) ScrollUp(n int)      { v.off += n }
func (v *viewportModel) ScrollDown(n int) {
	v.off -= n
	if v.off < 0 {
		v.off = 0
	}
}
func (v viewportModel) View() string {
	if v.Height <= 0 {
		return ""
	}
	end := len(v.lines) - v.off
	if end > len(v.lines) {
		end = len(v.lines)
	}
	start := end - v.Height
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return strings.Join(v.lines[start:end], "\n")
}

// ---------- конструктор ----------
func newTuiModel(cfg Config, llm *LLMClient, workdir string, plugins []*Plugin) *tuiModel {
	setUILang(orDefault(cfg.Lang, "ru"))
	RestoreCustomProviders(cfg)
	m := &tuiModel{
		cfg: cfg, llm: llm, workdir: workdir, plugins: plugins,
		provider:       Providers()[0].Name,
		model:          "(загружаю…)",
		reasoning:      1,
		width:          108,
		height:         30,
		bz:             newZoneMgr(),
		failoverOn:     cfg.FailoverEnabled,
		failoverTarget: cfg.FailoverTarget,
		sidebarRight:   cfg.SidebarSide == "right",
	}
	if !cfg.FailoverEnabled && cfg.FailoverTarget == "" {
		m.failoverOn = true // дефолт: включён
	}
	// восстановление последнего выбранного провайдера/модели из config.json
	// (со миграцией старых имён: «OpenCode Zen Free» → «OpenCode Zen no-key» и т.д.)
	if name := migrateProviderName(cfg.Provider); name != "" {
		if p := ProviderByName(name); p != nil {
			m.provider = p.Name
			m.cfg.Provider = p.Name // перезаписываем мигрированным именем
			m.llm.BaseURL = strings.TrimRight(p.Base, "/")
			if p.KeyFrom != nil {
				m.llm.APIKey = p.KeyFrom(m.cfg)
			}
		}
	} else if cfg.BaseURL != "" {
		if n := providerNameByBase(cfg.BaseURL); n != "" {
			m.provider = n
		}
	}
	if cfg.Model != "" {
		m.model = cfg.Model
	}
	m.input = newMlEditor()
	m.input.width = 90
	m.hist = loadInputHistory()
	m.histIdx = len(m.hist)
	m.vp.Width, m.vp.Height = 90, 20
	llm.OnRetry = func(msg string) {
		if m.prog != nil {
			m.prog.Send(toastMsg{kind: "err", msg: msg})
		}
	}
	m.addSession(workdir)
	return m
}

func (m *tuiModel) addSession(wd string) int {
	if wd == "" {
		wd = m.workdir
	}
	id := m.nextSID
	m.nextSID++
	s := &chatSession{id: id, name: fmt.Sprintf("chat%d", id+1), workdir: wd}
	s.agent = NewAgent(m.llm, wd, m.plugins...)
	attachMCP(s.agent)
	loadNamedSession(s.agent, s.name)
	s.mu.Lock()
	s.lines = append(s.lines, tline{"info", T("chats.workdir") + " " + wd})
	s.mu.Unlock()
	m.sessions = append(m.sessions, s)
	return len(m.sessions) - 1
}

func (m *tuiModel) switchProvider(name string) bool {
	p := ProviderByName(name)
	if p == nil {
		return false
	}
	m.provider = p.Name
	m.llm.BaseURL = strings.TrimRight(p.Base, "/")
	m.llm.SessionID = "" // новая сессия для нового бэкенда
	if p.KeyFrom != nil {
		m.llm.APIKey = p.KeyFrom(m.cfg)
	}
	m.model = "(загружаю…)"
	m.models = nil
	m.cfg.Provider = p.Name // запоминаем выбор между запусками
	m.cfg.BaseURL = m.llm.BaseURL
	m.saveSettings()
	return true
}

// ---------- фетч моделей (с пробингом для free) ----------
func (m *tuiModel) fetchCmd() tea.Cmd {
	p := ProviderByName(m.provider)
	cfg := m.cfg
	return func() tea.Msg {
		if p == nil {
			return modelsFetchedMsg{err: fmt.Errorf("провайдер не найден")}
		}
		infos, err := FetchModels(p, cfg, true)
		return modelsFetchedMsg{infos: infos, err: err}
	}
}

// ---------- запуск агента ----------
func (m *tuiModel) launchAgent(sid int, prompt string) tea.Cmd {
	// резолвим указатель на сессию НЕМЕДЛЕННО: если до исполнения команды
	// чат закроют (closeCurrent режет слайс), индекс sid устареет и в горутине
	// m.sessions[sid] паниковал бы index out of range — это и был краш из дампа.
	if sid < 0 || sid >= len(m.sessions) {
		return nil
	}
	s := m.sessions[sid]
	return func() tea.Msg {
		s.mu.Lock()
		s.lines = append(s.lines, tline{"user", prompt})
		s.title = trunc(prompt, 40)
		s.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.running = true
		a := s.agent
		a.StreamUI = true
		a.MaxSteps = []int{8, 25, 40}[m.reasoning]
		a.OnEvent = func(ev AgentEvent) {
			var k string
			switch ev.Kind {
			case "tool_call":
				k = "tool"
			case "tool_result":
				k = "result"
			case "info":
				k = "info"
			case "delta":
				k = "stream"
			default:
				return
			}
			if m.prog != nil {
				m.prog.Send(chatDeltaMsg{sid: sid, kind: k, text: renderEventText(ev)})
			}
		}
		_, err := a.Run(ctx, prompt)
		s.running = false
		s.cancel = nil
		saveNamedSession(a, s.name)
		return chatDoneMsg{sid: sid, err: err}
	}
}

// titleGenMsg — результат генерации заголовка чата.
type titleGenMsg struct {
	sid   int
	title string
}

// genTitleCmd — просит модель (cfg.TitleModel, иначе текущую) дать чату короткое имя.
func (m *tuiModel) genTitleCmd(sid int, firstUser string) tea.Cmd {
	llm := *m.llm // копия клиента: меняем только модель, стрим не трогаем
	if tm := strings.TrimSpace(m.cfg.TitleModel); tm != "" {
		llm.Model = tm
	}
	prompt := firstUser
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := llm.Complete(ctx, ChatRequest{
			Model: llm.Model,
			Messages: []Message{
				{Role: "system", Content: "Придумай очень короткое название чата (2-5 слов) по первому сообщению пользователя. Ответь ТОЛЬКО названием, без кавычек и пояснений."},
				{Role: "user", Content: prompt},
			},
			MaxTokens: 24,
		})
		if err != nil || resp == nil || len(resp.Choices) == 0 {
			return titleGenMsg{sid: sid}
		}
		t := strings.TrimSpace(resp.Choices[0].Message.Content)
		t = strings.Trim(t, "\"'«»")
		if t == "" {
			return titleGenMsg{sid: sid}
		}
		return titleGenMsg{sid: sid, title: trunc(t, 40)}
	}
}

func renderEventText(ev AgentEvent) string {
	switch ev.Kind {
	case "tool_call":
		return "⚙ " + ev.Title + " " + trunc(ev.Body, 90)
	case "tool_result":
		return "✓ " + ev.Title + " (" + ev.Elapsed.Round(time.Millisecond).String() + ")\n    " + trunc(ev.Body, 300)
	case "info":
		return ev.Body
	}
	return ev.Body
}

// ---------- Update ----------
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		if m.pendingCmd != nil {
			cmd := m.pendingCmd
			m.pendingCmd = nil
			m2, c2 := m.handleKey(msg)
			return m2, tea.Batch(cmd, c2)
		}
		return m.handleKey(msg)
	case sidebarAnimMsg:
		return m, m.stepSidebarAnim()
	case chatDeltaMsg:
		if msg.sid < 0 || msg.sid >= len(m.sessions) {
			return m, nil // сессия закрыта — поздняя дельта, игнорируем
		}
		s := m.sessions[msg.sid]
		s.mu.Lock()
		n := len(s.lines)
		if msg.kind == "stream" && n > 0 && s.lines[n-1].kind == "stream" {
			s.lines[n-1].text += msg.text
			if len(s.lines[n-1].text) > 4000 {
				s.lines[n-1].text = s.lines[n-1].text[:4000] + "…"
			}
		} else {
			s.lines = append(s.lines, tline{msg.kind, msg.text})
		}
		s.mu.Unlock()
		if msg.sid == m.cur {
			m.refreshTranscript()
		}
		return m, nil
	case chatDoneMsg:
		if msg.sid < 0 || msg.sid >= len(m.sessions) {
			return m, nil // сессия уже закрыта — позднее событие, игнорируем
		}
		s := m.sessions[msg.sid]
		s.running = false
		s.mu.Lock()
		if msg.err != nil {
			errTxt := friendlyAPIError(msg.err.Error())
			s.lines = append(s.lines, tline{"err", "✗ " + errTxt})
		} else if n := len(s.lines); n == 0 || s.lines[n-1].kind != "stream" {
			s.lines = append(s.lines, tline{"answer", "(ответ выше)"})
		}
		needTitle := msg.err == nil && !s.titleGen
		if needTitle {
			s.titleGen = true
		}
		var firstUser string
		if needTitle {
			for _, l := range s.lines {
				if l.kind == "user" {
					firstUser = l.text
					break
				}
			}
		}
		s.mu.Unlock()
		if needTitle && firstUser != "" {
			return m, m.genTitleCmd(msg.sid, firstUser)
		}
		var next string
		if len(s.queue) > 0 && msg.err == nil {
			next = s.queue[0]
			s.queue = s.queue[1:]
		}
		s.mu.Unlock()
		if msg.sid == m.cur {
			m.refreshTranscript()
		}
		if next != "" {
			m.addToast("info", T("queue.next"))
			return m, m.launchAgent(msg.sid, next)
		}
		m.addToast("ok", T("chat.done")+" "+s.name)
		notifySystem("SYNERGY", T("chat.done")+" "+s.name)
		return m, nil
	case modelsFetchedMsg:
		if msg.err != nil {
			m.addToast("err", T("models.err")+": "+msg.err.Error())
		} else {
			m.models = msg.infos
			var alive []string
			for _, mi := range msg.infos {
				if mi.Alive {
					alive = append(alive, mi.ID)
				}
			}
			cur := ""
			for _, mi := range m.models {
				if mi.ID == m.model {
					cur = mi.ID
				}
			}
			if cur == "" && len(alive) > 0 {
				m.model = alive[0]
				m.llm.Model = m.model
			}
			m.addToast("ok", fmt.Sprintf(T("models.count"), len(alive), len(m.models)))
		}
		return m, nil
	case titleGenMsg:
		if msg.title != "" && msg.sid >= 0 && msg.sid < len(m.sessions) {
			s := m.sessions[msg.sid]
			s.title = msg.title
			if s.agent != nil {
				saveNamedSession(s.agent, s.name)
			}
		}
		return m, nil
	case toastMsg:
		m.addToast(msg.kind, msg.msg)
		return m, nil
	case pasteMsg:
		m.input.insertText(msg.text)
		return m, nil
	case cronFireMsg:
		m.addToast("info", "⏰ cron: "+trunc(msg.entry.Text, 50))
		sid := m.cur
		return m, m.launchAgent(sid, msg.entry.Text)
	case tickMsg:
		keep := m.toasts[:0]
		for _, t := range m.toasts {
			if time.Since(t.born) < 6*time.Second {
				keep = append(keep, t)
			}
		}
		m.toasts = keep
		return m, tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return m, nil
}

// friendlyAPIError — понятные подсказки вместо сырых ответов API.
func friendlyAPIError(e string) string {
	switch {
	case strings.Contains(e, "MissingSessionID") || strings.Contains(e, "can only be used in OpenCode"):
		return T("err.zenfree")
	case strings.Contains(e, "API 401") || strings.Contains(e, "API 403"):
		return e + " — " + T("err.key")
	case strings.Contains(e, "Model is unavailable"):
		return T("err.modeldown")
	}
	return e
}

// ---------- клавиатура ----------
func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "ctrl+c", "ctrl+q":
		saveInputHistory(m.hist)
		return m, tea.Quit
	}

	// мастер кастомного провайдера
	if m.provStep > 0 {
		switch key {
		case "esc":
			m.provStep, m.provBuf, m.provName, m.provURL = 0, "", "", ""
			return m, nil
		case "enter":
			v := strings.TrimSpace(m.provBuf)
			m.provBuf = ""
			switch m.provStep {
			case 1:
				if v == "" {
					return m, nil
				}
				m.provName, m.provStep = v, 2
			case 2:
				if !strings.HasPrefix(v, "http") {
					m.addToast("err", "URL должен начинаться с http(s)://")
					return m, nil
				}
				m.provURL, m.provStep = strings.TrimRight(v, "/"), 3
			case 3:
				m.addCustomProvider(m.provName, m.provURL, v)
				m.provStep = 0
				return m, m.fetchCmd()
			}
			return m, nil
		case "backspace":
			if r := []rune(m.provBuf); len(r) > 0 {
				m.provBuf = string(r[:len(r)-1])
			}
			return m, nil
		case " ":
			m.provBuf += " "
			return m, nil
		default:
			if msg.Type == tea.KeyRunes {
				m.provBuf += string(msg.Runes)
			}
			return m, nil
		}
	}

	// ввод API-ключа
	if m.settings && m.keyEdit {
		switch key {
		case "esc":
			m.keyEdit, m.keyBuf = false, ""
			return m, nil
		case "enter":
			m.saveProviderKey(m.keyBuf)
			m.keyEdit, m.keyBuf = false, ""
			return m, m.fetchCmd()
		case "backspace":
			if r := []rune(m.keyBuf); len(r) > 0 {
				m.keyBuf = string(r[:len(r)-1])
			}
			return m, nil
		case " ":
			m.keyBuf += " "
			return m, nil
		default:
			if msg.Type == tea.KeyRunes {
				m.keyBuf += string(msg.Runes)
			}
			return m, nil
		}
	}

	if m.picker {
		switch key {
		case "up", "k":
			if m.pickerSel > 0 {
				m.pickerSel--
			}
			return m, nil
		case "down", "j":
			if m.pickerSel < len(m.pickerItems)-1 {
				m.pickerSel++
			}
			return m, nil
		case "enter":
			return m.applyPicker()
		case "esc":
			m.picker = false
			return m, nil
		}
	}

	if m.settings {
		switch key {
		case "esc":
			m.settings = false
			m.saveSettings()
			return m, nil
		case "left":
			m.cycleProvider(-1)
			return m, m.fetchCmd()
		case "right":
			m.cycleProvider(1)
			return m, m.fetchCmd()
		case "r", "R":
			m.reasoning = (m.reasoning + 1) % 3
			return m, nil
		case "l", "L":
			if uiLang == "ru" {
				setUILang("en")
			} else {
				setUILang("ru")
			}
			m.addToast("ok", T("toast.lang")+uiLang)
			return m, nil
		case "f", "F":
			m.failoverOn = !m.failoverOn
			m.addToast("ok", T("toast.failover")+onOff(m.failoverOn))
			return m, nil
		case "b", "B":
			m.sidebarRight = !m.sidebarRight
			m.addToast("ok", T("toast.sidebar")+m.sidebarSideName())
			m.layout()
			return m, nil
		case "e", "E":
			m.ctrlEnterNewline = !m.ctrlEnterNewline
			return m, nil
		case "k", "K":
			m.keyEdit, m.keyBuf = true, ""
			return m, nil
		case "enter", "m":
			m.openModelsPicker()
			return m, nil
		}
	}

	switch key {
	case "ctrl+n":
		m.addSession(m.workdir)
		m.cur = len(m.sessions) - 1
		m.refreshTranscript()
		return m, nil
	case "ctrl+s":
		m.settings = !m.settings
		return m, nil
	case "ctrl+l":
		return m, m.toggleSidebar()
	case "ctrl+m":
		m.openModelsPicker()
		return m, nil
	case "ctrl+p":
		if m.histIdx > 0 && len(m.hist) > 0 {
			m.histIdx--
			m.input.Reset()
			m.input.insertText(m.hist[m.histIdx])
		}
		return m, nil
	case "ctrl+y":
		return m, clipboardPasteCmd()
	case "esc":
		if m.compl.visible {
			m.compl.visible = false
			return m, nil
		}
		m.settings = false
		return m, nil
	case "enter":
		if m.compl.visible {
			m.completeApply()
			return m, nil
		}
		if m.smartEnterShouldSubmit() {
			return m, m.submitInput()
		}
		m.input.newline()
		return m, nil
	case "ctrl+j", "ctrl+enter":
		if m.ctrlEnterNewline {
			m.input.newline()
			return m, nil
		}
		return m, m.submitInput()
	case "tab":
		if m.compl.visible && len(m.compl.items) > 0 {
			m.completeApply()
			return m, nil
		}
		m.completeTab()
		return m, nil
	case "shift+tab":
		if m.compl.visible && len(m.compl.items) > 1 {
			m.compl.sel = (m.compl.sel + 1) % len(m.compl.items)
		}
		return m, nil
	case "up":
		if m.compl.visible {
			if m.compl.sel > 0 {
				m.compl.sel--
			}
			return m, nil
		}
		if m.input.cy > 0 {
			m.input.moveVert(-1)
			return m, nil
		}
		m.vp.ScrollUp(1)
		return m, nil
	case "down":
		if m.compl.visible {
			if m.compl.sel < len(m.compl.items)-1 {
				m.compl.sel++
			}
			return m, nil
		}
		if m.input.cy < len(m.input.lines)-1 {
			m.input.moveVert(1)
			return m, nil
		}
		m.vp.ScrollDown(1)
		return m, nil
	case "pgup":
		m.vp.ScrollUp(m.vp.Height / 2)
		return m, nil
	case "pgdown":
		m.vp.ScrollDown(m.vp.Height / 2)
		return m, nil
	case "left":
		if m.input.cx > 0 {
			m.input.cx--
		}
		return m, nil
	case "right":
		if m.input.cx < len([]rune(m.input.lines[m.input.cy])) {
			m.input.cx++
		}
		return m, nil
	case "home", "ctrl+a":
		m.input.cx = 0
		return m, nil
	case "end", "ctrl+e":
		m.input.cx = len([]rune(m.input.lines[m.input.cy]))
		return m, nil
	case "backspace":
		m.input.backspace()
		m.liveComplete()
		return m, nil
	case "delete":
		m.input.deleteForward()
		return m, nil
	case "ctrl+w":
		m.input.deleteWord()
		return m, nil
	case "ctrl+u":
		m.input.lines[m.input.cy] = string([]rune(m.input.lines[m.input.cy])[m.input.cx:])
		m.input.cx = 0
		return m, nil
	}

	// KeySpace на десктопных терминалах — отдельный тип, ловим явно
	if msg.Type == tea.KeySpace {
		m.input.insertText(" ")
		m.liveComplete()
		return m, nil
	}
	if msg.Type == tea.KeyRunes || msg.Paste {
		m.input.insertText(string(msg.Runes))
		m.liveComplete()
		return m, nil
	}
	return m, nil
}

// ---------- отправка / очередь / steer ----------
func (m *tuiModel) submitInput() tea.Cmd {
	line := strings.TrimRight(m.input.Value(), "\n")
	m.input.Reset()
	m.compl.visible = false
	if t := strings.TrimSpace(line); t != "" {
		m.hist = append(m.hist, t)
		if len(m.hist) > 200 {
			m.hist = m.hist[len(m.hist)-200:]
		}
		m.histIdx = len(m.hist)
		saveInputHistory(m.hist)
	}
	if strings.TrimSpace(line) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "/") {
		return m.dispatchCommand(trimmed)
	}
	s := m.sessions[m.cur]
	if len(m.pendingImages) > 0 {
		s.agent.pendingImages = append(s.agent.pendingImages, m.pendingImages...)
		m.pendingImages = nil
	}
	if s.running {
		if len([]rune(trimmed)) <= 400 {
			select {
			case s.agent.Steer <- trimmed:
				m.addToast("info", T("steer.ok"))
			default:
				m.addToast("err", T("steer.full"))
			}
		} else {
			s.mu.Lock()
			s.queue = append(s.queue, line)
			s.mu.Unlock()
			m.addToast("info", fmt.Sprintf(T("queue.added"), len(s.queue)))
		}
		return nil
	}
	return m.launchAgent(m.cur, line)
}

func (m *tuiModel) smartEnterShouldSubmit() bool {
	v := m.input.Value()
	if strings.Contains(v, "\n") {
		return false
	}
	t := strings.TrimSpace(v)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "/") {
		return true
	}
	return len([]rune(t)) <= 300
}

// ---------- автодополнение ----------
func (m *tuiModel) liveComplete() {
	// «по ходу набора»: если поле начинается с / — обновляем кандидатов сразу
	v := m.input.Value()
	if strings.HasPrefix(v, "/") && !strings.Contains(v, "\n") {
		m.completeTab()
	} else if m.compl.visible {
		m.compl.visible = false
	}
}

func (m *tuiModel) completeTab() {
	cur := m.input.Value()
	if !strings.HasPrefix(cur, "/") || strings.Contains(cur, "\n") {
		m.compl.visible = false
		return
	}
	fields := strings.Fields(cur)
	var cands []string
	if len(fields) <= 1 {
		for _, c := range slashCommands {
			if strings.HasPrefix(c, cur) {
				cands = append(cands, c)
			}
		}
	} else if fields[0] == "/provider" || fields[0] == "/providers" {
		tail := strings.ToLower(fields[len(fields)-1])
		for _, p := range Providers() {
			if strings.HasPrefix(strings.ToLower(p.Name), tail) {
				cands = append(cands, "/provider "+p.Name)
			}
		}
	}
	if len(cands) == 0 {
		m.compl.visible = false
		return
	}
	if len(cands) == 1 && cands[0] == cur {
		m.compl.visible = false
		return
	}
	if len(cands) == 1 {
		m.input.Reset()
		m.input.insertText(cands[0])
		m.compl.visible = false
		return
	}
	m.compl = completionState{visible: true, items: cands, sel: 0}
}

func (m *tuiModel) completeApply() {
	if !m.compl.visible || len(m.compl.items) == 0 {
		return
	}
	pick := m.compl.items[m.compl.sel]
	m.compl.visible = false
	m.input.Reset()
	m.input.insertText(pick)
	if strings.HasPrefix(pick, "/") && len(strings.Fields(pick)) == 1 {
		if cmd := m.dispatchCommand(strings.TrimSpace(pick)); cmd != nil {
			m.input.Reset()
			m.pendingCmd = cmd
		}
	}
}

// ---------- пикер ----------
func (m *tuiModel) openPicker(mode, title string, items []string) {
	m.picker, m.pickerMode, m.pickerTitle, m.pickerItems, m.pickerSel = true, mode, title, items, 0
}

func (m *tuiModel) openModelsPicker() {
	if len(m.models) == 0 {
		return
	}
	var items []string
	for _, mi := range m.models {
		label := mi.ID
		if !mi.Alive {
			label += "  ✗ " + trunc(mi.Err, 30)
		}
		items = append(items, label)
	}
	m.openPicker("models", T("picker.models")+" — "+providerLabel(m.provider), items)
}

func (m *tuiModel) applyPicker() (tea.Model, tea.Cmd) {
	m.picker = false
	sel := m.pickerSel
	if sel < 0 || sel >= len(m.pickerItems) {
		return m, nil
	}
	item := m.pickerItems[sel]
	var cmd tea.Cmd
	switch m.pickerMode {
	case "models":
		id := strings.Fields(item)[0]
		m.model = id
		m.llm.Model = id
		m.cfg.Model = id // запоминаем модель между запусками
		m.saveSettings()
		m.addToast("ok", T("model.set")+" "+id)
	case "provider":
		if item == T("provider.custom") {
			m.provStep, m.provBuf = 1, ""
			m.settings = true
			break
		}
		if m.switchProvider(item) {
			m.addToast("ok", T("provider.set")+" "+item)
			cmd = m.fetchCmd()
		}
	case "chat":
		if sel < len(m.sessions) {
			m.cur = sel
			m.refreshTranscript()
		}
	case "history":
		idx := m.addSession(m.workdir)
		s := m.sessions[idx]
		s.name = item
		loadNamedSession(s.agent, item)
		m.cur = idx
		m.refreshTranscript()
		m.addToast("ok", T("chat.loaded")+" "+item)
	}
	return m, cmd
}

// ---------- провайдеры: helpers ----------
func providerLabel(name string) string { return name }

func (m *tuiModel) cycleProvider(dir int) {
	provs := Providers()
	for i, p := range provs {
		if p.Name == m.provider {
			ni := (i + dir + len(provs)) % len(provs)
			m.switchProvider(provs[ni].Name)
			m.addToast("info", T("provider.set")+" "+provs[ni].Name)
			return
		}
	}
}

func (m *tuiModel) addCustomProvider(name, baseURL, key string) {
	AddCustomProvider(name, baseURL)
	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]*ProviderCfg{}
	}
	m.cfg.Providers[name] = &ProviderCfg{BaseURL: baseURL, APIKey: key}
	m.saveSettings()
	m.switchProvider(name)
	m.addToast("ok", T("provider.added")+" "+name)
}

func (m *tuiModel) saveProviderKey(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		m.addToast("err", T("key.empty"))
		return
	}
	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]*ProviderCfg{}
	}
	pc := m.cfg.Providers[m.provider]
	if pc == nil {
		pc = &ProviderCfg{}
		m.cfg.Providers[m.provider] = pc
	}
	pc.APIKey = key
	m.llm.APIKey = key
	if data, err := jsonMarshalIndent(m.cfg); err == nil {
		os.WriteFile(filepath.Join(getConfigDir(), "config.json"), data, 0o600)
	}
	m.addToast("ok", T("key.saved")+" "+m.provider)
}

func (m *tuiModel) providerKeyStatus() string {
	if p := ProviderByName(m.provider); p != nil && p.KeyFrom != nil {
		return p.KeyFrom(m.cfg)
	}
	return m.llm.APIKey
}

func (m *tuiModel) saveSettings() {
	m.cfg.Lang = uiLang
	m.cfg.SidebarSide = "left"
	if m.sidebarRight {
		m.cfg.SidebarSide = "right"
	}
	m.cfg.FailoverEnabled = m.failoverOn
	m.cfg.FailoverTarget = m.failoverTarget
	if data, err := jsonMarshalIndent(m.cfg); err == nil {
		os.WriteFile(filepath.Join(getConfigDir(), "config.json"), data, 0o600)
	}
}

func onOff(b bool) string {
	if b {
		return T("on")
	}
	return T("off")
}

func (m *tuiModel) sidebarSideName() string {
	if m.sidebarRight {
		return T("right")
	}
	return T("left")
}

// ---------- сайдбар: анимация ----------
func (m *tuiModel) toggleSidebar() tea.Cmd {
	m.sidebarTarget = !m.sidebarTarget
	return tea.Tick(45*time.Millisecond, func(t time.Time) tea.Msg { return sidebarAnimMsg(t) })
}

func (m *tuiModel) stepSidebarAnim() tea.Cmd {
	if m.sidebarTarget {
		m.sidebar = true
		if m.sidebarAnim < 8 {
			m.sidebarAnim++
			m.layout()
			return tea.Tick(45*time.Millisecond, func(t time.Time) tea.Msg { return sidebarAnimMsg(t) })
		}
	} else {
		if m.sidebarAnim > 0 {
			m.sidebarAnim--
			m.layout()
			return tea.Tick(45*time.Millisecond, func(t time.Time) tea.Msg { return sidebarAnimMsg(t) })
		}
		m.sidebar = false
		m.layout()
	}
	return nil
}

// sidebarWidth — текущая (анимированная) ширина панели.
// Держим ≥ 36: должны влезать кнопки «＋ новый ✕ удалить ◆ закреп» на полном кадре анимации.
func (m tuiModel) sidebarWidth() int {
	w := 4 + m.sidebarAnim*4
	if w > 38 {
		w = 38
	}
	return w
}

func (m tuiModel) sidebarVisible() bool {
	return (m.sidebar || m.sidebarAnim > 0) && m.width >= 76
}

// sidebarOverlay — при ширине < 96 панель рисуется поверх контента (не сужая его),
// иначе на узком терминале основная колонка схлопывается до нечитаемости.
func (m tuiModel) sidebarOverlay() bool {
	return m.sidebarVisible() && m.width < 96
}

// ---------- тосты ----------
func (m *tuiModel) addToast(kind, msg string) {
	m.toasts = append(m.toasts, toast{kind: kind, msg: msg, born: time.Now()})
	if len(m.toasts) > 4 {
		m.toasts = m.toasts[len(m.toasts)-4:]
	}
}

// ---------- мышь ----------
func (m tuiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp {
		m.vp.ScrollUp(3)
		return m, nil
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown {
		m.vp.ScrollDown(3)
		return m, nil
	}
	if m.bz.hit("btn_side", msg) {
		return m, m.toggleSidebar()
	}
	if m.bz.hit("btn_new", msg) {
		m.addSession(m.workdir)
		m.cur = len(m.sessions) - 1
		m.refreshTranscript()
		return m, nil
	}
	if m.bz.hit("btn_models", msg) || m.bz.hit("chip", msg) {
		m.openModelsPicker()
		return m, nil
	}
	if m.bz.hit("btn_cfg", msg) {
		m.settings = !m.settings
		return m, nil
	}
	if m.bz.hit("btn_quit", msg) {
		saveInputHistory(m.hist)
		m.saveSettings()
		return m, tea.Quit
	}
	if m.bz.hit("chat_new", msg) {
		m.addSession(m.workdir)
		m.cur = len(m.sessions) - 1
		m.refreshTranscript()
		return m, nil
	}
	if m.bz.hit("chat_del", msg) {
		m.closeCurrent()
		m.refreshTranscript()
		return m, nil
	}
	if m.bz.hit("chat_pin", msg) {
		s := m.sessions[m.cur]
		s.pinned = !s.pinned
		if s.pinned {
			m.addToast("ok", T("toast.pinned")+" "+s.name)
		} else {
			m.addToast("info", T("toast.unpinned")+" "+s.name)
		}
		return m, nil
	}
	for _, n := range listSavedSessions() {
		if m.bz.hit("saved:"+n, msg) {
			idx := m.addSession(m.workdir)
			s := m.sessions[idx]
			s.name = n
			loadNamedSession(s.agent, n)
			m.cur = idx
			m.refreshTranscript()
			m.addToast("ok", T("chat.loaded")+" "+n)
			return m, nil
		}
	}
	for i := range m.sessions {
		if m.bz.hit(fmt.Sprintf("sess:%d", i), msg) {
			if i != m.cur {
				m.cur = i
				m.refreshTranscript()
			}
			return m, nil
		}
	}
	// клик по пункту пикера — зоны создаются в renderPicker как pick:N
	if m.picker {
		for i := range m.pickerItems {
			if m.bz.hit(fmt.Sprintf("pick:%d", i), msg) {
				m.pickerSel = i
				_, c := m.applyPicker()
				return m, c
			}
		}
	}
	return m, nil
}

// ---------- история ввода ----------
func inputHistoryPath() string { return filepath.Join(getConfigDir(), "input_history.txt") }

func loadInputHistory() []string {
	data, err := os.ReadFile(inputHistoryPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}

func saveInputHistory(hist []string) {
	if len(hist) > 200 {
		hist = hist[len(hist)-200:]
	}
	os.WriteFile(inputHistoryPath(), []byte(strings.Join(hist, "\n")), 0o600)
}

func listSavedSessions() []string {
	des, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".json") {
			out = append(out, strings.TrimSuffix(de.Name(), ".json"))
		}
	}
	return out
}

// ---------- буфер обмена ----------
type pasteMsg struct{ text string }

func clipboardPasteCmd() tea.Cmd {
	return func() tea.Msg {
		p, err := LookPathSafe("termux-clipboard-get")
		if err != nil {
			return toastMsg{kind: "err", msg: T("clip.none")}
		}
		out, err := runRawOutput(context.Background(), 10*time.Second, p)
		if err != nil {
			return toastMsg{kind: "err", msg: "clipboard: " + err.Error()}
		}
		return pasteMsg{text: out}
	}
}

// ---------- уведомления (системные) ----------
func haveTermuxNotify() bool {
	_, err := LookPathSafe("termux-notification")
	return err == nil
}

func notifySystem(title, body string) {
	body = strings.NewReplacer("\n", " ", "`", "").Replace(body)
	if len(body) > 200 {
		body = trunc(body, 200)
	}
	if cmdline := os.Getenv("SYNERGY_NOTIFY_CMD"); cmdline != "" {
		c := strings.ReplaceAll(cmdline, "{msg}", title+": "+body)
		runRawDetached(shPath(), "-c", c)
		return
	}
	if p, err := LookPathSafe("termux-notification"); err == nil {
		runRawDetached(p, "--title", title, "--content", title+": "+body)
	}
}

func notifyInfo() string {
	if c := os.Getenv("SYNERGY_NOTIFY_CMD"); c != "" {
		return "cmd: " + trunc(c, 30)
	}
	if haveTermuxNotify() {
		return "termux-notification ✓"
	}
	return T("notify.none")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- команды ----------
func (m *tuiModel) dispatchCommand(line string) tea.Cmd {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	cmd := fields[0]
	if n, err := strconvAtoi(cmd); err == nil {
		if n >= 1 && n <= len(m.sessions) {
			m.cur = n - 1
			m.refreshTranscript()
		}
		return nil
	}
	switch cmd {
	case "/history":
		names := listSavedSessions()
		if len(names) == 0 {
			m.addToast("info", "сохранённых чатов нет")
		} else {
			m.openPicker("history", "История чатов", names)
		}
	case "/titlemodel":
		if len(fields) < 2 {
			cur := m.cfg.TitleModel
			if cur == "" {
				cur = m.model + " (текущая)"
			}
			m.addToast("info", "модель заголовков: "+cur+" — сменить: /titlemodel <id>, сброс: /titlemodel auto")
			break
		}
		if fields[1] == "auto" {
			m.cfg.TitleModel = ""
			m.addToast("ok", "заголовки генерит текущая модель")
		} else {
			m.cfg.TitleModel = fields[1]
			m.addToast("ok", "модель заголовков: "+fields[1])
		}
		m.saveSettings()
	case "/lang":
		if uiLang == "ru" {
			setUILang("en")
		} else {
			setUILang("ru")
		}
		m.saveSettings()
		m.addToast("ok", T("toast.lang")+uiLang)
	case "/attach", "/image", "/img":
		if len(fields) < 2 {
			m.addToast("err", "использование: /attach <путь к картинке> [ещё пути]")
			break
		}
		if !modelSupportsVision(m.model) {
			m.addToast("err", "модель "+shortModel(m.model)+" не vision — картинки не уйдут. Смени: /models")
			break
		}
		s := m.sessions[m.cur]
		ok := 0
		for _, p := range fields[1:] {
			du, err := imageToDataURL(p)
			if err != nil {
				m.addToast("err", p+": "+err.Error())
				continue
			}
			s.agent.pendingImages = append(s.agent.pendingImages, du)
			ok++
		}
		if ok > 0 {
			m.addToast("ok", fmt.Sprintf("📎 прикреплено: %d изобр. — уйдут со следующим сообщением", ok))
		}
	case "/detach":
		m.sessions[m.cur].agent.pendingImages = nil
		m.addToast("ok", "прикрепления сняты")
	case "/help", "/?":
		m.openPicker("help", "Команды Synergy Harness", helpCommands)
	case "/models":
		if len(fields) > 1 && fields[1] == "refresh" {
			InvalidateModelsCache()
			return m.fetchCmd()
		}
		m.openModelsPicker()
	case "/model":
		if len(fields) < 2 {
			m.addToast("err", "использование: /model <id> — своя модель, например /model gpt-5.5")
			break
		}
		id := fields[1]
		m.model = id
		m.llm.Model = id
		m.cfg.Model = id
		m.saveSettings()
		m.addToast("ok", T("model.set")+" "+id)
	case "/providers", "/provider":
		if cmd == "/provider" && len(fields) > 1 {
			name := strings.Join(fields[1:], " ")
			if !m.switchProvider(name) {
				m.addToast("err", "провайдер не найден")
			} else {
				m.addToast("ok", "провайдер: "+name)
				return m.fetchCmd()
			}
			break
		}
		var names []string
		for _, p := range AllProviders() {
			names = append(names, p.Name)
		}
		names = append(names, "＋ свой провайдер (custom)")
		m.openPicker("provider", "Провайдеры", names)
	case "/tasks":
		ts := ListTasks()
		if len(ts) == 0 {
			m.addToast("info", "тасков нет — агент открывает их сам (лимит 2)")
		} else {
			var rows []string
			for _, t := range ts {
				mark := "○"
				if t.Status == "done" {
					mark = "●"
				}
				rows = append(rows, fmt.Sprintf("%s #%d %s — %s", mark, t.ID, t.Title, t.Status))
			}
			m.openPicker("help", "Таски сессии (лимит 2 активных)", rows)
		}
	case "/cron":
		es := ListCron()
		if len(es) == 0 {
			m.addToast("info", "триггеров нет — агент ставит через schedule_trigger")
		} else {
			var rows []string
			for _, e := range es {
				st := "разово"
				if e.Every > 0 {
					st = fmt.Sprintf("каждые %d мин", e.Every)
				}
				on := "on"
				if !e.Enabled {
					on = "off"
				}
				rows = append(rows, fmt.Sprintf("[%s] %s — %s (%s)", e.ID, e.Text, st, on))
			}
			m.openPicker("cron", "Триггеры (cron)", rows)
		}
	case "/cron-del":
		if len(fields) > 1 && RemoveCron(fields[1]) {
			m.addToast("ok", "триггер "+fields[1]+" удалён")
		} else {
			m.addToast("err", "использование: /cron-del <id>")
		}
	case "/steer":
		if len(fields) > 1 {
			s := m.sessions[m.cur]
			txt := strings.Join(fields[1:], " ")
			if s.running {
				select {
				case s.agent.Steer <- txt:
					m.addToast("info", "steer: "+trunc(txt, 60))
				default:
					m.addToast("err", "очередь steer переполнена")
				}
			} else {
				m.addToast("err", "агент не запущен — steer ни к чему не применить")
			}
		}
	case "/queue":
		s := m.sessions[m.cur]
		s.mu.Lock()
		q := append([]string{}, s.queue...)
		s.mu.Unlock()
		if len(q) == 0 {
			m.addToast("info", "очередь пуста")
		} else {
			m.openPicker("queue", fmt.Sprintf("Очередь сообщений (%d)", len(q)), q)
		}
	case "/new":
		m.addSession(m.workdir)
		m.cur = len(m.sessions) - 1
		m.refreshTranscript()
	case "/chat":
		if len(fields) > 1 {
			switch fields[1] {
			case "new":
				m.addSession(m.workdir)
				m.cur = len(m.sessions) - 1
			case "next":
				m.cur = (m.cur + 1) % len(m.sessions)
			case "prev":
				m.cur = (m.cur - 1 + len(m.sessions)) % len(m.sessions)
			case "close":
				m.closeCurrent()
			}
			m.refreshTranscript()
		} else {
			var names []string
			for _, s := range m.sessions {
				names = append(names, s.name+" — "+s.workdir)
			}
			m.openPicker("chat", "Чаты", names)
		}
	case "/sessions":
		return m.toggleSidebar()
	case "/reset", "/wipe":
		m.sessions[m.cur].agent.Reset()
		ResetTasks()
		os.Remove(filepath.Join(sessionsDir(), m.sessions[m.cur].name+".json"))
		m.addToast("ok", "контекст чата и таски очищены")
	case "/compact":
		s := m.sessions[m.cur]
		sid := m.cur
		return func() tea.Msg {
			msg, err := s.agent.compactContext(context.Background())
			if err != nil {
				return chatDeltaMsg{sid: sid, kind: "err", text: err.Error()}
			}
			saveNamedSession(s.agent, s.name)
			return chatDeltaMsg{sid: sid, kind: "info", text: "сжатие: " + msg}
		}
	case "/memory":
		data, err := os.ReadFile(filepath.Join(workspaceDir(m.workdir), "..", ".synergy_memory.md"))
		if err != nil {
			m.addToast("info", "память пуста")
		} else {
			m.openPicker("help", "Память", strings.Split(clampStr(string(data), 2000), "\n"))
		}
	case "/plugins":
		var names []string
		for _, p := range m.plugins {
			names = append(names, fmt.Sprintf("%s v%s (%d тулов)", p.Name, p.Version, len(p.Tools)))
		}
		if len(names) == 0 {
			m.addToast("info", "плагинов нет")
		} else {
			m.openPicker("help", "Плагины", names)
		}
	case "/sandbox":
		if len(fields) > 1 && (fields[1] == "on" || fields[1] == "off") {
			os.Setenv("SYNERGY_SANDBOX", fields[1])
			m.addToast("ok", "сандбокс: "+fields[1]+" (для новых агентов)")
		} else {
			m.addToast("info", fmt.Sprintf("сандбокс: %s", map[bool]string{true: "on", false: "off"}[sandboxEnabled()]))
		}
	case "/workdir":
		if len(fields) > 1 {
			s := m.sessions[m.cur]
			s.workdir = fields[1]
			s.mu.Lock()
			s.lines = append(s.lines, tline{"info", "рабочая папка: " + fields[1]})
			s.mu.Unlock()
			m.addToast("ok", "workdir: "+fields[1])
		}
	case "/notify":
		text := strings.TrimSpace(strings.TrimPrefix(line, "/notify"))
		notifySystem("Synergy Harness", text)
		m.addToast("ok", "уведомление отправлено")
	case "/stats":
		s := m.sessions[m.cur]
		m.addToast("info", fmt.Sprintf("чат %s: %d сообщений, ~%d KB, %d токенов", s.name, len(s.agent.Messages), s.agent.contextSize()/1024, s.agent.TotalTok))
	case "/quit", "/exit":
		return tea.Quit
	default:
		m.addToast("err", "неизвестная команда "+cmd+" — /help")
	}
	return nil
}

func (m *tuiModel) closeCurrent() {
	if len(m.sessions) <= 1 {
		m.sessions[0].agent.Reset()
		ResetTasks()
		m.refreshTranscript()
		return
	}
	s := m.sessions[m.cur]
	if s.cancel != nil {
		s.cancel()
	}
	m.sessions = append(m.sessions[:m.cur], m.sessions[m.cur+1:]...)
	if m.cur >= len(m.sessions) {
		m.cur = len(m.sessions) - 1
	}
}

func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("bad")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

var helpCommands = []string{
	"/help                 справка",
	"/models [refresh]     выбор модели провайдера",
	"/providers            список провайдеров (как у моделей)",
	"/provider [имя]       быстрая смена провайдера",
	"/tasks                таски сессии (лимит 2 активных)",
	"/cron                 триггеры/напоминания",
	"/cron-del <id>        удалить триггер",
	"/new                  новый чат",
	"/chat <new|next|prev|close>   управление чатами",
	"/sessions             панель чатов вкл/выкл",
	"/attach <файл>        прикрепить картинку (vision-модели)",
	"/detach               снять прикрепления",
	"/compact              сжать контекст",
	"/reset                очистить контекст чата",
	"/memory               память агента",
	"/plugins              плагины",
	"/sandbox <on|off>     сандбокс",
	"/workdir <путь>       рабочая папка чата",
	"/stats                статистика",
	"/1..9                 переключить чат по номеру",
	"/quit                 выход",
}

// jsonMarshalIndent — маршал с отступами (единая точка для записи конфигов).
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// runNetTest — ручная проверка API: 3 запроса к активному провайдеру, печать кодов и латентности.
func runNetTest() {
	llm := NewLLMClient()
	// конфиг напрямую из файла (без интерактивного сетапа)
	var cfg Config
	if data, err := os.ReadFile(filepath.Join(getConfigDir(), "config.json")); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.APIKey != "" {
		llm.APIKey = cfg.APIKey
	}
	if cfg.BaseURL != "" {
		llm.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	if cfg.Model != "" {
		llm.Model = cfg.Model
	}
	fmt.Printf("== Synergy Harness net test ==\nprovider base: %s\nmodel: %s\n", llm.BaseURL, llm.Model)
	for i := 1; i <= 3; i++ {
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		resp, err := llm.Complete(ctx, ChatRequest{
			Messages: []Message{{Role: "user", Content: "Скажи OK"}},
		})
		cancel()
		if err != nil {
			fmt.Printf("  попытка %d: FAIL (%v, %.1fs)\n", i, err, time.Since(t0).Seconds())
		} else if resp != nil && len(resp.Choices) > 0 {
			fmt.Printf("  попытка %d: 200 OK (%.1fs) -> %s\n", i, time.Since(t0).Seconds(), clampStr(resp.Choices[0].Message.Content, 60))
		} else {
			fmt.Printf("  попытка %d: 200 OK, но пустой ответ (%.1fs)\n", i, time.Since(t0).Seconds())
		}
	}
}
