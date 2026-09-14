package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// mlEditor — собственный многострочный редактор для TUI (без bubbles/textarea).
// Enter = новая строка, Ctrl+Enter = отправка (обрабатывается выше по стеку).
// Корректно переносит длинные строки и многострочную вставку; горизонтальной
// прокрутки нет — строки визуально переносятся по ширине.
type mlEditor struct {
	lines       []string // логические строки
	cx, cy      int      // курсор: колонка (в рунах), строка
	width       int      // ширина отображения
	maxVisible  int      // сколько визуальных строк показывать (высота блока)
	scrollOff   int      // вертикальный оффсет в визуальных строках
	placeholder string
}

func newMlEditor() mlEditor {
	return mlEditor{
		lines:       []string{""},
		maxVisible:  6,
		placeholder: "Сообщение…  (Enter — новая строка, Ctrl+Enter — отправить, / — команды)",
	}
}

func (e *mlEditor) Value() string { return strings.Join(e.lines, "\n") }

func (e *mlEditor) Reset() {
	e.lines = []string{""}
	e.cx, e.cy, e.scrollOff = 0, 0, 0
}

func (e *mlEditor) curRunes() []rune { return []rune(e.lines[e.cy]) }

func (e *mlEditor) setCur(r []rune) { e.lines[e.cy] = string(r) }

// insertText вставляет текст (в т.ч. многострочную вставку из буфера — paste).
func (e *mlEditor) insertText(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	r := e.curRunes()
	head := string(r[:e.cx])
	tail := string(r[e.cx:])
	if len(parts) == 1 {
		e.setCur([]rune(head + parts[0] + tail))
		e.cx += len([]rune(parts[0]))
		return
	}
	newLines := make([]string, 0, len(e.lines)+len(parts)-1)
	newLines = append(newLines, e.lines[:e.cy]...)
	newLines = append(newLines, head+parts[0])
	for _, p := range parts[1 : len(parts)-1] {
		newLines = append(newLines, p)
	}
	newLines = append(newLines, parts[len(parts)-1]+tail)
	newLines = append(newLines, e.lines[e.cy+1:]...)
	e.lines = newLines
	e.cy += len(parts) - 1
	e.cx = len([]rune(parts[len(parts)-1]))
}

func (e *mlEditor) newline() {
	r := e.curRunes()
	head := string(r[:e.cx])
	tail := string(r[e.cx:])
	e.lines[e.cy] = head
	rest := append([]string{tail}, e.lines[e.cy+1:]...)
	e.lines = append(e.lines[:e.cy+1], rest...)
	e.cy++
	e.cx = 0
}

func (e *mlEditor) backspace() {
	if e.cx > 0 {
		r := e.curRunes()
		e.setCur(append(r[:e.cx-1], r[e.cx:]...))
		e.cx--
	} else if e.cy > 0 {
		prev := []rune(e.lines[e.cy-1])
		e.cx = len(prev)
		e.lines[e.cy-1] = string(prev) + e.lines[e.cy]
		e.lines = append(e.lines[:e.cy], e.lines[e.cy+1:]...)
		e.cy--
	}
}

func (e *mlEditor) deleteForward() {
	r := e.curRunes()
	if e.cx < len(r) {
		e.setCur(append(r[:e.cx], r[e.cx+1:]...))
	} else if e.cy < len(e.lines)-1 {
		e.lines[e.cy] = e.lines[e.cy] + e.lines[e.cy+1]
		e.lines = append(e.lines[:e.cy+1], e.lines[e.cy+2:]...)
	}
}

func (e *mlEditor) deleteWord() {
	r := e.curRunes()
	i := e.cx
	for i > 0 && r[i-1] == ' ' {
		i--
	}
	for i > 0 && r[i-1] != ' ' {
		i--
	}
	e.setCur(append(r[:i], r[e.cx:]...))
	e.cx = i
}

// moveVert — курсор вверх/вниз с учётом визуального переноса строк.
func (e *mlEditor) moveVert(dy int) {
	vis := e.visualLines()
	// текущая визуальная позиция
	vy, vx := 0, 0
	for i, vl := range vis {
		if vl.ly == e.cy && e.cx >= vl.start && (e.cx <= vl.end || i == len(vis)-1) {
			vy, vx = i, e.cx-vl.start
			break
		}
	}
	ny := vy + dy
	if ny < 0 {
		ny = 0
	}
	if ny >= len(vis) {
		ny = len(vis) - 1
	}
	vl := vis[ny]
	e.cy = vl.ly
	e.cx = vl.start + vx
	if e.cx > vl.end {
		e.cx = vl.end
	}
}

type visLine struct {
	ly         int // индекс логической строки
	start, end int // диапазон рун [start, end)
	first      bool
}

// visualLines раскладывает логические строки в визуальные с переносом по ширине.
func (e *mlEditor) visualLines() []visLine {
	w := e.width
	if w < 10 {
		w = 10
	}
	var out []visLine
	for ly, line := range e.lines {
		r := []rune(line)
		if len(r) == 0 {
			out = append(out, visLine{ly: ly, start: 0, end: 0, first: true})
			continue
		}
		for start := 0; start < len(r); start += w {
			end := start + w
			if end > len(r) {
				end = len(r)
			}
			out = append(out, visLine{ly: ly, start: start, end: end, first: start == 0})
		}
	}
	return out
}

// View рендерит редактор с вертикальной прокруткой и курсором.
func (e *mlEditor) View() string {
	vis := e.visualLines()
	// визуальная строка с курсором
	curVis := 0
	for i, vl := range vis {
		if vl.ly == e.cy && e.cx >= vl.start && e.cx <= vl.end {
			curVis = i
		}
	}
	if curVis < e.scrollOff {
		e.scrollOff = curVis
	}
	if curVis >= e.scrollOff+e.maxVisible {
		e.scrollOff = curVis - e.maxVisible + 1
	}

	promptStyle := lipgloss.NewStyle().Foreground(clGreen)
	contStyle := lipgloss.NewStyle().Foreground(clMuted)
	cursorStyle := lipgloss.NewStyle().Reverse(true)

	empty := len(e.lines) == 1 && e.lines[0] == ""
	var b strings.Builder
	if empty {
		b.WriteString(promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(clMuted).Render(e.placeholder))
		return b.String()
	}
	end := e.scrollOff + e.maxVisible
	if end > len(vis) {
		end = len(vis)
	}
	for i := e.scrollOff; i < end; i++ {
		vl := vis[i]
		if vl.first {
			b.WriteString(promptStyle.Render("❯ "))
		} else {
			b.WriteString(contStyle.Render("… "))
		}
		runes := []rune(e.lines[vl.ly])[vl.start:vl.end]
		if vl.ly == e.cy && e.cx >= vl.start && e.cx <= vl.end {
			pos := e.cx - vl.start
			before := string(runes[:pos])
			var cur, after string
			if pos < len(runes) {
				cur = string(runes[pos : pos+1])
				after = string(runes[pos+1:])
			} else {
				cur = " "
			}
			b.WriteString(before + cursorStyle.Render(cur) + after)
		} else {
			b.WriteString(string(runes))
		}
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// heightInLines — сколько визуальных строк занимает редактор (для layout).
func (e *mlEditor) heightInLines() int {
	n := len(e.visualLines())
	if n > e.maxVisible {
		return e.maxVisible
	}
	if n < 1 {
		return 1
	}
	return n
}
