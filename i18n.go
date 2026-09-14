package main

// i18n — минимальная система локализации UI (RU/EN).
// Язык: config.lang или SYNERGY_LANG; переключение — /lang или в настройках (L).

var uiLang = "ru"

// T — перевод ключа на текущий язык. Неизвестный ключ возвращается как есть.
func T(key string) string {
	if m, ok := translations[uiLang]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	if s, ok := translations["ru"][key]; ok { // фолбэк на русский
		return s
	}
	return key
}

var translations = map[string]map[string]string{
	"ru": {
		"greeting.title":      "SYNERGY",
		"greeting.sub":        "автономный ИИ-агент: веб-поиск, тулы, MCP, сандбокс, плагины, параллельные чаты",
		"greeting.hint1":      "просто напиши, что сделать —",
		"greeting.hint2":      "или введи / для списка команд",
		"chats":               " чаты",
		"chats.hint":          "  /sessions, /chat, + новый",
		"settings":            "  Параметры",
		"settings.provider":   "  Провайдер:  ",
		"settings.model":      "  Модель:  ",
		"settings.model.hint": "   (Enter — список)",
		"settings.reasoning":  "  Размышления:  ",
		"settings.notify":     "  Уведомления:  ",
		"settings.workdir":    "  Рабочая папка:  ",
		"settings.lang":       "  Язык / Language:  ",
		"settings.failover":   "  Фейловер провайдера:  ",
		"settings.sidebar":    "  Панель чатов:  ",
		"settings.enter":      "  Enter на десктопе:  ",
		"settings.keys":       "  ←/→ — провайдер • R — размышления • L — язык • F — фейловер • B — панель • E — Enter • Enter — модели • Esc — закрыть",
		"on":                  "вкл",
		"off":                 "выкл",
		"left":                "слева",
		"right":               "справа",
		"enter.send":          "отправка",
		"enter.newline":       "новая строка (Ctrl+Enter — отправка)",
		"toast.lang":          "язык: ",
		"toast.failover":      "фейловер: ",
		"toast.sidebar":       "панель чатов: ",
		"status.ctx":          "конт",
		"status.turn":         "ход",
		"status.tok":          "ток",
		"status.queue":        "⌛ очередь",
		"status.running":      "● работает",
		"settings.key":        "  API-ключ провайдера:  ",
		"settings.key.edit":   "  Ввод API-ключа",
		"settings.key.hint":   "  вставь ключ и нажми Enter • Esc — отмена",
		"key.none":            "не задан",
		"agents.title":        "▸ агенты:",
		"agents.running":      "в процессе…",
		"agents.done":         "готово",
		"plan":                "▸ план: ",
	},
	"en": {
		"greeting.title":      "SYNERGY",
		"greeting.sub":        "autonomous AI agent: web search, tools, MCP, sandbox, plugins, parallel chats",
		"greeting.hint1":      "just type what to do —",
		"greeting.hint2":      "or enter / for command list",
		"chats":               " chats",
		"chats.hint":          "  /sessions, /chat, + new",
		"settings":            "  Settings",
		"settings.provider":   "  Provider:  ",
		"settings.model":      "  Model:  ",
		"settings.model.hint": "   (Enter — list)",
		"settings.reasoning":  "  Reasoning:  ",
		"settings.notify":     "  Notifications:  ",
		"settings.workdir":    "  Working dir:  ",
		"settings.lang":       "  Language / Язык:  ",
		"settings.failover":   "  Provider failover:  ",
		"settings.sidebar":    "  Chat panel:  ",
		"settings.enter":      "  Desktop Enter key:  ",
		"settings.keys":       "  ←/→ — provider • R — reasoning • L — language • F — failover • B — panel • E — Enter • Enter — models • Esc — close",
		"on":                  "on",
		"off":                 "off",
		"left":                "left",
		"right":               "right",
		"enter.send":          "send",
		"enter.newline":       "newline (Ctrl+Enter sends)",
		"toast.lang":          "language: ",
		"toast.failover":      "failover: ",
		"toast.sidebar":       "chat panel: ",
		"status.ctx":          "ctx",
		"status.turn":         "turn",
		"status.tok":          "tok",
		"status.queue":        "⌛ queue",
		"status.running":      "● running",
		"settings.key":        "  Provider API key:  ",
		"settings.key.edit":   "  Enter API key",
		"settings.key.hint":   "  paste key and press Enter • Esc — cancel",
		"key.none":            "not set",
		"agents.title":        "▸ agents:",
		"agents.running":      "running…",
		"agents.done":         "done",
		"plan":                "▸ plan: ",
	},
}

func setUILang(l string) {
	if l == "en" || l == "ru" {
		uiLang = l
	}
}
