package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
)

func TestXAIDeviceLoginWaitsForApprovalAndWritesTheCredential(t *testing.T) {
	var polled atomic.Bool
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost || r.Form.Get("client_id") != "test-client" {
			t.Error("the xAI request did not use the configured client")
			http.Error(w, "bad client", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/oauth2/device/code":
			if !strings.Contains(r.Form.Get("scope"), "api:access") {
				t.Error("the device grant did not ask for API access")
			}
			_, _ = io.WriteString(w, `{"verification_uri":"https://login.example.com/device","user_code":"ABCD-EFGH","device_code":"machine-only","expires_in":60,"interval":0.001}`)
		case "/oauth2/token":
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "machine-only" {
				t.Error("the token request did not exchange the device code")
			}
			if !polled.Swap(true) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"test-access","refresh_token":"test-refresh","expires_in":3600}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"login", "--provider", "xai", "--data-dir", dir, "--oauth-issuer", issuer.URL, "--xai-client-id", "test-client", "--timeout", "2s"}
	if code := run(args, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("xAI login exited %d: %s", code, &stderr)
	}
	cred, _, err := auth.NewFileStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "test-access" || cred.RefreshToken != "test-refresh" || cred.ClientID != "test-client" {
		t.Error("auth.json does not contain the new xAI credential")
	}
	if out := stdout.String(); !strings.Contains(out, "https://login.example.com/device") || !strings.Contains(out, "ABCD-EFGH") {
		t.Error("login did not display the verification URL and user code")
	}
	for _, secret := range []string{"machine-only", "test-access", "test-refresh"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Error("login output disclosed a secret")
		}
	}
}

func TestXAIServeSendsTheResponsesBodyUnchanged(t *testing.T) {
	const body = "{\n  \"max_output_tokens\": 30, \"tools\": [{\"type\": \"x_search\"}]\n}\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != body {
			t.Error("xAI received a different request body")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-xai-access" {
			t.Error("xAI did not get an authenticated POST /v1/responses")
		}
		_, _ = io.WriteString(w, `{"object":"response"}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	store := auth.NewFileStore(dir)
	if _, err := store.Save(xaiTestCredential("test-xai-access")); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseServe([]string{"--provider", "xai", "--data-dir", dir, "--upstream", upstream.URL + "/v1", "--oauth-issuer", upstream.URL, "--insecure-skip-assertion"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(cfg, newTokenSource(cfg, store, nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-caller")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"object":"response"}` {
		t.Errorf("response = %d %q", resp.Code, resp.Body.String())
	}
}

func TestXAIRefreshKeepsTheRefreshTokenWhenItIsNotRotated(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.URL.Path != "/oauth2/token" ||
			r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "test-client" || r.Form.Get("refresh_token") != "test-refresh" {
			t.Error("the stored xAI grant was not used to refresh")
			http.Error(w, "bad refresh request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"test-new-access","expires_in":3600}`)
	}))
	defer issuer.Close()

	dir := t.TempDir()
	store := auth.NewFileStore(dir)
	cred := xaiTestCredential("test-old-access")
	cred.ExpiresAt = time.Now().Add(5 * time.Minute)
	if _, err := store.Save(cred); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseServe([]string{"--provider", "xai", "--data-dir", dir, "--oauth-issuer", issuer.URL, "--insecure-skip-assertion"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if token, err := newTokenSource(cfg, store, nil).Token(t.Context()); err != nil || token != "test-new-access" {
		t.Fatalf("the near-expiry token was not refreshed: %v", err)
	}
	got, _, err := store.Load()
	if err != nil || got.RefreshToken != "test-refresh" {
		t.Fatalf("the original refresh token was not preserved on disk: %v", err)
	}
}

func TestXAIServeDoesNotUseAnOpenAICredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the stored OpenAI credential should not be forwarded or refreshed")
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	store := auth.NewFileStore(dir)
	if _, err := store.Save(auth.Credential{
		AccessToken: "test-openai-access", RefreshToken: "test-openai-refresh",
		ClientID: "test-client", ExpiresAt: time.Now().Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseServe([]string{"--provider", "xai", "--data-dir", dir, "--oauth-issuer", upstream.URL, "--upstream", upstream.URL + "/v1", "--insecure-skip-assertion"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(cfg, newTokenSource(cfg, store, nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.Code)
	}
}

func xaiTestCredential(access string) auth.Credential {
	return auth.Credential{
		Provider: "xai", AccessToken: access, RefreshToken: "test-refresh", ClientID: "test-client",
		ExpiresAt: time.Now().Add(time.Hour),
	}
}
