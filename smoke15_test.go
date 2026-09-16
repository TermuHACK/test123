package main

import (
	"fmt"
	"strings"
	"testing"
)

// Сколько строк реально занимает шапка vs topBarRows
func TestTopBarHeight(t *testing.T) {
	m := &tuiModel{width: 120, height: 40, bz: newZoneMgr(), provider: "OpenCode Zen no-key", model: "big-pickle"}
	top := m.renderTop()
	n := strings.Count(top, "\n") + 1
	fmt.Printf("renderTop lines = %d, topBarRows = %d\n", n, topBarRows)
	if n != topBarRows {
		t.Fatalf("РАСХОЖДЕНИЕ: шапка %d строк, topBarRows=%d — все зоны кликов съеханы на %d", n, topBarRows, n-topBarRows)
	}
}
