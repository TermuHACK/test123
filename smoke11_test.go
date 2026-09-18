package main

import (
	"strings"
	"testing"
)

func TestRound11(t *testing.T) {
	// 1) /model убран из комплита, /models на месте
	for _, c := range slashCommands {
		if c == "/model" {
			t.Fatal("/model найден в slashCommands — должен быть убран")
		}
	}
	found := false
	for _, c := range slashCommands {
		if c == "/models" {
			found = true
		}
	}
	if !found {
		t.Fatal("/models пропал из slashCommands")
	}

	// 2) сайдбар виден на 80 колонках (было: только от 96)
	m := &tuiModel{width: 80, height: 24, sidebar: true, sidebarAnim: 8}
	if !m.sidebarVisible() {
		t.Fatal("sidebar невидим на 80 колонках")
	}
	if !m.sidebarOverlay() {
		t.Fatal("на 80 колонках сайдбар должен быть overlay")
	}
	m.width = 120
	if m.sidebarOverlay() {
		t.Fatal("на 120 колонках overlay не нужен")
	}

	// 3) truncVisible / dropVisibleLeft не портят ANSI
	plain := truncVisible("abcdef", 3)
	if !strings.Contains(plain, "abc") || strings.Contains(plain, "def") {
		t.Fatal("truncVisible:", plain)
	}
	dropped := dropVisibleLeft("abcdef", 2)
	if !strings.Contains(dropped, "cdef") || strings.Contains(dropped, "ab") {
		t.Fatal("dropVisibleLeft:", dropped)
	}

	// 4) title-модель по умолчанию = текущая
	m.cfg = Config{}
	m.model = "mimo-v2.5-free"
	if m.titleModelLabel() == "" {
		t.Fatal("titleModelLabel пустой")
	}
	m.cfg.TitleModel = "big-pickle"
	if m.titleModelLabel() != "big-pickle" {
		t.Fatal("titleModelLabel должен вернуть big-pickle")
	}

	// 5) empty-логика: running-сессия не считается пустой
	m.sessions = []*chatSession{{running: true}}
	m.cur = 0
	s := m.sessions[0]
	empty := !s.running && len(s.lines) == 0
	if empty {
		t.Fatal("running-сессия помечена пустой — инпут уедет в центр")
	}
	t.Log("ALL OK")
}
