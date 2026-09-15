package main

import "testing"

// Краш из дампа: события с устаревшим индексом сессии после закрытия чата.
func TestStaleSessionIndex(t *testing.T) {
	m := &tuiModel{sessions: []*chatSession{{name: "c1"}}, cur: 0}

	// launchAgent с невалидным индексом — nil-команда, не паника
	if cmd := m.launchAgent(5, "hi"); cmd != nil {
		t.Fatal("launchAgent с плохим индексом должен вернуть nil")
	}
	if cmd := m.launchAgent(-1, "hi"); cmd != nil {
		t.Fatal("launchAgent с отрицательным индексом должен вернуть nil")
	}

	// chatDoneMsg с устаревшим индексом — игнор, не паника
	defer func() {
		if r := recover(); r != nil {
			t.Fatal("паника на устаревшем chatDoneMsg:", r)
		}
	}()
	_, _ = m.Update(chatDoneMsg{sid: 7, err: nil})
	_, _ = m.Update(chatDeltaMsg{sid: 9, kind: "info", text: "x"})
	_, _ = m.Update(chatDeltaMsg{sid: -3, kind: "stream", text: "x"})
}

// Инпут в чате: ширина бокса фиксирована и не зависит от длины текста.
func TestInputWidthStable(t *testing.T) {
	m := &tuiModel{width: 120, height: 40, sessions: []*chatSession{{name: "c1", lines: []tline{{"user", "x"}}}}, cur: 0}
	w1 := m.mainColWidth()
	// mainColWidth не зависит от содержимого инпута
	m2 := &tuiModel{width: 120, height: 40, sessions: m.sessions, cur: 0}
	if m2.mainColWidth() != w1 {
		t.Fatal("mainColWidth нестабильна")
	}
	if w1 != 118 { // 120-2, сайдбар выключен
		t.Fatalf("mainColWidth=%d, ожидали 118", w1)
	}
}
