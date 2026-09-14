package main

// cron.go — триггеры/напоминания (cron-подобные записи) и система тасков
// с жёстким лимитом: не более 2 активных тасков на сессию.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CronEntry — запись триггера: одноразовая (Every == 0) или повторяющаяся.
type CronEntry struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Every   int       `json:"every_min"` // 0 = одноразово
	FireAt  time.Time `json:"fire_at"`
	Enabled bool      `json:"enabled"`
}

var (
	cronMu      sync.Mutex
	cronEntries []CronEntry
	cronLoaded  bool
)

func cronPath() string { return filepath.Join(getConfigDir(), "cron.json") }

func loadCron() {
	cronMu.Lock()
	defer cronMu.Unlock()
	if cronLoaded {
		return
	}
	cronLoaded = true
	data, err := os.ReadFile(cronPath())
	if err == nil {
		_ = json.Unmarshal(data, &cronEntries)
	}
}

func saveCron() {
	data, err := jsonMarshalIndent(cronEntries)
	if err == nil {
		_ = os.WriteFile(cronPath(), data, 0o600)
	}
}

// AddCron регистрирует триггер: через delayMin минут, optionally повторяя каждые everyMin.
func AddCron(text string, delayMin, everyMin int) CronEntry {
	loadCron()
	cronMu.Lock()
	defer cronMu.Unlock()
	e := CronEntry{
		ID:      fmt.Sprintf("c%d", time.Now().UnixNano()%1_000_000),
		Text:    text,
		Every:   everyMin,
		FireAt:  time.Now().Add(time.Duration(maxInt(delayMin, 1)) * time.Minute),
		Enabled: true,
	}
	cronEntries = append(cronEntries, e)
	saveCron()
	return e
}

// ListCron — снимок списка триггеров.
func ListCron() []CronEntry {
	loadCron()
	cronMu.Lock()
	defer cronMu.Unlock()
	out := make([]CronEntry, len(cronEntries))
	copy(out, cronEntries)
	return out
}

// RemoveCron удаляет триггер по id (true — удалён).
func RemoveCron(id string) bool {
	loadCron()
	cronMu.Lock()
	defer cronMu.Unlock()
	for i, e := range cronEntries {
		if e.ID == id {
			cronEntries = append(cronEntries[:i], cronEntries[i+1:]...)
			saveCron()
			return true
		}
	}
	return false
}

// StartCronLoop — фоновый цикл: раз в 10 сек проверяет, какие триггеры сработали,
// и вызывает onFire (в TUI это p.Send(cronFireMsg)).
func StartCronLoop(onFire func(CronEntry)) {
	loadCron()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		var fired []CronEntry
		cronMu.Lock()
		for i := range cronEntries {
			e := &cronEntries[i]
			if !e.Enabled || now.Before(e.FireAt) {
				continue
			}
			fired = append(fired, *e)
			if e.Every > 0 {
				e.FireAt = now.Add(time.Duration(e.Every) * time.Minute)
			} else {
				e.Enabled = false
			}
		}
		if len(fired) > 0 {
			saveCron()
		}
		cronMu.Unlock()
		for _, e := range fired {
			if p, err := LookPathSafe("termux-notification"); err == nil {
				_ = runRawDetached(p, "--title", "⏰ Synergy Harness", "--content", e.Text)
			}
			notifyApp("⏰ Напоминание", e.Text)
			onFire(e)
		}
	}
}

// ---------- система тасков (лимит: 2 активных на сессию) ----------

const maxActiveTasks = 2

// TaskItem — задача сессии: открывается агентом через task_open, закрывается task_close.
type TaskItem struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"` // open | done
	OpenedAt string `json:"opened_at"`
	ClosedAt string `json:"closed_at,omitempty"`
}

var (
	tasksMu      sync.Mutex
	sessionTasks []TaskItem
	nextTaskID   = 1
)

// OpenTask — создать таск. Возвращает ошибку, если уже 2 активных.
func OpenTask(title string) (TaskItem, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return TaskItem{}, fmt.Errorf("пустое название таска")
	}
	tasksMu.Lock()
	defer tasksMu.Unlock()
	active := 0
	for _, t := range sessionTasks {
		if t.Status == "open" {
			active++
		}
	}
	if active >= maxActiveTasks {
		return TaskItem{}, fmt.Errorf("лимит: не больше %d активных тасков на сессию — сначала закрой один через task_close", maxActiveTasks)
	}
	t := TaskItem{ID: nextTaskID, Title: title, Status: "open", OpenedAt: time.Now().Format("15:04")}
	nextTaskID++
	sessionTasks = append(sessionTasks, t)
	notifyApp("📌 Таск открыт", fmt.Sprintf("#%d %s", t.ID, title))
	return t, nil
}

// CloseTask — закрыть таск по id.
func CloseTask(id int) error {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	for i := range sessionTasks {
		if sessionTasks[i].ID == id && sessionTasks[i].Status == "open" {
			sessionTasks[i].Status = "done"
			sessionTasks[i].ClosedAt = time.Now().Format("15:04")
			notifyApp("✅ Таск закрыт", fmt.Sprintf("#%d %s", id, sessionTasks[i].Title))
			return nil
		}
	}
	return fmt.Errorf("таск #%d не найден или уже закрыт", id)
}

// ListTasks — снимок тасков текущей сессии.
func ListTasks() []TaskItem {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	out := make([]TaskItem, len(sessionTasks))
	copy(out, sessionTasks)
	return out
}

// ResetTasks — очистка тасков (при /reset чата).
func ResetTasks() {
	tasksMu.Lock()
	sessionTasks = nil
	nextTaskID = 1
	tasksMu.Unlock()
}

// formatTasks — компактный рендер списка тасков для статус-бара/ответа тула.
func formatTasks() string {
	ts := ListTasks()
	if len(ts) == 0 {
		return "тасков нет"
	}
	var b strings.Builder
	for _, t := range ts {
		mark := "○"
		if t.Status == "done" {
			mark = "●"
		}
		fmt.Fprintf(&b, "%s #%d %s (%s)\n", mark, t.ID, t.Title, t.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
