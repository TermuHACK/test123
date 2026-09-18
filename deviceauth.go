package main

// deviceauth.go — OAuth Device Authorization (порт с codex: codex-rs/login/src/device_code_auth.rs,
// лицензия Apache-2.0, © OpenAI). Официальный легальный флоу логина ChatGPT: пользователь
// сам вводит код на auth.openai.com, мы получаем access/refresh/id токены.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // публичный client_id codex (из их исходников)
	codexIssuer   = "https://auth.openai.com"
)

// codexDeviceCode — ответ /api/accounts/deviceauth/usercode.
type codexDeviceCode struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     uint64 `json:"-"`
}

func (d *codexDeviceCode) UnmarshalJSON(b []byte) error {
	var raw struct {
		DeviceAuthID string      `json:"device_auth_id"`
		UserCode     string      `json:"user_code"`
		Usercode     string      `json:"usercode"`
		Interval     interface{} `json:"interval"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d.DeviceAuthID = raw.DeviceAuthID
	d.UserCode = raw.UserCode
	if d.UserCode == "" {
		d.UserCode = raw.Usercode
	}
	d.Interval = 5
	switch v := raw.Interval.(type) {
	case string:
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			d.Interval = n
		}
	case float64:
		d.Interval = uint64(v)
	}
	if d.Interval < 1 {
		d.Interval = 1
	}
	return nil
}

// codexTokens — результат обмена/рефреша.
type codexTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	SavedAt      int64  `json:"saved_at"`
}

func codexAuthPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "gomni", "codex-auth.json")
}

func codexLoadTokens() *codexTokens {
	b, err := os.ReadFile(codexAuthPath())
	if err != nil {
		return nil
	}
	var t codexTokens
	if json.Unmarshal(b, &t) != nil || t.AccessToken == "" {
		return nil
	}
	return &t
}

func codexSaveTokens(t *codexTokens) error {
	p := codexAuthPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	t.SavedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(t, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

func codexAPIBase() string { return strings.TrimRight(codexIssuer, "/") + "/api/accounts" }

// codexRequestUserCode — шаг 1: просим одноразовый код (как request_user_code в codex).
func codexRequestUserCode(ctx context.Context) (*codexDeviceCode, error) {
	body, _ := json.Marshal(map[string]string{"client_id": codexClientID})
	req, err := http.NewRequestWithContext(ctx, "POST", codexAPIBase()+"/deviceauth/usercode", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("device login выключен на сервере (404)")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("usercode: HTTP %d: %s", resp.StatusCode, truncAuth(string(b), 300))
	}
	var dc codexDeviceCode
	if err := json.Unmarshal(b, &dc); err != nil {
		return nil, fmt.Errorf("usercode: плохой JSON: %w", err)
	}
	return &dc, nil
}

// codexPollToken — шаг 2: ждём, пока юзер введёт код (403/404 = ещё не ввёл, спим interval; макс 15 мин).
func codexPollToken(ctx context.Context, dc *codexDeviceCode, onWait func()) (*codexTokens, error) {
	deadline := time.Now().Add(15 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("таймаут 15 минут — код не введён")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		body, _ := json.Marshal(map[string]string{"device_auth_id": dc.DeviceAuthID, "user_code": dc.UserCode})
		req, err := http.NewRequestWithContext(ctx, "POST", codexAPIBase()+"/deviceauth/token", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := sharedHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var cs struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeChallenge     string `json:"code_challenge"`
				CodeVerifier      string `json:"code_verifier"`
			}
			if err := json.Unmarshal(b, &cs); err != nil {
				return nil, err
			}
			// шаг 3: обмен authorization_code на токены (PKCE verifier присылает сам сервер)
			return codexExchangeCode(ctx, cs.AuthorizationCode, cs.CodeVerifier)
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			if onWait != nil {
				onWait()
			}
			t := time.NewTimer(time.Duration(dc.Interval) * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
			continue
		}
		return nil, fmt.Errorf("poll: HTTP %d: %s", resp.StatusCode, truncAuth(string(b), 300))
	}
}

// codexExchangeCode — шаг 3: POST /oauth/token (form, grant_type=authorization_code, как oauth/client.rs).
func codexExchangeCode(ctx context.Context, code, verifier string) (*codexTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"redirect_uri":  {codexIssuer + "/deviceauth/callback"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", codexIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("exchange: HTTP %d: %s", resp.StatusCode, truncAuth(string(b), 300))
	}
	var t codexTokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, fmt.Errorf("exchange: пустой access_token")
	}
	return &t, nil
}

// codexRefreshToken — рефреш access_token (grant_type=refresh_token, тот же /oauth/token).
func codexRefreshToken(ctx context.Context, refresh string) (*codexTokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {codexClientID},
		"refresh_token": {refresh},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", codexIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("refresh: HTTP %d: %s", resp.StatusCode, truncAuth(string(b), 300))
	}
	var t codexTokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refresh
	}
	return &t, nil
}

// CodexDeviceLogin — полный флоу (CLI/TUI): печатает ссылку+код, ждёт, сохраняет токены.
func CodexDeviceLogin(ctx context.Context, out io.Writer) error {
	dc, err := codexRequestUserCode(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n  Открой в браузере:  %s/codex/device\n  Введи код (15 мин): %s\n\n  Жду подтверждения...\n", codexIssuer, dc.UserCode)
	tok, err := codexPollToken(ctx, dc, nil)
	if err != nil {
		return err
	}
	if err := codexSaveTokens(tok); err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ Залогинен. Токены: %s\n", codexAuthPath())
	return nil
}

func truncAuth(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
