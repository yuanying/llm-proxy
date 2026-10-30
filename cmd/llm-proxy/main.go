// Command llm-proxy serves one ChatGPT subscription to trusted callers on the
// OpenAI Responses API.
//
//	llm-proxy serve    run the proxy
//	llm-proxy login    sign in with ChatGPT and write the token file
//	llm-proxy version  print the version
//
// Every flag can also be set by an environment variable: LLM_PROXY_ followed
// by the flag's name in upper case, with - as _. A flag on the command line
// wins over the environment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yuanying/llm-proxy/internal/assertion"
	"github.com/yuanying/llm-proxy/internal/auth"
	"github.com/yuanying/llm-proxy/internal/login"
	"github.com/yuanying/llm-proxy/internal/oauth"
	"github.com/yuanying/llm-proxy/internal/proxy"
)

// version is set by the build (-X main.version).
var version = "dev"

const usage = `usage: llm-proxy <command> [flags]

commands:
  serve    run the proxy
  login    sign in with ChatGPT and write the token file
  version  print the version

Run "llm-proxy <command> -h" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	var err error
	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "serve":
		var cfg serveConfig
		if cfg, err = parseServe(args[1:], os.Getenv); err == nil {
			err = serve(ctx, cfg, logger)
		}
	case "login":
		var cfg loginConfig
		if cfg, err = parseLogin(args[1:], os.Getenv); err == nil {
			err = runLogin(ctx, cfg, stdin, stdout)
		}
	default:
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "llm-proxy %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// parseFlags parses args after taking each flag's default from the
// environment.
func parseFlags(fs *flag.FlagSet, args []string, getenv func(string) string) error {
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		name := "LLM_PROXY_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if value := getenv(name); value != "" && envErr == nil {
			if err := f.Value.Set(value); err != nil {
				envErr = fmt.Errorf("%s: %w", name, err)
			}
		}
	})
	if envErr != nil {
		return envErr
	}
	return fs.Parse(args)
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

type serveConfig struct {
	listen          string
	dataDir         string
	upstream        *url.URL
	routes          []proxy.Route
	headerTimeout   time.Duration
	refreshBefore   time.Duration
	oauthIssuer     string
	issuer          string
	jwksURL         string
	audience        string
	allowedSubjects []string
	skipAssertion   bool
}

func parseServe(args []string, getenv func(string) string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	dataDir := fs.String("data-dir", "", "directory holding the token file (required)")
	upstream := fs.String("upstream", "https://api.openai.com/v1", "upstream URL that stands for /v1")
	routes := fs.String("routes", proxy.DefaultRoutes, `comma-separated "METHOD /v1/path" entries that pass`)
	headerTimeout := fs.Duration("upstream-header-timeout", 10*time.Minute, "how long the upstream may take to send the response headers")
	refreshBefore := fs.Duration("refresh-before", 10*time.Minute, "how long before expiry the access token is refreshed")
	oauthIssuer := fs.String("oauth-issuer", oauth.DefaultIssuer, "origin of the OAuth token endpoint")
	issuer := fs.String("gated-issuer", "", "gated's central authentication host, as an https origin")
	jwksURL := fs.String("gated-jwks-url", "", "where gated publishes its keys (default <gated-issuer>/__gated/jwks.json)")
	audience := fs.String("gated-audience", "", "this proxy's public host name, as gated puts it in the assertion's aud")
	subjects := fs.String("allowed-subjects", "", "comma-separated subjects allowed through, e.g. system:serviceaccount:<namespace>:<name>")
	skip := fs.Bool("insecure-skip-assertion", false, "do not check gated's assertion; anyone who reaches the proxy can use it")
	if err := parseFlags(fs, args, getenv); err != nil {
		return serveConfig{}, err
	}

	cfg := serveConfig{
		listen:          *listen,
		dataDir:         *dataDir,
		headerTimeout:   *headerTimeout,
		refreshBefore:   *refreshBefore,
		oauthIssuer:     *oauthIssuer,
		issuer:          strings.TrimRight(*issuer, "/"),
		jwksURL:         *jwksURL,
		audience:        strings.ToLower(*audience),
		allowedSubjects: splitList(*subjects),
		skipAssertion:   *skip,
	}
	if cfg.dataDir == "" {
		return serveConfig{}, errors.New("--data-dir is required")
	}
	u, err := url.Parse(*upstream)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return serveConfig{}, fmt.Errorf("--upstream %q is not an http(s) URL", *upstream)
	}
	cfg.upstream = u
	if cfg.routes, err = proxy.ParseRoutes(*routes); err != nil {
		return serveConfig{}, fmt.Errorf("--routes: %w", err)
	}

	checking := cfg.issuer != "" || cfg.audience != "" || len(cfg.allowedSubjects) > 0 || *jwksURL != ""
	switch {
	case cfg.skipAssertion && checking:
		return serveConfig{}, errors.New("--insecure-skip-assertion cannot be combined with the gated settings")
	case cfg.skipAssertion:
		return cfg, nil
	case cfg.issuer == "":
		return serveConfig{}, errors.New("--gated-issuer is required to check gated's assertion (or pass --insecure-skip-assertion to run unchecked)")
	case cfg.audience == "":
		return serveConfig{}, errors.New("--gated-audience is required")
	case len(cfg.allowedSubjects) == 0:
		return serveConfig{}, errors.New("--allowed-subjects is required")
	}
	if cfg.jwksURL == "" {
		cfg.jwksURL = cfg.issuer + "/__gated/jwks.json"
	}
	return cfg, nil
}

func serve(ctx context.Context, cfg serveConfig, logger *slog.Logger) error {
	unlock, err := auth.LockDir(cfg.dataDir)
	if err != nil {
		return err
	}
	defer unlock()

	store := auth.NewFileStore(cfg.dataDir)
	tokens := auth.NewSource(auth.SourceConfig{
		Store:         store,
		Refresher:     &oauth.Client{Issuer: cfg.oauthIssuer},
		RefreshBefore: cfg.refreshBefore,
		Logger:        logger,
	})
	if _, _, err := store.Load(); errors.Is(err, os.ErrNotExist) {
		logger.Warn("no token file yet; requests get 503 until `llm-proxy login` writes one", "path", store.Path())
	}
	go tokens.Run(ctx, time.Minute)

	var verifier proxy.Verifier
	if cfg.skipAssertion {
		logger.Warn("gated's assertion is not checked: anyone who reaches the proxy can use the subscription")
	} else {
		verifier = assertion.New(assertion.Config{
			Issuer:          cfg.issuer,
			JWKSURL:         cfg.jwksURL,
			Audience:        cfg.audience,
			AllowedSubjects: cfg.allowedSubjects,
			Logger:          logger,
		})
	}

	handler, err := proxy.New(proxy.Config{
		Upstream:      cfg.upstream,
		Routes:        cfg.routes,
		Tokens:        tokens,
		Verifier:      verifier,
		Logger:        logger,
		HeaderTimeout: cfg.headerTimeout,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	logger.Info("serving", "version", version, "listen", cfg.listen, "upstream", cfg.upstream.String())

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	// Give running turns a while to finish before cutting them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	return nil
}

type loginConfig struct {
	dataDir        string
	oauthIssuer    string
	agentName      string
	callbackListen string
	timeout        time.Duration
}

func parseLogin(args []string, getenv func(string) string) (loginConfig, error) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "directory holding the token file (required)")
	oauthIssuer := fs.String("oauth-issuer", oauth.DefaultIssuer, "origin of the OAuth endpoints")
	agentName := fs.String("agent-name", "llm-proxy", "name the login introduces itself with")
	callbackListen := fs.String("callback-listen", "127.0.0.1:1455", "where to listen for the browser's callback; empty to only take a pasted URL")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the login")
	if err := parseFlags(fs, args, getenv); err != nil {
		return loginConfig{}, err
	}
	if *dataDir == "" {
		return loginConfig{}, errors.New("--data-dir is required")
	}
	return loginConfig{
		dataDir:        *dataDir,
		oauthIssuer:    *oauthIssuer,
		agentName:      *agentName,
		callbackListen: *callbackListen,
		timeout:        *timeout,
	}, nil
}

func runLogin(ctx context.Context, cfg loginConfig, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	flow := &login.Flow{
		OAuth:        &oauth.Client{Issuer: cfg.oauthIssuer},
		Store:        auth.NewFileStore(cfg.dataDir),
		AgentName:    cfg.agentName,
		CallbackAddr: cfg.callbackListen,
		In:           stdin,
		Out:          stdout,
	}
	_, err := flow.Run(ctx)
	return err
}
