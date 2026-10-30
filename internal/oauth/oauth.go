// Package oauth speaks the "Sign in with ChatGPT" OAuth flow: a public client
// with PKCE, a client registered anew at every login, and a grant scoped for
// direct use against the OpenAI API (ADR 0003).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
)

const (
	// DefaultIssuer is where the authorize and token endpoints live.
	DefaultIssuer = "https://auth.openai.com"
	// Resource is the API the grant is for.
	Resource = "https://api.openai.com/v1"
	// RedirectURI is the callback the flow is registered with. It is fixed:
	// the browser is sent here whether or not anything listens.
	RedirectURI = "http://127.0.0.1:1455/auth/callback"
	// dynamicClientID asks for a client to be registered for this login; the
	// issued ID comes back on the callback.
	dynamicClientID = "dynamic_agent_client"
	// DirectScope is what lets the access token be sent to the API directly.
	DirectScope = "chatgpt.tokens.use.direct"
	scope       = "openid profile email offline_access resource.invoke " + DirectScope

	authorizePath = "/api/accounts/authorize"
	tokenPath     = "/api/accounts/oauth/token"
)

// ErrAuthorizationDenied means the callback carried an error: the person
// declined, or the authorization server refused. Trying again needs a new
// login.
var ErrAuthorizationDenied = errors.New("authorization was not granted")

// Client talks to the authorization server.
type Client struct {
	// Issuer is the authorization server's origin; DefaultIssuer when empty.
	Issuer     string
	HTTPClient *http.Client
	Now        func() time.Time
}

func (c *Client) issuer() string {
	if c.Issuer == "" {
		return DefaultIssuer
	}
	return strings.TrimRight(c.Issuer, "/")
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return c.HTTPClient
}

// AuthorizeParams are the per-login values of the authorize URL.
type AuthorizeParams struct {
	// AgentName is the name the login introduces itself with.
	AgentName string
	// DeviceID is the installation's UUID.
	DeviceID  string
	State     string
	Nonce     string
	Challenge string
}

// AuthorizeURL is the page the person opens in a browser.
func (c *Client) AuthorizeURL(p AuthorizeParams) string {
	q := url.Values{
		"client_id":             {dynamicClientID},
		"agent_name_hint":       {p.AgentName},
		"ext_agent_host_id":     {"urn:uuid:" + strings.ToLower(p.DeviceID)},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI},
		"resource":              {Resource},
		"scope":                 {scope},
		"state":                 {p.State},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {"S256"},
		"nonce":                 {p.Nonce},
	}
	return c.issuer() + authorizePath + "?" + q.Encode()
}

// NewPKCE returns a code verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string) {
	verifier = RandomValue()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomValue is 32 random bytes in base64url, for state and nonce.
func RandomValue() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ParseCallback reads the code and the issued client ID from the URL the
// browser was sent back to.
func ParseCallback(u *url.URL, state string) (code, clientID string, err error) {
	expected, _ := url.Parse(RedirectURI)
	if u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path {
		return "", "", fmt.Errorf("the callback URL must start with %s", RedirectURI)
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", "", fmt.Errorf("%w: %s", ErrAuthorizationDenied, e)
	}
	if q.Get("state") == "" {
		return "", "", errors.New("the callback URL has no state")
	}
	if q.Get("state") != state {
		return "", "", errors.New("the callback URL is for another login (state mismatch)")
	}
	code = q.Get("code")
	if code == "" {
		return "", "", errors.New("the callback URL has no authorization code")
	}
	clientID = strings.TrimSpace(q.Get("client_id"))
	if clientID == "" {
		return "", "", errors.New("the callback URL has no issued client ID")
	}
	return code, clientID, nil
}

// Exchange trades the authorization code for the first credential.
func (c *Client) Exchange(ctx context.Context, code, verifier, clientID string) (auth.Credential, error) {
	token, err := c.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {RedirectURI},
		"resource":      {Resource},
	})
	if err != nil {
		return auth.Credential{}, err
	}
	// The ID token is not used, but its presence is part of a good answer.
	if strings.TrimSpace(token.IDToken) == "" {
		return auth.Credential{}, errors.New("the token response has no ID token")
	}
	return c.credential(token, clientID)
}

// Refresh trades the credential's refresh token for a new credential. The
// old refresh token cannot be used again.
func (c *Client) Refresh(ctx context.Context, cred auth.Credential) (auth.Credential, error) {
	if strings.TrimSpace(cred.ClientID) == "" {
		return auth.Credential{}, errors.New("the credential has no issued client ID; run `llm-proxy login` again")
	}
	token, err := c.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cred.ClientID},
		"refresh_token": {cred.RefreshToken},
		"resource":      {Resource},
	})
	if err != nil {
		return auth.Credential{}, err
	}
	return c.credential(token, cred.ClientID)
}

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	ExpiresIn    float64 `json:"expires_in"`
	Scope        string  `json:"scope"`
}

// TokenError is a token endpoint's refusal. It carries the status and the
// error code only: the rest of the body is never quoted (ADR 0005).
type TokenError struct {
	Status int
	Code   string
}

func (e *TokenError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("the token endpoint answered %d", e.Status)
	}
	return fmt.Sprintf("the token endpoint answered %d (%s)", e.Status, e.Code)
}

func (c *Client) requestToken(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer()+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		return tokenResponse{}, &TokenError{Status: resp.StatusCode, Code: printable(refusal.Error)}
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return tokenResponse{}, errors.New("the token response is not JSON")
	}
	return token, nil
}

// printable keeps an error code short and on one line.
func printable(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (c *Client) credential(token tokenResponse, clientID string) (auth.Credential, error) {
	switch {
	case strings.TrimSpace(token.AccessToken) == "":
		return auth.Credential{}, errors.New("the token response has no access token")
	case strings.TrimSpace(token.RefreshToken) == "":
		return auth.Credential{}, errors.New("the token response has no refresh token")
	case token.ExpiresIn <= 0:
		return auth.Credential{}, errors.New("the token response has no lifetime")
	}
	scopes := strings.Fields(token.Scope)
	if !slices.Contains(scopes, DirectScope) {
		return auth.Credential{}, fmt.Errorf("the grant does not include %s", DirectScope)
	}
	return auth.Credential{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    c.now().Add(time.Duration(token.ExpiresIn * float64(time.Second))),
		ClientID:     clientID,
		Scopes:       scopes,
	}, nil
}
