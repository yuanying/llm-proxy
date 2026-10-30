// Package login runs the interactive "Sign in with ChatGPT" login where the
// token is kept (ADR 0003): it prints the authorize URL, takes the callback
// either pasted into the terminal or through a port-forward to the callback
// port, and writes the credential to the token file.
package login

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
	"github.com/yuanying/llm-proxy/internal/oauth"
)

// Flow is one login.
type Flow struct {
	OAuth *oauth.Client
	Store *auth.FileStore
	// AgentName is the name the login introduces itself with.
	AgentName string
	// CallbackAddr is where the callback is listened for, normally
	// 127.0.0.1:1455. Empty leaves pasting as the only way.
	CallbackAddr string
	In           io.Reader
	Out          io.Writer
}

type callback struct {
	code, clientID string
	err            error
}

// Run logs in and saves the credential. It returns when the credential is
// saved, the authorization was declined, or ctx is done.
func (f *Flow) Run(ctx context.Context) (auth.Credential, error) {
	deviceID, err := f.Store.DeviceID()
	if err != nil {
		return auth.Credential{}, fmt.Errorf("reading the device ID: %w", err)
	}
	verifier, challenge := oauth.NewPKCE()
	state := oauth.RandomValue()
	authorizeURL := f.OAuth.AuthorizeURL(oauth.AuthorizeParams{
		AgentName: f.AgentName,
		DeviceID:  deviceID,
		State:     state,
		Nonce:     oauth.RandomValue(),
		Challenge: challenge,
	})

	results := make(chan callback, 1)
	deliver := func(c callback) {
		select {
		case results <- c:
		default:
		}
	}
	listening := false
	if f.CallbackAddr != "" {
		stop, err := f.listen(state, deliver)
		if err != nil {
			f.printf("Not listening for the callback on %s (%v); paste the URL instead.\n\n", f.CallbackAddr, err)
		} else {
			listening = true
			defer stop()
		}
	}

	f.printf("Open this URL in a browser and sign in with ChatGPT:\n\n%s\n\n", authorizeURL)
	f.printf("After you approve, the browser goes to %s, which does not open.\n", oauth.RedirectURI)
	f.printf("Copy the whole URL from the address bar and paste it here.\n")
	if listening {
		f.printf("(With `kubectl port-forward <pod> 1455:1455` running, the browser delivers it by itself.)\n")
	}
	f.printf("\n")
	go f.readPasted(ctx, state, deliver)

	var got callback
	select {
	case got = <-results:
	case <-ctx.Done():
		return auth.Credential{}, fmt.Errorf("login cancelled: %w", ctx.Err())
	}
	if got.err != nil {
		return auth.Credential{}, got.err
	}

	f.printf("Exchanging the authorization code for tokens...\n")
	cred, err := f.OAuth.Exchange(ctx, got.code, verifier, got.clientID)
	if err != nil {
		return auth.Credential{}, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	if _, err := f.Store.Save(cred); err != nil {
		return auth.Credential{}, fmt.Errorf("writing %s: %w", f.Store.Path(), err)
	}
	f.printf("Saved the credential to %s. It expires at %s and is refreshed by `llm-proxy serve`.\n",
		f.Store.Path(), cred.ExpiresAt.Format(time.RFC3339))
	return cred, nil
}

func (f *Flow) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(f.Out, format, args...)
}

// readPasted takes lines from the terminal until one is this login's
// callback. A line that is not keeps the login waiting; a callback carrying
// an error ends it.
func (f *Flow) readPasted(ctx context.Context, state string, deliver func(callback)) {
	scanner := bufio.NewScanner(f.In)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			f.printf("That is not a URL. Paste the whole URL from the address bar.\n")
			continue
		}
		code, clientID, err := oauth.ParseCallback(u, state)
		if errors.Is(err, oauth.ErrAuthorizationDenied) {
			deliver(callback{err: err})
			return
		}
		if err != nil {
			f.printf("%v. Paste the whole URL from the address bar.\n", err)
			continue
		}
		deliver(callback{code: code, clientID: clientID})
		return
	}
	// The terminal closed: the callback can still come through the
	// listener, so the login keeps waiting for it or for its deadline.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		f.printf("Reading the terminal failed (%v); waiting for the browser's callback.\n", err)
	}
}

// listen serves the callback path on CallbackAddr. The listener is on the
// loopback address only; a port-forward reaches it, other hosts do not.
func (f *Flow) listen(state string, deliver func(callback)) (stop func(), err error) {
	l, err := net.Listen("tcp", f.CallbackAddr)
	if err != nil {
		return nil, err
	}
	expected, _ := url.Parse(oauth.RedirectURI)
	mux := http.NewServeMux()
	mux.HandleFunc(expected.Path, func(w http.ResponseWriter, r *http.Request) {
		// The browser addressed the registered redirect URI; judge the
		// callback as that, whatever port the forward came in on.
		u := *r.URL
		u.Scheme, u.Host = expected.Scheme, expected.Host
		code, clientID, err := oauth.ParseCallback(&u, state)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch {
		case errors.Is(err, oauth.ErrAuthorizationDenied):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "ChatGPT was not connected. You can close this window.\n")
			deliver(callback{err: err})
		case err != nil:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "%v.\n", err)
		default:
			_, _ = io.WriteString(w, "Signed in. You can close this window.\n")
			deliver(callback{code: code, clientID: clientID})
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}, nil
}
