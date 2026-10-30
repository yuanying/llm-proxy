// Package proxy is the pass-through to the upstream Responses API (ADR 0001):
// it checks gated's assertion, lets the listed routes through, puts the real
// access token on the request and streams the answer back as it arrives.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yuanying/llm-proxy/internal/assertion"
	"github.com/yuanying/llm-proxy/internal/auth"
)

// DefaultRoutes is what passes unless configured otherwise.
const DefaultRoutes = "POST /v1/responses,GET /v1/models"

// HealthPath answers 200 without an assertion or a token.
const HealthPath = "/healthz"

// TokenSource hands out the access token to put on a request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Verifier checks gated's assertion and returns the subject.
type Verifier interface {
	Verify(ctx context.Context, assertion string) (string, *assertion.Refusal)
}

// Route is one method and path that passes. The path is matched exactly.
type Route struct {
	Method string
	Path   string
}

// ParseRoutes reads "METHOD /v1/path" entries separated by commas.
func ParseRoutes(s string) ([]Route, error) {
	var routes []Route
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.Fields(entry)
		if len(fields) != 2 {
			return nil, fmt.Errorf("route %q is not METHOD /v1/path", entry)
		}
		method, path := strings.ToUpper(fields[0]), fields[1]
		if !strings.HasPrefix(path, "/v1/") {
			return nil, fmt.Errorf("route %q is not under /v1/", entry)
		}
		routes = append(routes, Route{Method: method, Path: path})
	}
	if len(routes) == 0 {
		return nil, errors.New("no routes")
	}
	return routes, nil
}

// Config configures the proxy.
type Config struct {
	// Upstream is the URL that stands for /v1: a request for /v1/responses
	// goes to Upstream's path followed by /responses.
	Upstream *url.URL
	Routes   []Route
	Tokens   TokenSource
	// Verifier checks gated's assertion. Nil lets every request through
	// unchecked; that is only for an explicit opt-out (ADR 0004).
	Verifier Verifier
	Logger   *slog.Logger
	// HeaderTimeout is how long the upstream may take to send the response
	// headers. The body is not limited: a stream lasts as long as the turn.
	HeaderTimeout time.Duration
	// Transport overrides the transport to the upstream, for tests.
	Transport http.RoundTripper
}

type tokenKey struct{}

// New returns the proxy's handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.Upstream == nil || cfg.Tokens == nil || len(cfg.Routes) == 0 {
		return nil, errors.New("proxy: upstream, tokens and routes are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = cfg.HeaderTimeout
		// Pass the caller's Accept-Encoding and the upstream's encoding
		// through as they are; decompressing would change the body.
		t.DisableCompression = true
		transport = t
	}
	upstream := *cfg.Upstream
	upstream.Path = strings.TrimRight(upstream.Path, "/")
	upstream.RawPath = ""

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = upstream.Scheme
			out.URL.Host = upstream.Host
			out.URL.Path = upstream.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1")
			out.URL.RawPath = ""
			out.URL.RawQuery = pr.In.URL.RawQuery
			out.Host = ""
			stripHeaders(out.Header)
			out.Header.Set("Authorization", "Bearer "+out.Context().Value(tokenKey{}).(string))
		},
		Transport: transport,
		// Every write is flushed at once: an event of a stream reaches the
		// caller as soon as the upstream sent it.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
				// The caller went away; nobody reads an answer.
				w.WriteHeader(499)
				return
			}
			cfg.Logger.Error("the upstream did not answer", "path", r.URL.Path, "error", describe(err))
			http.Error(w, "the upstream did not answer", http.StatusBadGateway)
		},
		ErrorLog: slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn),
	}

	h := &handler{cfg: cfg, proxy: rp}
	return h, nil
}

// describe names the failure without the URL's query or anything else of
// the request.
func describe(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return err.Error()
}

// stripHeaders removes what the proxy does not pass on (ADR 0001): the
// caller's credentials, gated's headers and the forwarding headers of the
// ingress. Hop-by-hop headers are already gone.
func stripHeaders(h http.Header) {
	for name := range h {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case canonical == "Authorization", canonical == "Cookie",
			canonical == "Forwarded", canonical == "X-Real-Ip",
			strings.HasPrefix(canonical, "X-Forwarded-"),
			strings.HasPrefix(canonical, "Gated-"):
			delete(h, name)
		}
	}
}

type handler struct {
	cfg   Config
	proxy *httputil.ReverseProxy
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthPath {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
		return
	}

	start := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	var subject string
	defer func() {
		h.cfg.Logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
			"subject", subject,
		)
	}()

	// The assertion is checked before the route, so that nobody who cannot
	// pass learns which paths exist.
	if h.cfg.Verifier != nil {
		s, refusal := h.cfg.Verifier.Verify(r.Context(), r.Header.Get(assertion.Header))
		if refusal != nil {
			h.cfg.Logger.Warn("refused a request", "path", r.URL.Path, "reason", refusal.Reason)
			http.Error(rec, http.StatusText(refusal.Status), refusal.Status)
			return
		}
		subject = s
	}

	if status, allow := h.route(r); status != 0 {
		if allow != "" {
			rec.Header().Set("Allow", allow)
		}
		http.Error(rec, http.StatusText(status), status)
		return
	}

	token, err := h.cfg.Tokens.Token(r.Context())
	if err != nil {
		reason := "no access token"
		if errors.Is(err, auth.ErrExpired) {
			reason = "the access token has expired"
		}
		http.Error(rec, reason, http.StatusServiceUnavailable)
		return
	}
	h.proxy.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), tokenKey{}, token)))
}

// route returns 0 when the request's route is listed, or the status to
// refuse it with and the methods its path allows.
func (h *handler) route(r *http.Request) (status int, allow string) {
	var methods []string
	for _, route := range h.cfg.Routes {
		if route.Path != r.URL.Path {
			continue
		}
		if route.Method == r.Method {
			return 0, ""
		}
		if !slices.Contains(methods, route.Method) {
			methods = append(methods, route.Method)
		}
	}
	if len(methods) == 0 {
		return http.StatusNotFound, ""
	}
	return http.StatusMethodNotAllowed, strings.Join(methods, ", ")
}

// recorder remembers the status for the access log. Unwrap lets the reverse
// proxy reach the connection's Flush through it.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
