package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
)

var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// tokenServer is a fake of the token endpoint that records the form it got
// and answers with the given status and body.
func tokenServer(t *testing.T, status int, body map[string]any) (*httptest.Server, *url.Values) {
	t.Helper()
	got := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/oauth/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		*got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func newClient(issuer string) *Client {
	return &Client{Issuer: issuer, Now: func() time.Time { return fixedNow }}
}

func goodTokenResponse() map[string]any {
	return map[string]any{
		"access_token":  "access-secret-new",
		"refresh_token": "refresh-secret-new",
		"id_token":      "id-token",
		"expires_in":    3600,
		"scope":         "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
	}
}

func TestTheAuthorizeURLAsksForTheDirectTokenScope(t *testing.T) {
	raw := newClient("https://auth.example.com").AuthorizeURL(AuthorizeParams{
		AgentName: "llm-proxy",
		DeviceID:  "0F8FAD5B-D9CB-469F-A165-70867728950E",
		State:     "the-state",
		Nonce:     "the-nonce",
		Challenge: "the-challenge",
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://auth.example.com/api/accounts/authorize" {
		t.Errorf("endpoint = %s", raw)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             "dynamic_agent_client",
		"agent_name_hint":       "llm-proxy",
		"ext_agent_host_id":     "urn:uuid:0f8fad5b-d9cb-469f-a165-70867728950e",
		"response_type":         "code",
		"redirect_uri":          "http://127.0.0.1:1455/auth/callback",
		"resource":              "https://api.openai.com/v1",
		"scope":                 "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
		"state":                 "the-state",
		"nonce":                 "the-nonce",
		"code_challenge":        "the-challenge",
		"code_challenge_method": "S256",
	}
	for key, value := range want {
		if q.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, q.Get(key), value)
		}
	}
}

func TestThePKCEChallengeIsTheVerifiersHash(t *testing.T) {
	verifier, challenge := NewPKCE()
	sum := sha256.Sum256([]byte(verifier))
	if challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Errorf("challenge %q is not the S256 of %q", challenge, verifier)
	}
	if len(verifier) < 43 {
		t.Errorf("verifier %q is shorter than RFC 7636 allows", verifier)
	}
	again, _ := NewPKCE()
	if again == verifier {
		t.Error("two verifiers are the same")
	}
}

func TestTheCallbackGivesTheCodeAndTheIssuedClient(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1455/auth/callback?code=the-code&state=s1&client_id=issued")
	code, clientID, err := ParseCallback(u, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if code != "the-code" || clientID != "issued" {
		t.Errorf("code, client = %q, %q", code, clientID)
	}
}

func TestABadCallbackIsRejected(t *testing.T) {
	cases := map[string]string{
		"another state":     "http://127.0.0.1:1455/auth/callback?code=c&state=other&client_id=issued",
		"no code":           "http://127.0.0.1:1455/auth/callback?state=s1&client_id=issued",
		"no issued client":  "http://127.0.0.1:1455/auth/callback?code=c&state=s1",
		"an error":          "http://127.0.0.1:1455/auth/callback?error=access_denied&state=s1",
		"another path":      "http://127.0.0.1:1455/elsewhere?code=c&state=s1&client_id=issued",
		"another authority": "http://example.com:1455/auth/callback?code=c&state=s1&client_id=issued",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			u, _ := url.Parse(raw)
			if _, _, err := ParseCallback(u, "s1"); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestAnErrorInTheCallbackIsMarkedAsFinal(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1455/auth/callback?error=access_denied&state=s1")
	_, _, err := ParseCallback(u, "s1")
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Errorf("err = %v, want ErrAuthorizationDenied", err)
	}
}

func TestExchangingTheCodeSendsTheVerifierAndReturnsTheCredential(t *testing.T) {
	srv, form := tokenServer(t, http.StatusOK, goodTokenResponse())
	c, err := newClient(srv.URL).Exchange(context.Background(), "the-code", "the-verifier", "issued")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "issued",
		"code":          "the-code",
		"code_verifier": "the-verifier",
		"redirect_uri":  "http://127.0.0.1:1455/auth/callback",
		"resource":      "https://api.openai.com/v1",
	}
	for key, value := range want {
		if form.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, form.Get(key), value)
		}
	}
	if c.AccessToken != "access-secret-new" || c.RefreshToken != "refresh-secret-new" || c.ClientID != "issued" {
		t.Errorf("credential = %+v", c)
	}
	if !c.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Errorf("expires at %v, want an hour from now", c.ExpiresAt)
	}
}

func TestAGrantWithoutTheDirectScopeIsRefused(t *testing.T) {
	body := goodTokenResponse()
	body["scope"] = "openid profile email offline_access"
	srv, _ := tokenServer(t, http.StatusOK, body)
	if _, err := newClient(srv.URL).Exchange(context.Background(), "c", "v", "issued"); err == nil {
		t.Error("accepted a grant without chatgpt.tokens.use.direct")
	}
}

func TestAnExchangeWithoutAnIDTokenIsRefused(t *testing.T) {
	body := goodTokenResponse()
	delete(body, "id_token")
	srv, _ := tokenServer(t, http.StatusOK, body)
	if _, err := newClient(srv.URL).Exchange(context.Background(), "c", "v", "issued"); err == nil {
		t.Error("accepted a token response without an ID token")
	}
}

func TestRefreshingSendsTheRefreshTokenAndTheIssuedClient(t *testing.T) {
	body := goodTokenResponse()
	delete(body, "id_token")
	srv, form := tokenServer(t, http.StatusOK, body)
	old := auth.Credential{RefreshToken: "refresh-secret-old", ClientID: "issued"}
	c, err := newClient(srv.URL).Refresh(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     "issued",
		"refresh_token": "refresh-secret-old",
		"resource":      "https://api.openai.com/v1",
	}
	for key, value := range want {
		if form.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, form.Get(key), value)
		}
	}
	if c.RefreshToken != "refresh-secret-new" || c.ClientID != "issued" {
		t.Errorf("credential = %+v", c)
	}
}

func TestARefusedRefreshNamesTheErrorButNotTheBody(t *testing.T) {
	srv, _ := tokenServer(t, http.StatusBadRequest, map[string]any{
		"error":             "invalid_grant",
		"error_description": "refresh token refresh-secret-old was already used",
	})
	_, err := newClient(srv.URL).Refresh(context.Background(), auth.Credential{RefreshToken: "refresh-secret-old", ClientID: "issued"})
	if err == nil {
		t.Fatal("a refused refresh succeeded")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error %q does not name the status and the error code", err)
	}
	if strings.Contains(err.Error(), "refresh-secret-old") {
		t.Errorf("error %q carries the response body", err)
	}
}
