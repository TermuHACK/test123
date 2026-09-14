package main

// providers.go — полный rewrite слоя провайдеров (2026, по мотивам anomalyco/opencode).
//
// Чем отличается от старого:
//  1. Пробинг моделей: для бесплатных провайдеров каждая модель проверяется живым
//     запросом (max_tokens=1) и помечается alive/dead — мёртвые («Model is unavailable»,
//     таймауты) исключаются из списка, а не падают на пользователя посреди диалога.
//  2. Ретраи бесконечные (как у opencode): читаем Retry-After / retry-after-ms из
//     ответа, иначе экспоненциальный бэкофф 2s·2^n с джиттером 25%, потолок 5 минут.
//     Прерывание — только через контекст (Esc/Ctrl+C пользователя).
//  3. Ротация free-моделей: при 429 на Zen пробуем следующую живую бесплатную модель,
//     затем — фейловер на другого провайдера (настраивается).
//  4. Всё с событиями наружу: TUI показывает «повтор N, ждём Xs» в статус-баре.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- провайдеры ----------

// ProviderDef — описание провайдера (OpenAI-совместимого API).
type ProviderDef struct {
	Name    string                // отображаемое имя
	Base    string                // base URL (без trailing /)
	Free    bool                  // фильтровать бесплатные модели
	KeyFrom func(c Config) string // откуда брать ключ (env → per-provider → global)
	Custom  bool                  // добавлен пользователем
	NoProbe bool                  // не пробить модели (локальные/медленные)
}

var providersMu sync.RWMutex

// builtinProviders — встроенный список; кастомные добавляются из конфига.
var builtinProviders = []ProviderDef{
	{Name: "OpenCode Zen Free", Base: "https://opencode.ai/zen/v1", Free: true},
	{Name: "OpenCode Zen", Base: "https://opencode.ai/zen/v1", KeyFrom: func(c Config) string { return providerKey(c, "OpenCode Zen") }},
	{Name: "OpenRouter Free", Base: "https://openrouter.ai/api/v1", Free: true, KeyFrom: func(c Config) string { return providerKey(c, "OpenRouter") }},
	{Name: "OpenRouter", Base: "https://openrouter.ai/api/v1", KeyFrom: func(c Config) string { return providerKey(c, "OpenRouter") }},
}

func providerKey(c Config, name string) string {
	if c.Providers != nil {
		if pc, ok := c.Providers[name]; ok && pc != nil && pc.APIKey != "" {
			return pc.APIKey
		}
	}
	if name == "OpenRouter" {
		if k := getenv("OPENROUTER_API_KEY", ""); k != "" {
			return k
		}
	}
	return c.APIKey
}

// providerDefs — живой реестр (встроенные + кастомные из конфига).
var providerDefsV2 = builtinProviders

// Providers возвращает копию реестра.
func Providers() []ProviderDef {
	providersMu.RLock()
	defer providersMu.RUnlock()
	out := make([]ProviderDef, len(providerDefsV2))
	copy(out, providerDefsV2)
	return out
}

// ProviderByName находит провайдера по имени (или nil).
func ProviderByName(name string) *ProviderDef {
	providersMu.RLock()
	defer providersMu.RUnlock()
	for i := range providerDefsV2 {
		if strings.EqualFold(providerDefsV2[i].Name, name) {
			return &providerDefsV2[i]
		}
	}
	return nil
}

// AddCustomProvider регистрирует OpenAI-совместимого провайдера.
func AddCustomProvider(name, baseURL string) {
	providersMu.Lock()
	defer providersMu.Unlock()
	for _, p := range providerDefsV2 {
		if strings.EqualFold(p.Name, name) {
			return
		}
	}
	nm := name
	providerDefsV2 = append(providerDefsV2, ProviderDef{
		Name: nm, Base: strings.TrimRight(baseURL, "/"), Custom: true,
		KeyFrom: func(c Config) string { return providerKey(c, nm) },
	})
}

// RestoreCustomProviders поднимает кастомных провайдеров из конфига.
func RestoreCustomProviders(cfg Config) {
	for name, pc := range cfg.Providers {
		if pc == nil || pc.BaseURL == "" {
			continue
		}
		if ProviderByName(name) == nil {
			AddCustomProvider(name, pc.BaseURL)
		}
	}
}

// ---------- модели: фетч + пробинг ----------

// ModelInfo — модель с результатом пробинга.
type ModelInfo struct {
	ID    string
	Alive bool   // пробинг прошёл (для free; у остальных всегда true)
	Err   string // причина смерти («unavailable», «timeout»)
}

var (
	modelsCacheMu sync.Mutex
	modelsCache   = map[string][]ModelInfo{}
)

// FetchModels тянет список моделей провайдера; для free — пробит каждую параллельно.
func FetchModels(p *ProviderDef, cfg Config, probe bool) ([]ModelInfo, error) {
	key := p.Name
	modelsCacheMu.Lock()
	if cached, ok := modelsCache[key]; ok {
		modelsCacheMu.Unlock()
		return cached, nil
	}
	modelsCacheMu.Unlock()

	ids, err := fetchModelIDs(p, cfg)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(ids))
	if !p.Free || !probe {
		for _, id := range ids {
			out = append(out, ModelInfo{ID: id, Alive: true})
		}
	} else {
		// Пробинг бесплатных моделей параллельно (как models.dev статус у opencode,
		// только живой): tiny-запрос max_tokens=1, таймаут 20с.
		var wg sync.WaitGroup
		res := make([]ModelInfo, len(ids))
		sem := make(chan struct{}, 6)
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res[i] = probeModel(p, cfg, id)
			}(i, id)
		}
		wg.Wait()
		for _, mi := range res {
			out = append(out, mi)
		}
		// живые вперёд
		sort.SliceStable(out, func(i, j int) bool { return out[i].Alive && !out[j].Alive })
	}
	modelsCacheMu.Lock()
	modelsCache[key] = out
	modelsCacheMu.Unlock()
	return out, nil
}

// InvalidateModelsCache сбрасывает кэш (кнопка refresh).
func InvalidateModelsCache() {
	modelsCacheMu.Lock()
	modelsCache = map[string][]ModelInfo{}
	modelsCacheMu.Unlock()
}

func fetchModelIDs(p *ProviderDef, cfg Config) ([]string, error) {
	base := strings.TrimRight(p.Base, "/")
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.KeyFrom != nil {
		if k := p.KeyFrom(cfg); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	applyZenHeaders(req, "")
	req.Header.Set("User-Agent", "synergy/3.0")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := sharedHTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s/models → HTTP %d", base, resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, d := range out.Data {
		if d.ID == "" || seen[d.ID] {
			continue
		}
		if p.Free {
			low := strings.ToLower(d.ID)
			if strings.Contains(base, "openrouter") {
				if !strings.HasSuffix(low, ":free") {
					continue
				}
			} else if !strings.Contains(low, "free") {
				continue
			}
		}
		seen[d.ID] = true
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("провайдер %s вернул пустой список", p.Name)
	}
	sort.Strings(ids)
	return ids, nil
}

// probeModel — один живой запрос к модели: alive при HTTP 200 (даже пустой ответ).
func probeModel(p *ProviderDef, cfg Config, modelID string) ModelInfo {
	body := []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`)
	req, err := http.NewRequest("POST", strings.TrimRight(p.Base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ModelInfo{ID: modelID, Err: err.Error()}
	}
	if p.KeyFrom != nil {
		if k := p.KeyFrom(cfg); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	applyZenHeaders(req, newUUID())
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := sharedHTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return ModelInfo{ID: modelID, Err: "timeout/net"}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == 200 {
		return ModelInfo{ID: modelID, Alive: true}
	}
	errMsg := trunc(string(data), 120)
	if strings.Contains(errMsg, "unavailable") {
		errMsg = "model unavailable"
	}
	return ModelInfo{ID: modelID, Err: fmt.Sprintf("HTTP %d", resp.StatusCode) + " " + errMsg}
}

// ---------- ретраи (по образцу opencode session/retry.ts) ----------

// RetryEvent — событие ретрая для UI (статус-бар/тост).
type RetryEvent struct {
	Attempt  int
	Wait     time.Duration
	Reason   string
	Rotated  string // новая модель при ротации (пусто = та же)
	Failover string // новый провайдер при фейловере
}

// retryDelay считает паузу: Retry-After заголовки → экспонента 2s·2^n + джиттер 25%, потолок 5 мин.
func retryDelay(attempt int, headers http.Header) time.Duration {
	if headers != nil {
		if v := headers.Get("retry-after-ms"); v != "" {
			if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
				return time.Duration(ms) * time.Millisecond
			}
		}
		if v := headers.Get("retry-after"); v != "" {
			if s, err := strconv.Atoi(v); err == nil && s > 0 {
				return time.Duration(s) * time.Second
			}
		}
	}
	d := 2 * time.Second << minInt2(attempt, 8) // 2s, 4s, 8s … потолок 512s
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	jitter := time.Duration(rand.Float64() * 0.25 * float64(d))
	return d - jitter/2
}

func minInt2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isRetryableStatus — статусы, на которые есть смысл повторять (429/5xx).
func isRetryableStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504, 524:
		return true
	}
	return false
}

// applyZenHeaders — служебные заголовки клиента opencode для Zen (без них — MissingSessionID).
// sessionID пустой → сгенерируется новый.
func applyZenHeaders(req *http.Request, sessionID string) {
	if !strings.Contains(req.URL.Host, "opencode.ai") {
		return
	}
	if sessionID == "" {
		sessionID = newUUID()
	}
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-request", sessionID)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "opencode/1.0")
	}
}

// AllProviders — снимок живого реестра провайдеров (встроенные + кастомные).
func AllProviders() []ProviderDef {
	providersMu.RLock()
	defer providersMu.RUnlock()
	out := make([]ProviderDef, len(providerDefsV2))
	copy(out, providerDefsV2)
	return out
}
