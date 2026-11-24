package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuanying/llm-proxy/internal/auth"
)

const (
	DefaultXAIIssuer = "https://auth.x.ai"
	// DefaultXAIClientID is the public subscription client used by pi-ai.
	DefaultXAIClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiScope           = "openid profile email offline_access grok-cli:access api:access"
	xaiDevicePath      = "/oauth2/device/code"
	xaiTokenPath       = "/oauth2/token"
	defaultDevicePoll  = 5 * time.Second
)

// XAIClient speaks the xAI subscription device and refresh grants.
type XAIClient struct {
	// Issuer is the authorization server's origin; DefaultXAIIssuer when empty.
	Issuer string
}

// DeviceLogin requests a device code, presents the browser instructions, and
// waits for the person to authorize. The machine's code never leaves OAuth.
func (c *XAIClient) DeviceLogin(ctx context.Context, clientID string, prompt func(verificationURI, userCode string)) (auth.Credential, error) {
	var device struct {
		DeviceCode      string  `json:"device_code"`
		UserCode        string  `json:"user_code"`
		VerificationURI string  `json:"verification_uri"`
		ExpiresIn       float64 `json:"expires_in"`
		Interval        float64 `json:"interval"`
	}
	if err := c.post(ctx, xaiDevicePath, url.Values{
		"client_id": {clientID},
		"scope":     {xaiScope},
		"referrer":  {"llm-proxy"},
	}, &device); err != nil {
		return auth.Credential{}, fmt.Errorf("requesting xAI device authorization: %w", err)
	}
	uri, err := url.Parse(device.VerificationURI)
	if err != nil || uri.Scheme != "https" || uri.Hostname() == "" || uri.User != nil {
		return auth.Credential{}, errors.New("xAI returned an invalid verification URL")
	}
	lifetime := oauthDuration(device.ExpiresIn)
	if strings.TrimSpace(device.DeviceCode) == "" || !validUserCode(device.UserCode) || lifetime == 0 {
		return auth.Credential{}, errors.New("xAI returned an incomplete device authorization")
	}
	interval := oauthDuration(device.Interval)
	if interval == 0 {
		interval = defaultDevicePoll
	}
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	prompt(uri.String(), device.UserCode)
	return c.poll(ctx, clientID, device.DeviceCode, interval)
}

func (c *XAIClient) poll(ctx context.Context, clientID, deviceCode string, interval time.Duration) (auth.Credential, error) {
	form := url.Values{
		"client_id":   {clientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return auth.Credential{}, fmt.Errorf("waiting for xAI device authorization: %w", ctx.Err())
		case <-timer.C:
		}
		var token tokenResponse
		err := c.post(ctx, xaiTokenPath, form, &token)
		if err == nil {
			return xaiCredential(token, auth.Credential{ClientID: clientID})
		}
		var refusal *TokenError
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval = min(interval, math.MaxInt64-defaultDevicePoll) + defaultDevicePoll
				continue
			}
		}
		return auth.Credential{}, fmt.Errorf("waiting for xAI device authorization: %w", err)
	}
}

// Refresh uses the public client ID saved at login. xAI may retain the
// current refresh token rather than return a new one.
func (c *XAIClient) Refresh(ctx context.Context, previous auth.Credential) (auth.Credential, error) {
	if strings.TrimSpace(previous.ClientID) == "" || strings.TrimSpace(previous.RefreshToken) == "" {
		return auth.Credential{}, errors.New("the xAI credential cannot be refreshed; run `llm-proxy login` again")
	}
	var token tokenResponse
	err := c.post(ctx, xaiTokenPath, url.Values{
		"client_id":     {previous.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {previous.RefreshToken},
	}, &token)
	if err != nil {
		return auth.Credential{}, err
	}
	return xaiCredential(token, previous)
}

func xaiCredential(token tokenResponse, previous auth.Credential) (auth.Credential, error) {
	lifetime := oauthDuration(token.ExpiresIn)
	refresh := token.RefreshToken
	if refresh == "" {
		refresh = previous.RefreshToken
	}
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(refresh) == "" || lifetime == 0 {
		return auth.Credential{}, errors.New("xAI returned an incomplete token response")
	}
	scopes := strings.Fields(token.Scope)
	if len(scopes) == 0 {
		scopes = previous.Scopes
	}
	return auth.Credential{
		Provider:     auth.ProviderXAI,
		AccessToken:  token.AccessToken,
		RefreshToken: refresh,
		ExpiresAt:    time.Now().Add(lifetime),
		ClientID:     previous.ClientID,
		Scopes:       scopes,
	}, nil
}

func oauthDuration(seconds float64) time.Duration {
	if seconds <= 0 || seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func validUserCode(code string) bool {
	return code != "" && strings.IndexFunc(code, func(r rune) bool { return r < '!' || r > '~' }) == -1
}

func (c *XAIClient) post(ctx context.Context, path string, form url.Values, result any) error {
	issuer := c.Issuer
	if issuer == "" {
		issuer = DefaultXAIIssuer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(issuer, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("invalid xAI OAuth endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the xAI OAuth endpoint did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("could not read the xAI OAuth response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		// Never quote arbitrary strings returned by the endpoint.
		switch refusal.Error {
		case "authorization_pending", "slow_down", "access_denied", "authorization_denied", "expired_token", "invalid_grant", "invalid_client", "invalid_scope":
			return &TokenError{Status: resp.StatusCode, Code: refusal.Error}
		default:
			return &TokenError{Status: resp.StatusCode}
		}
	}
	if err := json.Unmarshal(body, result); err != nil {
		return errors.New("invalid xAI OAuth response")
	}
	return nil
}
