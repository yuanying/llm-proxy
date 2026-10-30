package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestVersionPrintsTheBuiltVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("printed %q, want %q", got, version)
	}
}

func TestAnUnknownCommandPrintsTheUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"frobnicate"}, nil, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "serve") || !strings.Contains(stderr.String(), "login") {
		t.Errorf("usage does not name the commands: %q", stderr.String())
	}
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func gatedArgs() []string {
	return []string{
		"--data-dir", "/data",
		"--gated-issuer", "https://auth.example.com",
		"--gated-audience", "llm-proxy.example.com",
		"--allowed-subjects", "system:serviceaccount:agents:caller-a, system:serviceaccount:agents:other",
	}
}

func TestServeTakesItsDefaults(t *testing.T) {
	cfg, err := parseServe(gatedArgs(), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != ":8080" || cfg.upstream.String() != "https://api.openai.com/v1" ||
		cfg.headerTimeout != 10*time.Minute || cfg.refreshBefore != 10*time.Minute {
		t.Errorf("defaults = %+v", cfg)
	}
	if len(cfg.routes) != 2 {
		t.Errorf("routes = %v", cfg.routes)
	}
	if cfg.jwksURL != "https://auth.example.com/__gated/jwks.json" {
		t.Errorf("JWKS URL = %q, want the issuer's", cfg.jwksURL)
	}
	if strings.Join(cfg.allowedSubjects, "|") != "system:serviceaccount:agents:caller-a|system:serviceaccount:agents:other" {
		t.Errorf("subjects = %q", cfg.allowedSubjects)
	}
}

func TestServeReadsItsSettingsFromTheEnvironment(t *testing.T) {
	cfg, err := parseServe(nil, env(map[string]string{
		"LLM_PROXY_DATA_DIR":         "/var/lib/llm-proxy",
		"LLM_PROXY_LISTEN":           ":9090",
		"LLM_PROXY_UPSTREAM":         "https://upstream.example.com/v1",
		"LLM_PROXY_ROUTES":           "POST /v1/responses",
		"LLM_PROXY_GATED_ISSUER":     "https://auth.example.com",
		"LLM_PROXY_GATED_JWKS_URL":   "https://keys.example.com/jwks.json",
		"LLM_PROXY_GATED_AUDIENCE":   "llm-proxy.example.com",
		"LLM_PROXY_ALLOWED_SUBJECTS": "system:serviceaccount:agents:caller-a",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dataDir != "/var/lib/llm-proxy" || cfg.listen != ":9090" || cfg.upstream.Host != "upstream.example.com" ||
		len(cfg.routes) != 1 || cfg.jwksURL != "https://keys.example.com/jwks.json" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestAFlagWinsOverTheEnvironment(t *testing.T) {
	cfg, err := parseServe(append(gatedArgs(), "--listen", ":7070"), env(map[string]string{"LLM_PROXY_LISTEN": ":9090"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != ":7070" {
		t.Errorf("listen = %q, want the flag's", cfg.listen)
	}
}

func TestServeRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	cases := map[string][]string{
		"no data directory":             {"--gated-issuer", "https://auth.example.com", "--gated-audience", "a.example.com", "--allowed-subjects", "s"},
		"no assertion settings":         {"--data-dir", "/data"},
		"no audience":                   {"--data-dir", "/data", "--gated-issuer", "https://auth.example.com", "--allowed-subjects", "s"},
		"no subjects":                   {"--data-dir", "/data", "--gated-issuer", "https://auth.example.com", "--gated-audience", "a.example.com"},
		"skipping and checking":         append(gatedArgs(), "--insecure-skip-assertion"),
		"a bad route":                   append(gatedArgs(), "--routes", "POST /responses"),
		"an upstream that is not a URL": append(gatedArgs(), "--upstream", "api.openai.com"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseServe(args, env(nil)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestServeRunsUncheckedOnlyWhenToldTo(t *testing.T) {
	cfg, err := parseServe([]string{"--data-dir", "/data", "--insecure-skip-assertion"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.skipAssertion {
		t.Error("the assertion is still checked")
	}
}

func TestLoginTakesItsDefaults(t *testing.T) {
	cfg, err := parseLogin([]string{"--data-dir", "/data"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.agentName != "llm-proxy" || cfg.callbackListen != "127.0.0.1:1455" || cfg.timeout != 10*time.Minute {
		t.Errorf("defaults = %+v", cfg)
	}
	if _, err := parseLogin(nil, env(nil)); err == nil {
		t.Error("login without a data directory was accepted")
	}
	cfg, err = parseLogin(nil, env(map[string]string{"LLM_PROXY_DATA_DIR": "/data"}))
	if err != nil || cfg.dataDir != "/data" {
		t.Errorf("data directory from the environment: %+v, %v", cfg, err)
	}
}
