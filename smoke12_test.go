package main

import (
	"strings"
	"sync"
	"testing"
)

// Воспроизводим паттерн из FetchModels: горутины пишут в res[i], потом читаем.
func TestProbePattern(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var wg sync.WaitGroup
	res := make([]string, len(ids))
	sem := make(chan struct{}, 6)
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res[i] = "alive:" + id
		}(i, id)
	}
	wg.Wait()
	for i, r := range res {
		if !strings.HasPrefix(r, "alive:") {
			t.Fatalf("res[%d] = %q", i, r)
		}
	}
}
