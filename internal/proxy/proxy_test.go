package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanying/llm-proxy/internal/assertion"
	"github.com/yuanying/llm-proxy/internal/auth"
)

const (
	realToken = "access-secret-real"
	subject   = "system:serviceaccount:agents:caller-a"
	// goodAssertion is what the fake verifier accepts as the subject's.
	goodAssertion = "assertion-for-caller-a"
	otherSubject  = "assertion-for-someone-else"
)

type fakeTokens struct {
	token string
	err   error
}

func (f fakeTokens) Token(context.Context) (string, error) { return f.token, f.err }

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, a string) (string, *assertion.Refusal) {
	switch a {
	case goodAssertion:
		return subject, nil
	case otherSubject:
		return "", &assertion.Refusal{Status: http.StatusForbidden, Reason: "subject is not allowed"}
	case "":
		return "", &assertion.Refusal{Status: http.StatusUnauthorized, Reason: "no assertion"}
	default:
		return "", &assertion.Refusal{Status: http.StatusUnauthorized, Reason: "bad signature"}
	}
}

// recorded is what the fake upstream received.
type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

type fixture struct {
	t        *testing.T
	upstream *httptest.Server
	proxy    *httptest.Server
	logs     *bytes.Buffer
	mu       sync.Mutex
	got      []recorded
}

type options struct {
	tokens   TokenSource
	verifier Verifier
	routes   string
	handler  http.HandlerFunc
	basePath string
}

func newFixture(t *testing.T, opts options) *fixture {
	t.Helper()
	f := &fixture{t: t, logs: &bytes.Buffer{}}
	if opts.handler == nil {
		opts.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-Id", "req-1")
			w.Header().Set("Openai-Processing-Ms", "12")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"resp_1","output":[]}`)
		}
	}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		f.mu.Unlock()
		opts.handler(w, r)
	}))
	t.Cleanup(f.upstream.Close)
	if opts.basePath == "" {
		// As the real upstream, https://api.openai.com/v1.
		opts.basePath = "/v1"
	}
	upstream, _ := url.Parse(f.upstream.URL + opts.basePath)
	if opts.tokens == nil {
		opts.tokens = fakeTokens{token: realToken}
	}
	if opts.routes == "" {
		opts.routes = DefaultRoutes
	}
	routes, err := ParseRoutes(opts.routes)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Upstream:      upstream,
		Routes:        routes,
		Tokens:        opts.tokens,
		Verifier:      opts.verifier,
		Logger:        slog.New(slog.NewJSONHandler(f.logs, nil)),
		HeaderTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.proxy = httptest.NewServer(handler)
	t.Cleanup(f.proxy.Close)
	return f
}

func (f *fixture) do(method, path string, body io.Reader, header http.Header) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.proxy.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// The test reads what the proxy sent, compressed or not.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *fixture) last() recorded {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		f.t.Fatal("the upstream received nothing")
	}
	return f.got[len(f.got)-1]
}

func (f *fixture) received() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func withAssertion(h http.Header) http.Header {
	if h == nil {
		h = http.Header{}
	}
	h.Set(assertion.Header, goodAssertion)
	return h
}

func TestTheRequestBodyAndHeadersReachTheUpstreamUnchanged(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	body := `{"model":"gpt-x","input":[{"type":"reasoning","encrypted_content":"opaque"}],"prompt_cache_key":"k1","stream":true}`
	resp := f.do(http.MethodPost, "/v1/responses?trace=1", strings.NewReader(body), withAssertion(http.Header{
		"Content-Type":         {"application/json"},
		"Accept":               {"text/event-stream"},
		"Session_id":           {"session-1"},
		"X-Client-Request-Id":  {"session-1"},
		"Openai-Beta":          {"responses=v1"},
		"X-Stainless-Lang":     {"js"},
		"User-Agent":           {"OpenAI/JS 6.0.0"},
		"Authorization":        {"Bearer dummy-from-caller"},
		"Cookie":               {"session=caller-cookie"},
		"X-Forwarded-For":      {"10.0.0.1"},
		"X-Forwarded-Proto":    {"https"},
		"X-Forwarded-Host":     {"llm-proxy.example.com"},
		"X-Real-Ip":            {"10.0.0.1"},
		"Forwarded":            {"for=10.0.0.1"},
		"Gated-Something-Else": {"x"},
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := f.last()
	if got.method != http.MethodPost || got.path != "/v1/responses" || got.query != "trace=1" {
		t.Errorf("upstream got %s %s?%s", got.method, got.path, got.query)
	}
	if string(got.body) != body {
		t.Errorf("body = %q, want it unchanged", got.body)
	}
	for _, name := range []string{"Content-Type", "Accept", "Session_id", "X-Client-Request-Id", "Openai-Beta", "X-Stainless-Lang", "User-Agent"} {
		if got.header.Get(name) == "" {
			t.Errorf("%s did not reach the upstream", name)
		}
	}
	for _, name := range []string{"Cookie", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip", "Forwarded", "Gated-Assertion", "Gated-Something-Else"} {
		if v := got.header.Get(name); v != "" {
			t.Errorf("%s reached the upstream: %q", name, v)
		}
	}
}

func TestTheAuthorizationIsReplacedWithTheRealToken(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), withAssertion(http.Header{"Authorization": {"Bearer dummy"}}))
	if got := f.last().header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+realToken {
		t.Errorf("Authorization = %q, want the real token only", got)
	}
}

func TestTheResponseComesBackUnchanged(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}, handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}})
	resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), withAssertion(nil))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != `{"error":{"type":"usage_limit_reached"}}` {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") != "req-9" {
		t.Errorf("X-Request-Id = %q", resp.Header.Get("X-Request-Id"))
	}
}

func TestACompressedResponseIsNotDecompressed(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, `{"id":"resp_1"}`)
	_ = zw.Close()
	f := newFixture(t, options{verifier: fakeVerifier{}, handler: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding = %q, want the caller's", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}})
	resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), withAssertion(http.Header{"Accept-Encoding": {"gzip"}}))
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, compressed.Bytes()) {
		t.Errorf("got encoding %q and %d bytes, want the upstream's gzip bytes", resp.Header.Get("Content-Encoding"), len(body))
	}
}

func TestStreamedEventsArriveAsTheUpstreamSendsThem(t *testing.T) {
	next := make(chan struct{})
	f := newFixture(t, options{verifier: fakeVerifier{}, handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, event := range []string{"response.created", "response.output_text.delta", "response.completed"} {
			if i > 0 {
				// The next event is only sent once the caller has seen
				// this one: a proxy that buffers never gets there.
				select {
				case <-next:
				case <-time.After(5 * time.Second):
					return
				}
			}
			_, _ = io.WriteString(w, "event: "+event+"\ndata: {}\n\n")
			w.(http.Flusher).Flush()
		}
	}})
	resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`), withAssertion(nil))
	reader := bufio.NewReader(resp.Body)
	var seen []string
	for len(seen) < 3 {
		line, err := readLineWithin(reader, 2*time.Second)
		if err != nil {
			t.Fatalf("after %v: %v", seen, err)
		}
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			seen = append(seen, strings.TrimSpace(after))
			if len(seen) < 3 {
				next <- struct{}{}
			}
		}
	}
	want := []string{"response.created", "response.output_text.delta", "response.completed"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", seen, want)
	}
}

func readLineWithin(r *bufio.Reader, d time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-time.After(d):
		return "", errors.New("no line arrived in time: the stream is buffered")
	}
}

func TestTheUpstreamPathIsJoinedUnderItsBase(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}, basePath: "/openai/v1"})
	f.do(http.MethodGet, "/v1/models", nil, withAssertion(nil))
	if got := f.last().path; got != "/openai/v1/models" {
		t.Errorf("upstream path = %q", got)
	}
}

func TestOnlyTheListedRoutesPass(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	cases := []struct {
		method, path string
		status       int
		allow        string
	}{
		{http.MethodPost, "/v1/responses", http.StatusOK, ""},
		{http.MethodGet, "/v1/models", http.StatusOK, ""},
		{http.MethodGet, "/v1/responses", http.StatusMethodNotAllowed, "POST"},
		{http.MethodDelete, "/v1/models", http.StatusMethodNotAllowed, "GET"},
		{http.MethodGet, "/v1/responses/resp_1", http.StatusNotFound, ""},
		{http.MethodPost, "/v1/chat/completions", http.StatusNotFound, ""},
		{http.MethodPost, "/v1/files", http.StatusNotFound, ""},
		{http.MethodGet, "/", http.StatusNotFound, ""},
		{http.MethodPost, "/v1/../v1/files", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			before := f.received()
			resp := f.do(tc.method, tc.path, strings.NewReader(`{}`), withAssertion(nil))
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.allow != "" && resp.Header.Get("Allow") != tc.allow {
				t.Errorf("Allow = %q, want %q", resp.Header.Get("Allow"), tc.allow)
			}
			if tc.status != http.StatusOK && f.received() != before {
				t.Error("a refused request reached the upstream")
			}
		})
	}
}

func TestRequestsWithoutGatedsAssertionAreRefused(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	cases := []struct {
		name, assertion string
		path            string
		status          int
	}{
		{"no assertion", "", "/v1/responses", http.StatusUnauthorized},
		{"a forged assertion", "forged", "/v1/responses", http.StatusUnauthorized},
		{"a subject not allowed", otherSubject, "/v1/responses", http.StatusForbidden},
		{"no assertion on a path that does not exist", "", "/v1/files", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.assertion != "" {
				h.Set(assertion.Header, tc.assertion)
			}
			resp := f.do(http.MethodPost, tc.path, strings.NewReader(`{}`), h)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
	if n := f.received(); n != 0 {
		t.Errorf("%d refused requests reached the upstream", n)
	}
}

func TestWithoutAVerifierRequestsPassWithoutAnAssertion(t *testing.T) {
	f := newFixture(t, options{})
	if resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), nil); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestHealthzAnswersWithoutAnAssertionOrAToken(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}, tokens: fakeTokens{err: auth.ErrNoCredential}})
	if resp := f.do(http.MethodGet, "/healthz", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if n := f.received(); n != 0 {
		t.Error("healthz reached the upstream")
	}
}

func TestWithoutATokenTheProxyIsUnavailable(t *testing.T) {
	for _, err := range []error{auth.ErrNoCredential, auth.ErrExpired} {
		f := newFixture(t, options{verifier: fakeVerifier{}, tokens: fakeTokens{err: err}})
		resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), withAssertion(nil))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%v: status = %d, want 503", err, resp.StatusCode)
		}
		if f.received() != 0 {
			t.Errorf("%v: reached the upstream", err)
		}
	}
}

func TestAnUnreachableUpstreamIsABadGateway(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	f.upstream.Close()
	resp := f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), withAssertion(nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestTheAccessLogHasTheAgreedFieldsAndNoSecret(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	f.do(http.MethodPost, "/v1/responses?key=query-secret", strings.NewReader(`{"input":"body-secret"}`), withAssertion(http.Header{
		"Authorization": {"Bearer caller-secret"},
		"Cookie":        {"cookie-secret"},
	}))
	f.do(http.MethodPost, "/v1/responses", strings.NewReader(`{}`), http.Header{assertion.Header: {"forged"}})

	logs := f.logs.String()
	for _, secret := range []string{realToken, "caller-secret", "cookie-secret", "query-secret", "body-secret", goodAssertion, "forged"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs carry %q:\n%s", secret, logs)
		}
	}
	var entries []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		if entry["msg"] == "request" {
			entries = append(entries, entry)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("got %d access log entries, want 2:\n%s", len(entries), logs)
	}
	first := entries[0]
	if first["method"] != "POST" || first["path"] != "/v1/responses" || first["status"] != float64(200) || first["subject"] != subject {
		t.Errorf("entry = %v", first)
	}
	if _, ok := first["duration"]; !ok {
		t.Errorf("entry has no duration: %v", first)
	}
	if second := entries[1]; second["status"] != float64(401) || second["subject"] != "" {
		t.Errorf("refused entry = %v", second)
	}
}

func TestParsingRoutes(t *testing.T) {
	routes, err := ParseRoutes("POST /v1/responses, GET /v1/models")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0] != (Route{Method: "POST", Path: "/v1/responses"}) || routes[1] != (Route{Method: "GET", Path: "/v1/models"}) {
		t.Errorf("routes = %v", routes)
	}
	for _, bad := range []string{"", "POST", "POST responses", "POST /responses", "POST /v1/a b"} {
		if _, err := ParseRoutes(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
