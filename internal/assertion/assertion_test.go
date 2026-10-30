package assertion

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testIssuer   = "https://auth.example.com"
	testAudience = "llm-proxy.example.com"
	testSubject  = "system:serviceaccount:agents:caller-a"
)

type key struct {
	kid  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newKey(t *testing.T) key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pub)
	return key{kid: base64.RawURLEncoding.EncodeToString(sum[:]), priv: priv, pub: pub}
}

func (k key) jwk() map[string]any {
	return map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(k.pub), "kid": k.kid, "alg": "EdDSA", "use": "sig"}
}

func b64json(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func (k key) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	signing := b64json(t, header) + "." + b64json(t, claims)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(signing)))
}

// jwksServer publishes the keys it holds and counts how often it was asked.
type jwksServer struct {
	*httptest.Server
	mu      sync.Mutex
	keys    []map[string]any
	fetches atomic.Int32
	down    atomic.Bool
}

func newJWKSServer(t *testing.T, keys ...key) *jwksServer {
	t.Helper()
	s := &jwksServer{}
	for _, k := range keys {
		s.keys = append(s.keys, k.jwk())
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		if s.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) publish(k key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k.jwk())
}

type fixture struct {
	t        *testing.T
	key      key
	jwks     *jwksServer
	now      time.Time
	verifier *Verifier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	k := newKey(t)
	f := &fixture{t: t, key: k, jwks: newJWKSServer(t, k), now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f.verifier = New(Config{
		Issuer:          testIssuer,
		JWKSURL:         f.jwks.URL,
		Audience:        testAudience,
		AllowedSubjects: []string{testSubject},
		Now:             func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) header() map[string]any {
	return map[string]any{"alg": "EdDSA", "typ": "gated-assertion+jwt", "kid": f.key.kid}
}

func (f *fixture) claims() map[string]any {
	return map[string]any{
		"iss": testIssuer, "sub": testSubject, "aud": testAudience,
		"iat": f.now.Unix(), "exp": f.now.Add(60 * time.Second).Unix(),
	}
}

func (f *fixture) good() string { return f.key.sign(f.t, f.header(), f.claims()) }

func TestAnAssertionFromGatedForAnAllowedSubjectPasses(t *testing.T) {
	f := newFixture(t)
	subject, err := f.verifier.Verify(context.Background(), f.good())
	if err != nil {
		t.Fatal(err)
	}
	if subject != testSubject {
		t.Errorf("subject = %q", subject)
	}
}

func TestRefusals(t *testing.T) {
	f := newFixture(t)
	other := newKey(t)
	with := func(change func(h, c map[string]any)) string {
		h, c := f.header(), f.claims()
		change(h, c)
		return f.key.sign(t, h, c)
	}
	good := f.good()
	parts := strings.Split(good, ".")
	cases := []struct {
		name      string
		assertion string
		status    int
	}{
		{"no assertion", "", http.StatusUnauthorized},
		{"not a JWS", "garbage", http.StatusUnauthorized},
		{"a padded part", parts[0] + "." + parts[1] + "=." + parts[2], http.StatusUnauthorized},
		{"another algorithm", with(func(h, _ map[string]any) { h["alg"] = "none" }), http.StatusUnauthorized},
		{"another type", with(func(h, _ map[string]any) { h["typ"] = "JWT" }), http.StatusUnauthorized},
		{"no key ID", with(func(h, _ map[string]any) { delete(h, "kid") }), http.StatusUnauthorized},
		{"an unknown key", other.sign(t, map[string]any{"alg": "EdDSA", "typ": "gated-assertion+jwt", "kid": other.kid}, f.claims()), http.StatusUnauthorized},
		{"signed by another key under a known ID", other.sign(t, f.header(), f.claims()), http.StatusUnauthorized},
		{"a changed body", parts[0] + "." + b64json(t, map[string]any{"iss": testIssuer, "sub": "system:serviceaccount:x:y", "aud": testAudience, "iat": f.now.Unix(), "exp": f.now.Add(time.Minute).Unix()}) + "." + parts[2], http.StatusUnauthorized},
		{"another issuer", with(func(_, c map[string]any) { c["iss"] = "https://evil.example.com" }), http.StatusUnauthorized},
		{"another host", with(func(_, c map[string]any) { c["aud"] = "shop.example.com" }), http.StatusUnauthorized},
		{"expired", with(func(_, c map[string]any) { c["exp"] = f.now.Add(-time.Minute).Unix() }), http.StatusUnauthorized},
		{"issued in the future", with(func(_, c map[string]any) { c["iat"] = f.now.Add(time.Minute).Unix() }), http.StatusUnauthorized},
		{"no expiry", with(func(_, c map[string]any) { delete(c, "exp") }), http.StatusUnauthorized},
		{"a subject not allowed here", with(func(_, c map[string]any) { c["sub"] = "system:serviceaccount:agents:other" }), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.verifier.Verify(context.Background(), tc.assertion)
			if err == nil {
				t.Fatal("accepted")
			}
			if err.Status != tc.status {
				t.Errorf("status = %d, want %d (%s)", err.Status, tc.status, err.Reason)
			}
		})
	}
}

func TestAnAudienceListIncludingThisHostPasses(t *testing.T) {
	f := newFixture(t)
	c := f.claims()
	c["aud"] = []string{"other.example.com", testAudience}
	if _, err := f.verifier.Verify(context.Background(), f.key.sign(t, f.header(), c)); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func TestTheKeysAreCachedBetweenRequests(t *testing.T) {
	f := newFixture(t)
	for range 5 {
		if _, err := f.verifier.Verify(context.Background(), f.good()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.jwks.fetches.Load(); n != 1 {
		t.Errorf("fetched the keys %d times, want 1", n)
	}
}

func TestANewKeyIsFetchedOnceItIsUsed(t *testing.T) {
	f := newFixture(t)
	if _, err := f.verifier.Verify(context.Background(), f.good()); err != nil {
		t.Fatal(err)
	}
	rotated := newKey(t)
	f.jwks.publish(rotated)
	assertion := rotated.sign(t, map[string]any{"alg": "EdDSA", "typ": "gated-assertion+jwt", "kid": rotated.kid}, f.claims())

	// Within a minute of the last fetch, an unknown key does not fetch again.
	if _, err := f.verifier.Verify(context.Background(), assertion); err == nil {
		t.Fatal("accepted a key not fetched yet")
	}
	f.now = f.now.Add(61 * time.Second)
	if _, err := f.verifier.Verify(context.Background(), rotated.sign(t, map[string]any{"alg": "EdDSA", "typ": "gated-assertion+jwt", "kid": rotated.kid}, f.claims())); err != nil {
		t.Errorf("refused the rotated key after a minute: %v", err)
	}
	if n := f.jwks.fetches.Load(); n != 2 {
		t.Errorf("fetched %d times, want 2", n)
	}
}

func TestCachedKeysKeepWorkingWhenTheIssuerIsDown(t *testing.T) {
	f := newFixture(t)
	if _, err := f.verifier.Verify(context.Background(), f.good()); err != nil {
		t.Fatal(err)
	}
	f.jwks.down.Store(true)
	f.now = f.now.Add(10 * time.Minute)
	if _, err := f.verifier.Verify(context.Background(), f.good()); err != nil {
		t.Errorf("refused with cached keys: %v", err)
	}
}

func TestWithoutKeysEverythingIsRefused(t *testing.T) {
	f := newFixture(t)
	f.jwks.down.Store(true)
	if _, err := f.verifier.Verify(context.Background(), f.good()); err == nil || err.Status != http.StatusUnauthorized {
		t.Errorf("err = %v, want a 401", err)
	}
}

func TestAReasonNeverQuotesTheAssertion(t *testing.T) {
	f := newFixture(t)
	c := f.claims()
	c["aud"] = "shop.example.com"
	assertion := f.key.sign(t, f.header(), c)
	_, err := f.verifier.Verify(context.Background(), assertion)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, part := range strings.Split(assertion, ".") {
		if strings.Contains(err.Reason, part) {
			t.Errorf("reason %q quotes the assertion", err.Reason)
		}
	}
}
