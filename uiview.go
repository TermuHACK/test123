package main

// uiview.go — рендер интерфейса (View + все отрисовщики).
// Переписан с нуля: настоящие скругления lipgloss.RoundedBorder, кнопки по углам
// через JoinHorizontal (без ручного подсчёта ширин — не съезжают никогда),
// зоны мыши сайдбара вычисляются от реальной стороны панели.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------- layout ----------
func (m *tuiModel) layout() {
	w := m.width - 2
	mainW := w
	if m.sidebarVisible() && !m.sidebarOverlay() {
		mainW = w - m.sidebarWidth()
	}
	s := m.sessions[m.cur]
	s.mu.Lock()
	empty := !s.running && (len(s.lines) == 0 || (len(s.lines) == 1 && s.lines[0].kind == "info"))
	s.mu.Unlock()
	// ширина инпута фиксирована и НЕ зависит от текста — только от ширины колонки
	if empty {
		m.input.width = m.inputWidthFor(true) - 4
	} else {
		m.input.width = mainW - 6
	}
	m.vp.Width = mainW
	// потолок высоты инпута: не более 40% высоты экрана, чтобы не съесть транскрипт
	maxEd := (m.height - topBarRows - 4) * 2 / 5
	if maxEd < 3 {
		maxEd = 3
	}
	m.input.maxVisible = maxEd // внутри — вертикальный скролл (scrollOff в editor)
	edH := m.input.heightInLines()
	// вьюпорт = экран минус шапка(3) минус инпут(рамка 2 + строки) минус статус-строка(1)
	m.vp.Height = m.height - topBarRows - 3 - edH
	if m.vp.Height < 3 {
		m.vp.Height = 3
	}
	m.refreshTranscript()
}

func (m tuiModel) mainColWidth() int {
	w := m.width - 2
	if m.sidebarVisible() && !m.sidebarOverlay() {
		w -= m.sidebarWidth()
	}
	return w
}

func (m tuiModel) inputWidthFor(empty bool) int {
	// инпут всегда на всю ширину рабочей колонки — без сужения на главном экране
	w := m.mainColWidth() - 4
	if w < 20 {
		w = 20
	}
	return w
}

// topBarRows — реальная высота шапки: пустая строка-отступ + кнопки + разделитель.
// renderTop: "\n"(1) + row(2) + "\n"(3) + separator(4) → 4 строки.
// Несовпадение этой константы с реальной высотой сдвигает ВСЕ зоны кликов (пикер,
// сайдбар) — клик по строке N попадал в N+1 («выбрал big-pickle, получил glm»).
const topBarRows = 4

// ---------- корневой View ----------
func (m tuiModel) View() string {
	m.bz.reset()
	if m.width < 60 {
		return m.miniView()
	}
	out := lipgloss.JoinVertical(lipgloss.Top, m.renderTop(), m.renderBody())
	if toasts := m.renderToasts(); toasts != "" {
		// оверлей поверх нижних строк, НЕ добавление новых — иначе экран вырастает
		// за m.height и терминал срезает шапку сверху
		outLines := strings.Split(out, "\n")
		tLines := strings.Split(toasts, "\n")
		k := len(tLines)
		if k > len(outLines) {
			k = len(outLines)
		}
		for i := 0; i < k; i++ {
			outLines[len(outLines)-k+i] = tLines[len(tLines)-k+i]
		}
		out = strings.Join(outLines, "\n")
	}
	return out
}

func (m tuiModel) miniView() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render("SYNERGY ") +
		lipgloss.NewStyle().Foreground(clMuted).Render(shortModel(m.model)))
	b.WriteString("\n" + strings.Repeat("─", m.width) + "\n")
	s := m.sessions[m.cur]
	s.mu.Lock()
	ls := make([]tline, len(s.lines))
	copy(ls, s.lines)
	s.mu.Unlock()
	start := 0
	if len(ls) > 8 {
		start = len(ls) - 8
	}
	for _, l := range ls[start:] {
		b.WriteString(trunc(renderLine(l), m.width) + "\n")
	}
	b.WriteString(m.input.View())
	return b.String()
}

// ---------- верхний бар: кнопки по углам, без ручной арифметики ----------
func (m tuiModel) renderTop() string {
	// настоящие скруглённые кнопки-плитки: RoundedBorder — глифы ╭╮╰╯ рисует lipgloss
	tile := func(label string, active bool) string {
		st := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), false, true, true, true). // ╮ ╯ ╰ (левый край сливается с текстом)
			BorderForeground(clBorder).
			Foreground(clFG).
			Padding(0, 1)
		if active {
			st = st.BorderForeground(clAccent).Bold(true).Foreground(clAccent)
		}
		return st.Render(label)
	}

	side := tile("☰", m.sidebar)
	brand := lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render(" SYNERGY ") +
		lipgloss.NewStyle().Foreground(clMuted).Render(providerLabel(m.provider))
	newBtn := tile("＋", false)
	modelsBtn := tile(shortModel(m.model)+" ◎"+strings.Repeat("●", m.reasoning+1), false)
	cfgBtn := tile("⚙", m.settings)
	quitBtn := tile("✕", false)

	left := lipgloss.JoinHorizontal(lipgloss.Top, side, " ", brand, " ", newBtn)
	right := lipgloss.JoinHorizontal(lipgloss.Top, modelsBtn, " ", cfgBtn, " ", quitBtn)

	// join вместо пробельной арифметики: кнопки не съезжают ни при какой ширине
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Repeat(" ", gap), right)

	// зоны мыши — y=1, потому что строка 0 — отступ от верхнего края
	z := m.bz
	x := 0
	z.set("btn_side", x, 1, x+lipgloss.Width(side)-1, 1)
	x += lipgloss.Width(side) + 1 + lipgloss.Width(brand) + 1
	z.set("btn_new", x, 1, x+lipgloss.Width(newBtn)-1, 1)
	rx := m.width - lipgloss.Width(right)
	z.set("btn_models", rx, 1, rx+lipgloss.Width(modelsBtn)-1, 1)
	x2 := rx + lipgloss.Width(modelsBtn) + 1
	z.set("btn_cfg", x2, 1, x2+lipgloss.Width(cfgBtn)-1, 1)
	x2 += lipgloss.Width(cfgBtn) + 1
	z.set("btn_quit", x2, 1, x2+lipgloss.Width(quitBtn)-1, 1)

	// отступ от верхнего края: пустая строка, потом кнопки, потом разделитель
	return "\n" + row + "\n" + lipgloss.NewStyle().Foreground(clBorder).Render(strings.Repeat("─", m.width))
}

// ---------- тело ----------
func (m tuiModel) renderBody() string {
	if m.settings {
		return m.renderSettings()
	}
	if m.picker {
		return m.renderPicker()
	}
	mainCol := m.renderMainCol()
	if m.sidebarVisible() {
		side := m.renderSidebar()
		if m.sidebarOverlay() {
			// overlay: панель поверх контента — заменяем первые/последние sw колонок
			mainLines := strings.Split(mainCol, "\n")
			sideLines := strings.Split(side, "\n")
			sw := m.sidebarWidth()
			for i, sl := range sideLines {
				if i >= len(mainLines) {
					break
				}
				ml := mainLines[i]
				if m.sidebarRight {
					mainLines[i] = truncVisible(ml, m.width-2-sw) + sl
				} else {
					mainLines[i] = sl + dropVisibleLeft(ml, sw)
				}
			}
			return strings.Join(mainLines, "\n")
		}
		if m.sidebarRight {
			return lipgloss.JoinHorizontal(lipgloss.Top, mainCol, side)
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, side, mainCol)
	}
	return mainCol
}

func (m tuiModel) renderMainCol() string {
	s := m.sessions[m.cur]
	s.mu.Lock()
	empty := !s.running && (len(s.lines) == 0 || (len(s.lines) == 1 && s.lines[0].kind == "info"))
	s.mu.Unlock()

	if empty {
		greet := m.renderGreeting()
		boxW := m.inputWidthFor(true)
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(clAccent).
			Padding(1, 2).
			Width(boxW).
			Render(m.input.View())
		parts := []string{greet,
			lipgloss.NewStyle().Width(m.mainColWidth()).Align(lipgloss.Center).Render(box)}
		if m.compl.visible && len(m.compl.items) > 0 {
			parts = append(parts, lipgloss.NewStyle().Width(m.mainColWidth()).Align(lipgloss.Center).
				Render(m.renderCompletionPanel(boxW)))
		}
		parts = append(parts, "",
			lipgloss.NewStyle().Width(m.mainColWidth()).Align(lipgloss.Center).Render(m.renderInputChip()))
		return lipgloss.JoinVertical(lipgloss.Top, parts...)
	}

	area := m.vp.View()
	// прибиваем инпут к нижней строке: область транскрипта всегда ровно vp.Height —
	// пустое место остаётся между текстом и инпутом, инпут никогда не «всплывает»
	if m.vp.Height > 0 {
		n := 0
		if area != "" {
			n = strings.Count(area, "\n") + 1
		}
		if pad := m.vp.Height - n; pad > 0 {
			area += strings.Repeat("\n", pad)
		}
	}
	// фиксированная ширина: без Width рамка росла вместе с вводимым текстом
	inputBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clBorder).
		Padding(0, 1).
		Width(m.mainColWidth() - 4).
		MaxWidth(m.mainColWidth() - 4).
		Render(m.input.View())
	// под инпутом — одна компактная строка статуса (макет Frame 4: чистый текст + инпут внизу)
	status := lipgloss.NewStyle().Foreground(clMuted).Render(
		"  " + providerLabel(m.provider) + " | " + shortModel(m.model) + " | " + m.enterModeName())
	inputArea := lipgloss.JoinVertical(lipgloss.Top, inputBox, status)
	if m.compl.visible && len(m.compl.items) > 0 {
		inputArea = lipgloss.JoinVertical(lipgloss.Top, m.renderCompletionPanel(0), inputArea)
	}
	if todos := m.renderTodos(); todos != "" {
		inputArea = lipgloss.JoinVertical(lipgloss.Top, todos, inputArea)
	}
	return lipgloss.JoinVertical(lipgloss.Top, area, inputArea)
}

// ---------- приветствие: чистый текст + список чатов ----------
func (m tuiModel) renderGreeting() string {
	rows := []string{
		lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render("S Y N E R G Y"),
		"",
		lipgloss.NewStyle().Foreground(clFG).Render(T("greeting.sub")),
	}
	if agents := m.renderAgentsList(); agents != "" {
		rows = append(rows, "", agents)
	}
	if len(m.sessions) > 1 {
		var names []string
		for i, s := range m.sessions {
			mark := "○"
			if s.running {
				mark = "◐"
			}
			if i == m.cur {
				mark = "●"
			}
			names = append(names, mark+" "+trunc(s.name, 22))
		}
		rows = append(rows, "", lipgloss.NewStyle().Foreground(clMuted).Render(strings.Join(names, "   ")))
	}
	rows = append(rows, "",
		lipgloss.NewStyle().Foreground(clMuted).Render(T("greeting.hint1")),
		lipgloss.NewStyle().Foreground(clMuted).Render(T("greeting.hint2")),
	)
	inner := lipgloss.NewStyle().Width(m.mainColWidth() - 4).Align(lipgloss.Center).
		Render(strings.Join(rows, "\n"))
	// высота приветствия = экран − шапка − бокс инпута(строки+рамка 2+паддинг 2) − пустая строка − чип
	h := m.height - topBarRows - m.input.heightInLines() - 6
	if h < 5 {
		h = 5
	}
	return lipgloss.NewStyle().
		Width(m.mainColWidth()).
		Height(h).
		Align(lipgloss.Center, lipgloss.Center).
		Render(inner)
}

func (m tuiModel) renderAgentsList() string {
	type agentRow struct{ name, status string }
	var running, done []agentRow
	for _, s := range m.sessions {
		s.mu.Lock()
		hasWork := false
		for _, l := range s.lines {
			if l.kind == "user" {
				hasWork = true
				break
			}
		}
		r := agentRow{name: s.name}
		if s.title != "" {
			r.name = s.title
		}
		s.mu.Unlock()
		if s.running {
			r.status = T("agents.running")
			running = append(running, r)
		} else if hasWork {
			r.status = T("agents.done")
			done = append(done, r)
		}
	}
	if len(running)+len(done) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(clBlue).Render(T("agents.title")))
	for _, r := range running {
		b.WriteString("\n  " + lipgloss.NewStyle().Foreground(clOrange).Render("◐ ") +
			lipgloss.NewStyle().Foreground(clFG).Render(trunc(r.name, 34)) + " " +
			lipgloss.NewStyle().Foreground(clOrange).Render(r.status))
	}
	for i, r := range done {
		if i >= 6 {
			b.WriteString("\n  " + lipgloss.NewStyle().Foreground(clMuted).Render(fmt.Sprintf("+%d ещё", len(done)-6)))
			break
		}
		b.WriteString("\n  " + lipgloss.NewStyle().Foreground(clGreen).Render("● ") +
			lipgloss.NewStyle().Foreground(clMuted).Render(trunc(r.name, 34)) + " " +
			lipgloss.NewStyle().Foreground(clMuted).Render(r.status))
	}
	return b.String()
}

// ---------- статус-бар, чип, todo ----------
func (m tuiModel) renderStatusBar() string {
	s := m.sessions[m.cur]
	chars, pct := s.agent.ContextStatus()
	ctxCol := clGreen
	if pct >= 70 {
		ctxCol = clOrange
	}
	if pct >= 90 {
		ctxCol = clRed
	}
	seg := func(txt string, col lipgloss.TerminalColor) string {
		return lipgloss.NewStyle().Foreground(col).Render(txt)
	}
	parts := []string{
		seg(fmt.Sprintf(" %s %d%% (%s)", T("status.ctx"), pct, humanCount(chars)), ctxCol),
		seg(fmt.Sprintf("%s %d", T("status.turn"), s.agent.Turn), clCyan),
		seg(fmt.Sprintf("%s %d", T("status.tok"), s.agent.TotalTok), clMuted),
	}
	s.mu.Lock()
	qlen := len(s.queue)
	s.mu.Unlock()
	if qlen > 0 {
		parts = append(parts, seg(fmt.Sprintf("%s %d", T("status.queue"), qlen), clOrange))
	}
	if len(s.agent.pendingImages) > 0 {
		parts = append(parts, seg(fmt.Sprintf("📎 %d", len(s.agent.pendingImages)), clPurple))
	}
	if s.running {
		parts = append(parts, seg(T("status.running"), clGreen))
	}
	bar := strings.Join(parts, lipgloss.NewStyle().Foreground(clBorder).Render("  │  "))
	return lipgloss.NewStyle().Foreground(clMuted).Render("──── ") + bar
}

func (m tuiModel) renderInputChip() string {
	chip := fmt.Sprintf("%s | %s | %s",
		providerLabel(m.provider), shortModel(m.model),
		[]string{T("reason.low"), T("reason.mid"), T("reason.high")}[m.reasoning])
	return lipgloss.NewStyle().Foreground(clMuted).Render("    " + chip)
}

func (m tuiModel) renderTodos() string {
	if m.width < 120 || len(currentTodos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(clPurple).Render(T("plan")))
	shown := 0
	for _, t := range currentTodos {
		if shown >= 8 {
			b.WriteString(lipgloss.NewStyle().Foreground(clMuted).Render(fmt.Sprintf("  +%d ещё", len(currentTodos)-shown)))
			break
		}
		icon, colr := "○", clMuted
		switch t.Status {
		case "doing":
			icon, colr = "◐", clOrange
		case "done":
			icon, colr = "●", clGreen
		}
		b.WriteString(lipgloss.NewStyle().Foreground(colr).Render(fmt.Sprintf(" %s %s", icon, trunc(t.Text, 30))))
		shown++
	}
	return b.String()
}

// ---------- автодополнение (маркер ▸, без нумерации) ----------
func (m tuiModel) renderCompletionPanel(width int) string {
	var rows []string
	for i, c := range m.compl.items {
		if i > 7 {
			rows = append(rows, lipgloss.NewStyle().Foreground(clMuted).Render(fmt.Sprintf("  +%d ещё", len(m.compl.items)-8)))
			break
		}
		if i == m.compl.sel {
			rows = append(rows, lipgloss.NewStyle().Foreground(clBG).Background(clAccent).Render(" ▸ "+c+" "))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(clMuted).Render("   "+c))
		}
	}
	st := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clAccent).
		Padding(0, 1)
	if width > 0 {
		st = st.Width(width)
	}
	return st.Render(strings.Join(rows, "\n"))
}

// ---------- сайдбар: зоны мыши от РЕАЛЬНОЙ стороны (фикс «чаты не открываются») ----------
func (m tuiModel) renderSidebar() string {
	grouped := map[string][]int{}
	var dirs []string
	for i, s := range m.sessions {
		if _, ok := grouped[s.workdir]; !ok {
			dirs = append(dirs, s.workdir)
		}
		grouped[s.workdir] = append(grouped[s.workdir], i)
	}
	sortStrs(dirs)

	w := m.sidebarWidth()
	// X-зона панели зависит от стороны — раньше была захардкожена вправо, поэтому клики мимо
	var sx0, sx1 int
	if m.sidebarRight {
		sx0, sx1 = m.width-w-2, m.width-3
	} else {
		sx0, sx1 = 0, w-1
	}
	y := 2

	var b strings.Builder
	// тело начинается с y=topBarRows (отступ + кнопки + разделитель), +1 за рамку панели
	y = topBarRows + 1
	pinCol := clYellow

	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(clBlue).Render(T("chats") + "\n"))
	y++

	renderItem := func(i int) {
		s := m.sessions[i]
		mark := "  "
		if i == m.cur {
			mark = "▶"
		}
		pin := " "
		if s.pinned {
			pin = lipgloss.NewStyle().Foreground(pinCol).Render("◆")
		}
		status := ""
		if s.running {
			status = " …"
		}
		label := fmt.Sprintf("%s %s%s%s", mark, s.name, status, " "+pin)
		var item string
		if i == m.cur {
			item = lipgloss.NewStyle().Padding(0, 1).Foreground(clBG).Background(clAccent).Render(label)
		} else {
			item = lipgloss.NewStyle().Padding(0, 1).Foreground(clFG).Render(label)
		}
		m.bz.set(fmt.Sprintf("sess:%d", i), sx0, y, sx1, y)
		b.WriteString(item + "\n")
		y++
	}

	// закреплённые — отдельным блоком сверху
	var pins []int
	for i, s := range m.sessions {
		if s.pinned {
			pins = append(pins, i)
		}
	}
	if len(pins) > 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(pinCol).Render("  "+T("chats.pinned")) + "\n")
		y++
		for _, i := range pins {
			renderItem(i)
		}
		b.WriteString("\n")
		y++
	}

	for _, d := range dirs {
		short := d
		if short == "" {
			short = "."
		}
		b.WriteString(lipgloss.NewStyle().Foreground(clMuted).Render("  📁 "+trunc(filepath.Base(short), 24)) + "\n")
		y++
		for _, i := range grouped[d] {
			if m.sessions[i].pinned {
				continue // закреплённые уже отрисованы сверху
			}
			renderItem(i)
		}
	}

	// сохранённые на диске чаты, которых нет среди живых сессий
	liveNames := map[string]bool{}
	for _, s := range m.sessions {
		liveNames[s.name] = true
	}
	var saved []string
	for _, n := range listSavedSessions() {
		if !liveNames[n] {
			saved = append(saved, n)
		}
	}
	if len(saved) > 0 {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(clMuted).Render("  "+T("chats.saved")) + "\n")
		y += 2
		for i, n := range saved {
			if i >= 6 {
				b.WriteString(lipgloss.NewStyle().Foreground(clMuted).Render(fmt.Sprintf("    +%d", len(saved)-6)) + "\n")
				y++
				break
			}
			item := lipgloss.NewStyle().Padding(0, 1).Foreground(clMuted).Render("◌ " + trunc(n, 24))
			m.bz.set("saved:"+n, sx0, y, sx1, y)
			b.WriteString(item + "\n")
			y++
		}
	}

	// кнопки действий: создать / удалить / закрепить (по текущему чату)
	b.WriteString("\n")
	y++
	btnNew := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clGreen).Foreground(clGreen).Padding(0, 1).Render(T("chats.new"))
	btnDel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clRed).Foreground(clRed).Padding(0, 1).Render(T("chats.del"))
	btnPin := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(pinCol).Foreground(pinCol).Padding(0, 1).Render(T("chats.pin"))
	bx := sx0 + 1
	m.bz.set("chat_new", bx, y, bx+lipgloss.Width(btnNew)-1, y+2)
	bx += lipgloss.Width(btnNew) + 1
	m.bz.set("chat_del", bx, y, bx+lipgloss.Width(btnDel)-1, y+2)
	bx += lipgloss.Width(btnDel) + 1
	m.bz.set("chat_pin", bx, y, bx+lipgloss.Width(btnPin)-1, y+2)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, btnNew, " ", btnDel, " ", btnPin) + "\n")

	b.WriteString("\n" + lipgloss.NewStyle().Foreground(clMuted).Render(T("chats.hint")+"\n"))
	return lipgloss.NewStyle().
		Width(w).
		MaxWidth(w).
		Height(m.height - topBarRows).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clBorder).
		Render(b.String())
}

// ---------- пикер (модели/провайдеры/чаты/история) ----------
func (m tuiModel) renderPicker() string {
	var lines []string
	lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(clBlue).Render("  "+m.pickerTitle))
	lines = append(lines, "")
	start := 0
	maxShow := m.height - topBarRows - 8
	if maxShow < 3 {
		maxShow = 3
	}
	if len(m.pickerItems) > maxShow {
		start = m.pickerSel - maxShow/2
		if start < 0 {
			start = 0
		}
		if start+maxShow > len(m.pickerItems) {
			start = len(m.pickerItems) - maxShow
		}
	}
	end := start + maxShow
	if end > len(m.pickerItems) {
		end = len(m.pickerItems)
	}
	for idx := start; idx < end; idx++ {
		it := m.pickerItems[idx]
		if idx == m.pickerSel {
			lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(clBG).Background(clAccent).Padding(0, 1).Render("▸ "+trunc(it, m.width-14)))
		} else {
			lines = append(lines, lipgloss.NewStyle().Foreground(clFG).Padding(0, 1).Render("  "+trunc(it, m.width-14)))
		}
	}
	lines = append(lines, "")
	lines = append(lines, lipgloss.NewStyle().Foreground(clMuted).Render("  "+T("picker.keys")))
	// зоны клика по строкам пикера: тело стартует с topBarRows; дальше центровка,
	// рамка(1) + паддинг(0) + заголовок(2 строки: title + пустая)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clCyan).
		Padding(0, 2).
		Render(strings.Join(lines, "\n"))
	availH := m.height - topBarRows - 1
	boxH := strings.Count(box, "\n") + 1
	if boxH >= availH {
		// не влезает — прижимаем к верху, центрирование бы обрезало заголовок
		m.pickY0 = topBarRows + 1 + 2
		for i := 0; i < end-start; i++ {
			m.bz.set(fmt.Sprintf("pick:%d", start+i), 0, m.pickY0+i, m.width-1, m.pickY0+i)
		}
		return lipgloss.NewStyle().
			Width(m.width-2).
			Align(lipgloss.Center, lipgloss.Top).
			Render(box)
	}
	boxY := topBarRows + (availH-boxH)/2
	m.pickY0 = boxY + 1 + 2
	for i := 0; i < end-start; i++ {
		m.bz.set(fmt.Sprintf("pick:%d", start+i), 0, m.pickY0+i, m.width-1, m.pickY0+i)
	}
	return lipgloss.NewStyle().
		Width(m.width-2).
		Height(availH).
		Align(lipgloss.Center, lipgloss.Center).
		Render(box)
}

// ---------- настройки ----------
func (m tuiModel) renderSettings() string {
	provs := Providers()
	var provParts []string
	for _, p := range provs {
		if p.Name == m.provider {
			provParts = append(provParts, lipgloss.NewStyle().Bold(true).Foreground(clCyan).Render("▸"+p.Name))
		} else {
			provParts = append(provParts, lipgloss.NewStyle().Foreground(clMuted).Render(p.Name))
		}
	}
	reason := []string{T("reason.low"), T("reason.mid"), T("reason.high")}
	var rParts []string
	for i, r := range reason {
		if i == m.reasoning {
			rParts = append(rParts, lipgloss.NewStyle().Bold(true).Foreground(clYellow).Render("◎"+r))
		} else {
			rParts = append(rParts, lipgloss.NewStyle().Foreground(clMuted).Render(r))
		}
	}

	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render(T("settings")),
		"",
		T("settings.provider") + strings.Join(provParts, "  "),
		T("settings.model") + lipgloss.NewStyle().Foreground(clCyan).Render(shortModel(m.model)) + lipgloss.NewStyle().Foreground(clMuted).Render(T("settings.model.hint")),
		T("settings.reasoning") + strings.Join(rParts, "  "),
		"",
		T("settings.key") + lipgloss.NewStyle().Foreground(clCyan).Render(maskKey(m.providerKeyStatus())) + lipgloss.NewStyle().Foreground(clMuted).Render("  (K)"),
		T("settings.lang") + lipgloss.NewStyle().Foreground(clCyan).Render(uiLang) + lipgloss.NewStyle().Foreground(clMuted).Render("  (L)"),
		T("settings.failover") + lipgloss.NewStyle().Foreground(boolColor(m.failoverOn)).Render(onOff(m.failoverOn)) + lipgloss.NewStyle().Foreground(clMuted).Render(" → "+orDefault(m.failoverTarget, "auto")+"  (F)"),
		T("settings.sidebar") + lipgloss.NewStyle().Foreground(clCyan).Render(m.sidebarSideName()) + lipgloss.NewStyle().Foreground(clMuted).Render("  (B)"),
		T("settings.enter") + lipgloss.NewStyle().Foreground(clCyan).Render(m.enterModeName()) + lipgloss.NewStyle().Foreground(clMuted).Render("  (E)"),
		T("settings.titlemodel") + lipgloss.NewStyle().Foreground(clCyan).Render(m.titleModelLabel()) + lipgloss.NewStyle().Foreground(clMuted).Render("  (/titlemodel)"),
		"",
		T("settings.notify") + lipgloss.NewStyle().Foreground(clMuted).Render(notifyInfo()),
		T("settings.workdir") + lipgloss.NewStyle().Foreground(clMuted).Render(m.sessions[m.cur].workdir),
		"",
		lipgloss.NewStyle().Foreground(clMuted).Render(T("settings.keys")),
	}

	if m.provStep > 0 {
		prompt := []string{"", T("prov.name"), T("prov.url"), T("prov.key")}[m.provStep]
		lines = []string{
			lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render(fmt.Sprintf(T("prov.title"), m.provStep)),
			"",
			"  " + lipgloss.NewStyle().Foreground(clCyan).Render(m.provName) + " " + lipgloss.NewStyle().Foreground(clMuted).Render(prompt),
			"",
			lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clAccent).Padding(0, 1).
				Render(lipgloss.NewStyle().Foreground(clFG).Render(m.provBuf + "▌")),
			"",
			lipgloss.NewStyle().Foreground(clMuted).Render("  " + T("prov.keys")),
		}
	} else if m.keyEdit {
		stars := strings.Repeat("•", len([]rune(m.keyBuf)))
		lines = []string{
			lipgloss.NewStyle().Bold(true).Foreground(clAccent).Render(T("settings.key.edit")),
			"",
			"  " + lipgloss.NewStyle().Foreground(clCyan).Render(m.provider),
			"",
			lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clAccent).Padding(0, 1).
				Render(lipgloss.NewStyle().Foreground(clFG).Render(stars + "▌")),
			"",
			lipgloss.NewStyle().Foreground(clMuted).Render(T("settings.key.hint")),
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(clAccent).
		Padding(1, 3).
		Render(strings.Join(lines, "\n"))
	availH := m.height - topBarRows - 1
	if strings.Count(box, "\n")+1 >= availH {
		// высокий бокс — прижимаем к верху, иначе центрирование обрежет шапку
		return lipgloss.NewStyle().
			Width(m.width-2).
			Align(lipgloss.Center, lipgloss.Top).
			Render(box)
	}
	return lipgloss.NewStyle().
		Width(m.width-2).
		Height(availH).
		Align(lipgloss.Center, lipgloss.Center).
		Render(box)
}

// titleModelLabel — какая модель генерит заголовки чатов (по умолчанию текущая).
func (m tuiModel) titleModelLabel() string {
	if strings.TrimSpace(m.cfg.TitleModel) != "" {
		return m.cfg.TitleModel
	}
	return T("titlemodel.current")
}

func (m tuiModel) enterModeName() string {
	if m.ctrlEnterNewline {
		return T("enter.newline")
	}
	return T("enter.send")
}

func boolColor(b bool) lipgloss.TerminalColor {
	if b {
		return clGreen
	}
	return clRed
}

// ---------- тосты ----------
func (m tuiModel) renderToasts() string {
	if len(m.toasts) == 0 {
		return ""
	}
	var parts []string
	for _, t := range m.toasts {
		icon, colr := "ℹ", clCyan
		switch t.kind {
		case "ok":
			icon, colr = "✓", clGreen
		case "err":
			icon, colr = "✗", clRed
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(colr).Render(icon+" "+trunc(t.msg, m.width-8)))
	}
	return strings.Join(parts, "   ")
}

// ---------- транскрипт ----------
func renderLine(l tline) string {
	switch l.kind {
	case "user":
		return lipgloss.NewStyle().Bold(true).Foreground(clBlue).Render("› ") + l.text
	case "think":
		return lipgloss.NewStyle().Faint(true).Foreground(clAccent).Render("💭 " + l.text)
	case "tool":
		return lipgloss.NewStyle().Foreground(clCyan).Render(l.text)
	case "result":
		lines := strings.SplitN(l.text, "\n", 2)
		out := lipgloss.NewStyle().Foreground(clGreen).Render(lines[0])
		if len(lines) > 1 {
			out += "\n" + lipgloss.NewStyle().Foreground(clMuted).Render(lines[1])
		}
		return out
	case "info":
		return lipgloss.NewStyle().Foreground(clMuted).Render(l.text)
	case "err":
		return lipgloss.NewStyle().Foreground(clRed).Render(l.text)
	case "answer":
		return renderMarkdownLite(l.text)
	default:
		return lipgloss.NewStyle().Foreground(clFG).Render(l.text)
	}
}

func (m *tuiModel) refreshTranscript() {
	if len(m.sessions) == 0 {
		return
	}
	s := m.sessions[m.cur]
	s.mu.Lock()
	lines := make([]tline, len(s.lines))
	copy(lines, s.lines)
	s.mu.Unlock()
	if len(lines) > 600 {
		lines = lines[len(lines)-600:]
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(renderLine(l))
		b.WriteByte('\n')
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	m.vp.GotoBottom()
}

// ---------- helpers ----------
func shortModel(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 22 {
		s = s[:22] + "…"
	}
	return s
}

func humanCount(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func maskKey(k string) string {
	if k == "" {
		return T("key.none")
	}
	r := []rune(k)
	if len(r) <= 8 {
		return strings.Repeat("•", len(r))
	}
	return string(r[:3]) + "…" + string(r[len(r)-3:])
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sortStrs(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// ---------- точка входа ----------
func runBubbleTUI(cfg Config, llm *LLMClient, workdir string, plugins []*Plugin) int {
	m := newTuiModel(cfg, llm, workdir, plugins)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	globalProgram = p
	m.prog = p
	go StartCronLoop(func(e CronEntry) {
		p.Send(cronFireMsg{entry: e})
	})
	if _, err := p.Run(); err != nil {
		fmt.Println("TUI error:", err)
		return 1
	}
	return 0
}

// Init — стартовые команды: фетч моделей + тикер тостов.
func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		m.fetchCmd(),
		tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }),
	)
}

// truncVisible обрезает строку с ANSI-кодами до n видимых колонок.
func truncVisible(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	vis := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if vis >= n {
			break
		}
		b.WriteRune(r)
		vis++
	}
	if inEsc {
		return b.String()
	}
	return b.String() + "\x1b[0m"
}

// dropVisibleLeft отбрасывает первые n видимых колонок ANSI-строки.
func dropVisibleLeft(s string, n int) string {
	if n <= 0 {
		return s
	}
	var b strings.Builder
	vis := 0
	inEsc := false
	dropped := false
	for _, r := range s {
		if inEsc {
			if dropped {
				b.WriteRune(r)
			}
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			if dropped {
				b.WriteRune(r)
			}
			continue
		}
		if !dropped {
			vis++
			if vis > n {
				dropped = true
				b.WriteString("\x1b[0m")
				b.WriteRune(r)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
