package main

import (
	"fmt"
	"strings"
	"testing"
)

// Проверка сортировки моделей Zen: free первыми (big-pickle в топе), живые выше мёртвых
func TestZenSort(t *testing.T) {
	p := &ProviderDef{Name: "OpenCode Zen no-key", Base: "https://opencode.ai/zen/v1", Free: true}
	InvalidateModelsCache()
	infos, err := FetchModels(p, Config{}, false) // без пробинга — только порядок из /models
	if err != nil {
		t.Skip("нет сети:", err)
	}
	for i, mi := range infos[:min2(10, len(infos))] {
		fmt.Printf("  %2d. %s alive=%v\n", i, mi.ID, mi.Alive)
	}
	if len(infos) == 0 {
		t.Fatal("пустой список")
	}
	first := strings.ToLower(infos[0].ID)
	if !strings.Contains(first, "free") && first != "big-pickle" {
		t.Fatalf("первым должна быть free-модель, а не %s", infos[0].ID)
	}
	// big-pickle должен быть выше остальных free
	bp, other := -1, -1
	for i, mi := range infos {
		id := strings.ToLower(mi.ID)
		if id == "big-pickle" && bp < 0 {
			bp = i
		}
		if other < 0 && strings.Contains(id, "free") && id != "big-pickle" {
			other = i
		}
	}
	fmt.Printf("big-pickle idx=%d, первый другой free idx=%d\n", bp, other)
	if bp >= 0 && other >= 0 && bp > other {
		t.Fatalf("big-pickle (%d) должен быть выше других free (%d)", bp, other)
	}
}
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
