// Package auth keeps the ChatGPT sign-in credential: the token file in the
// data directory (ADR 0002) and the source that refreshes it before it
// expires.
package auth

import (
	"fmt"
	"log/slog"
	"time"
)

// Credential is the OAuth grant for the subscription. It prints, logs and
// marshals with its tokens left out (ADR 0005); the token file is written
// from a separate type.
type Credential struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is when the access token actually expires.
	ExpiresAt time.Time
	// ClientID is the client issued at login, which refreshing needs.
	ClientID string
	Scopes   []string
}

func (c Credential) redacted() string {
	return fmt.Sprintf("Credential{AccessToken: <redacted>, RefreshToken: <redacted>, ExpiresAt: %s, ClientID: %q, Scopes: %q}",
		c.ExpiresAt.Format(time.RFC3339), c.ClientID, c.Scopes)
}

// Format prints the credential without its tokens, whatever the verb.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, c.redacted()) }

// LogValue logs the credential without its tokens.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Time("expires_at", c.ExpiresAt),
		slog.String("client_id", c.ClientID),
	)
}

// MarshalJSON leaves the tokens out, so that a credential passed to a JSON
// encoder by mistake does not carry them.
func (c Credential) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"expires_at":%q,"client_id":%q}`, c.ExpiresAt.Format(time.RFC3339), c.ClientID), nil
}
