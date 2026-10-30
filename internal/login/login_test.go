package login

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
	"github.com/yuanying/llm-proxy/internal/oauth"
)

// fakeAuthServer is the authorization server's token endpoint. It accepts
// the one code it was told about, with the verifier matching the challenge
// the login put on its authorize URL.
type fakeAuthServer struct {
	*httptest.Server
	mu        sync.Mutex
	challenge string
	code      string
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{code: "the-code"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/oauth/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		challenge := f.challenge
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != f.code || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge ||
			r.PostForm.Get("client_id") != "issued-client" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-secret-login",
			"refresh_token": "refresh-secret-login",
			"id_token":      "id-token",
			"expires_in":    3600,
			"scope":         "openid offline_access chatgpt.tokens.use.direct",
		})
	}))
	t.Cleanup(f.Close)
	return f
}

type loginFixture struct {
	t      *testing.T
	auth   *fakeAuthServer
	store  *auth.FileStore
	flow   *Flow
	stdin  *io.PipeWriter
	lines  chan string
	output *strings.Builder
	mu     sync.Mutex
}

func newLoginFixture(t *testing.T, callbackAddr string) *loginFixture {
	t.Helper()
	authServer := newFakeAuthServer(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &loginFixture{
		t: t, auth: authServer, store: auth.NewFileStore(t.TempDir()),
		stdin: inW, lines: make(chan string, 100), output: &strings.Builder{},
	}
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			f.mu.Lock()
			f.output.WriteString(scanner.Text() + "\n")
			f.mu.Unlock()
			f.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() { _ = outW.Close(); _ = inW.Close() })
	f.flow = &Flow{
		OAuth:        &oauth.Client{Issuer: authServer.URL},
		Store:        f.store,
		AgentName:    "llm-proxy",
		CallbackAddr: callbackAddr,
		In:           inR,
		Out:          outW,
	}
	return f
}

// start runs the login and returns the authorize URL it printed.
func (f *loginFixture) start(ctx context.Context) (*url.URL, chan error) {
	f.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := f.flow.Run(ctx)
		done <- err
	}()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-f.lines:
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, f.auth.URL) {
				continue
			}
			u, err := url.Parse(line)
			if err != nil {
				f.t.Fatal(err)
			}
			f.auth.mu.Lock()
			f.auth.challenge = u.Query().Get("code_challenge")
			f.auth.mu.Unlock()
			return u, done
		case err := <-done:
			f.t.Fatalf("login ended before printing the URL: %v", err)
		case <-timeout:
			f.t.Fatal("login printed no authorize URL")
		}
	}
}

func (f *loginFixture) paste(line string) {
	f.t.Helper()
	go func() { _, _ = io.WriteString(f.stdin, line+"\n") }()
}

func (f *loginFixture) wait(done chan error) error {
	f.t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		f.t.Fatal("login did not finish")
		return nil
	}
}

func (f *loginFixture) printed() string {
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.output.String()
}

func callbackURL(state string) string {
	return fmt.Sprintf("http://127.0.0.1:1455/auth/callback?code=the-code&state=%s&client_id=issued-client", url.QueryEscape(state))
}

func TestPastingTheCallbackURLSavesTheCredential(t *testing.T) {
	f := newLoginFixture(t, "")
	authorize, done := f.start(context.Background())
	f.paste(callbackURL(authorize.Query().Get("state")))
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
	saved, _, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "access-secret-login" || saved.RefreshToken != "refresh-secret-login" || saved.ClientID != "issued-client" {
		t.Errorf("saved %+v", saved)
	}
}

func TestTheLoginNamesThisInstallation(t *testing.T) {
	f := newLoginFixture(t, "")
	authorize, done := f.start(context.Background())
	f.paste(callbackURL(authorize.Query().Get("state")))
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
	id, err := f.store.DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if got := authorize.Query().Get("ext_agent_host_id"); got != "urn:uuid:"+id {
		t.Errorf("host ID = %q, want the stored device ID %q", got, id)
	}
	if got := authorize.Query().Get("agent_name_hint"); got != "llm-proxy" {
		t.Errorf("agent name = %q", got)
	}
}

func TestTheBrowserCanReachTheCallbackThroughAPortForward(t *testing.T) {
	addr := freeAddr(t)
	f := newLoginFixture(t, addr)
	authorize, done := f.start(context.Background())
	callback, _ := url.Parse(callbackURL(authorize.Query().Get("state")))
	callback.Host = addr
	resp, err := http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback answered %d", resp.StatusCode)
	}
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Load(); err != nil {
		t.Errorf("nothing saved: %v", err)
	}
}

func TestACallbackForAnotherLoginIsTurnedAwayAndTheLoginKeepsWaiting(t *testing.T) {
	addr := freeAddr(t)
	f := newLoginFixture(t, addr)
	authorize, done := f.start(context.Background())
	stale, _ := url.Parse(callbackURL("another-state"))
	stale.Host = addr
	resp, err := http.Get(stale.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a stale callback answered %d, want 400", resp.StatusCode)
	}
	f.paste("not a url at all")
	f.paste(callbackURL(authorize.Query().Get("state")))
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
}

func TestADeclinedAuthorizationEndsTheLogin(t *testing.T) {
	f := newLoginFixture(t, "")
	authorize, done := f.start(context.Background())
	f.paste("http://127.0.0.1:1455/auth/callback?error=access_denied&state=" + url.QueryEscape(authorize.Query().Get("state")))
	if err := f.wait(done); !errors.Is(err, oauth.ErrAuthorizationDenied) {
		t.Errorf("err = %v, want ErrAuthorizationDenied", err)
	}
	if _, _, err := f.store.Load(); err == nil {
		t.Error("a declined login saved a credential")
	}
}

func TestAPortInUseLeavesPastingAsTheWay(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	f := newLoginFixture(t, busy.Addr().String())
	authorize, done := f.start(context.Background())
	f.paste(callbackURL(authorize.Query().Get("state")))
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
}

func TestCancellingEndsTheLogin(t *testing.T) {
	f := newLoginFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	_, done := f.start(ctx)
	cancel()
	if err := f.wait(done); err == nil {
		t.Error("a cancelled login succeeded")
	}
}

func TestTheTerminalNeverShowsATokenOrTheVerifier(t *testing.T) {
	f := newLoginFixture(t, "")
	authorize, done := f.start(context.Background())
	f.paste(callbackURL(authorize.Query().Get("state")))
	if err := f.wait(done); err != nil {
		t.Fatal(err)
	}
	out := f.printed()
	for _, secret := range []string{"access-secret-login", "refresh-secret-login"} {
		if strings.Contains(out, secret) {
			t.Errorf("terminal shows %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, f.store.Path()) {
		t.Errorf("terminal does not say where the credential went:\n%s", out)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
