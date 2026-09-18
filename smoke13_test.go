package main

import (
	"strings"
	"testing"
)

// overlay-слияние при разной длине строк mainCol/sideLines
func TestOverlayMerge(t *testing.T) {
	// короткий контент + длинная панель — раньше могли паниковать на индексах
	mainLines := []string{"abc", "def"}
	sideLines := []string{"S1", "S2", "S3", "S4"}
	for i, sl := range sideLines {
		if i >= len(mainLines) {
			break
		}
		mainLines[i] = sl + dropVisibleLeft(mainLines[i], 1)
	}
	out := strings.Join(mainLines, "\n")
	if !strings.Contains(out, "S1") || !strings.Contains(out, "S2") {
		t.Fatal("overlay merge:", out)
	}

	// ANSI-строка: truncVisible не должна резать escape-последовательность
	ansi := "\x1b[31mhello\x1b[0m world"
	tr := truncVisible(ansi, 5)
	if strings.Contains(tr, " world") {
		t.Fatal("truncVisible обрезала не так:", tr)
	}
	if !strings.HasSuffix(tr, "\x1b[0m") {
		t.Fatal("truncVisible не закрыла reset")
	}
}
