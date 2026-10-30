// Package assertion checks the Gated-Assertion header gated puts on the
// requests of a subject it authenticated, so that requests reaching the proxy
// without passing gated are refused (ADR 0004).
//
// It is written on the standard library: the algorithm is fixed to EdDSA
// (Ed25519), so a JOSE library would add little but a dependency.
package assertion

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header is where gated puts the assertion.
const Header = "Gated-Assertion"

const (
	algorithm = "EdDSA"
	jwsType   = "gated-assertion+jwt"
	clockSkew = 30 * time.Second
	// maxCache bounds how long fetched keys are used, whatever the response
	// says.
	maxCache = 5 * time.Minute
	// refetchInterval is how long after a fetch an unknown key, or a failed
	// fetch, waits before fetching again.
	refetchInterval = time.Minute
	fetchTimeout    = 10 * time.Second
)

// Config configures a Verifier.
type Config struct {
	// Issuer is gated's central authentication host, as an origin.
	Issuer string
	// JWKSURL is where gated publishes its keys. Never taken from an
	// assertion.
	JWKSURL string
	// Audience is this host's public host name. It is compared with `aud`;
	// the request's Host header is not.
	Audience        string
	AllowedSubjects []string
	HTTPClient      *http.Client
	Now             func() time.Time
	Logger          *slog.Logger
}

// Refusal is why an assertion was not accepted, with the status to answer.
// The reason never quotes the assertion.
type Refusal struct {
	Status int
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(status int, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Reason: fmt.Sprintf(format, args...)}
}

// Verifier checks assertions against one issuer's keys.
type Verifier struct {
	cfg Config

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	fetchedAt time.Time
	expiresAt time.Time
}

// New returns a verifier for cfg.
func New(cfg Config) *Verifier {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: fetchTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Verifier{cfg: cfg}
}

// Verify checks an assertion in the order gated documents and returns its
// subject. Steps 1 to 7 establish that gated authenticated the subject for
// this host; step 8 is whether this host lets the subject in.
func (v *Verifier) Verify(ctx context.Context, assertion string) (string, *Refusal) {
	// 1. There is an assertion.
	if assertion == "" {
		return "", refuse(http.StatusUnauthorized, "no assertion")
	}
	// 2. A JWS in compact form, of the algorithm and type fixed here.
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return "", refuse(http.StatusUnauthorized, "not a compact JWS")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Iat *float64        `json:"iat"`
		Exp *float64        `json:"exp"`
	}
	if !decodePart(parts[0], &header) || !decodePart(parts[1], &claims) {
		return "", refuse(http.StatusUnauthorized, "not a compact JWS")
	}
	if header.Alg != algorithm {
		return "", refuse(http.StatusUnauthorized, "algorithm %s is not %s", strconv.Quote(short(header.Alg)), algorithm)
	}
	if header.Typ != jwsType {
		return "", refuse(http.StatusUnauthorized, "type %s is not %s", strconv.Quote(short(header.Typ)), jwsType)
	}
	// 3. The key is in the configured JWKS.
	if header.Kid == "" {
		return "", refuse(http.StatusUnauthorized, "no key ID")
	}
	key := v.key(ctx, header.Kid)
	if key == nil {
		return "", refuse(http.StatusUnauthorized, "unknown key")
	}
	// 4. The key verifies the signature.
	signature, ok := decodeBase64(parts[2])
	if !ok || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return "", refuse(http.StatusUnauthorized, "bad signature")
	}
	// 5. The issuer is the configured one.
	if claims.Iss != v.cfg.Issuer {
		return "", refuse(http.StatusUnauthorized, "issuer %s is not accepted", strconv.Quote(short(claims.Iss)))
	}
	// 6. The audience is this host.
	if !hasAudience(claims.Aud, v.cfg.Audience) {
		return "", refuse(http.StatusUnauthorized, "the assertion is not for this host")
	}
	// 7. Not expired, and not issued too far in the future.
	now := v.cfg.Now()
	if claims.Exp == nil || unix(*claims.Exp).Add(clockSkew).Before(now) {
		return "", refuse(http.StatusUnauthorized, "expired")
	}
	if claims.Iat == nil || unix(*claims.Iat).Add(-clockSkew).After(now) {
		return "", refuse(http.StatusUnauthorized, "issued in the future")
	}
	// 8. The subject is allowed here.
	if !slices.Contains(v.cfg.AllowedSubjects, claims.Sub) {
		return "", refuse(http.StatusForbidden, "subject %s is not allowed", strconv.Quote(short(claims.Sub)))
	}
	return claims.Sub, nil
}

// short keeps a value quoted in a reason to a readable length.
func short(s string) string {
	if len(s) > 128 {
		return s[:128] + "..."
	}
	return s
}

func unix(seconds float64) time.Time {
	return time.Unix(0, int64(seconds*float64(time.Second)))
}

// decodeBase64 accepts unpadded base64url only, and only its one encoding of
// the bytes: a lenient decoder lets trailing characters through.
func decodeBase64(s string) ([]byte, bool) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, false
	}
	return data, true
}

func decodePart(s string, into any) bool {
	data, ok := decodeBase64(s)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(into); err != nil {
		return false
	}
	return !dec.More()
}

func hasAudience(raw json.RawMessage, audience string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == audience
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return slices.Contains(many, audience)
	}
	return false
}

// key returns the public key of the ID. The keys are fetched when the cache
// has expired, and once more for an unknown ID if the last fetch is a minute
// old or more. When they cannot be fetched, the cached keys go on being used.
// One fetch runs at a time: the lock is held across it.
func (v *Verifier) key(ctx context.Context, kid string) ed25519.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.cfg.Now()
	switch {
	case v.fetchedAt.IsZero() || !now.Before(v.expiresAt):
		v.fetch(ctx, now)
	case v.keys[kid] == nil && now.Sub(v.fetchedAt) >= refetchInterval:
		v.fetch(ctx, now)
	}
	return v.keys[kid]
}

func (v *Verifier) fetch(ctx context.Context, now time.Time) {
	v.fetchedAt = now
	keys, maxAge, err := v.get(ctx)
	if err != nil {
		v.cfg.Logger.Error("fetching gated's keys failed; using the cached ones", "url", v.cfg.JWKSURL, "error", err)
		v.expiresAt = now.Add(refetchInterval)
		return
	}
	v.keys = keys
	v.expiresAt = now.Add(min(maxAge, maxCache))
}

func (v *Verifier) get(ctx context.Context) (map[string]ed25519.PublicKey, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, 0, fmt.Errorf("not a JWKS: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range body.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		x, ok := decodeBase64(k.X)
		if !ok || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	return keys, maxAge(resp.Header.Get("Cache-Control")), nil
}

// maxAge reads max-age from Cache-Control; maxCache when there is none.
func maxAge(cacheControl string) time.Duration {
	for directive := range strings.SplitSeq(cacheControl, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(directive), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		seconds, err := strconv.Atoi(strings.Trim(value, `"`))
		if err != nil || seconds < 0 {
			continue
		}
		return time.Duration(seconds) * time.Second
	}
	return maxCache
}
