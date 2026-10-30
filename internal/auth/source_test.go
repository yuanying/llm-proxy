package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRefresher hands out a new pair for each refresh token it has issued and
// refuses any refresh token used a second time, as the real endpoint does.
type fakeRefresher struct {
	mu       sync.Mutex
	now      func() time.Time
	lifetime time.Duration
	issued   int
	used     map[string]int
	fail     error
	calls    atomic.Int32
	delay    time.Duration
}

func newFakeRefresher(now func() time.Time) *fakeRefresher {
	return &fakeRefresher{now: now, lifetime: time.Hour, used: map[string]int{}}
}

func (f *fakeRefresher) Refresh(_ context.Context, c Credential) (Credential, error) {
	f.calls.Add(1)
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return Credential{}, f.fail
	}
	f.used[c.RefreshToken]++
	if f.used[c.RefreshToken] > 1 {
		return Credential{}, errors.New("refresh token reused")
	}
	f.issued++
	return Credential{
		AccessToken:  fmt.Sprintf("access-secret-%d", f.issued+1),
		RefreshToken: fmt.Sprintf("refresh-secret-%d", f.issued+1),
		ExpiresAt:    f.now().Add(f.lifetime),
		ClientID:     c.ClientID,
		Scopes:       c.Scopes,
	}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sourceFixture struct {
	store     *FileStore
	clock     *clock
	refresher *fakeRefresher
	source    *Source
	logs      *bytes.Buffer
}

// newSourceFixture saves a credential expiring after expiresIn and builds a
// source that refreshes ten minutes ahead.
func newSourceFixture(t *testing.T, expiresIn time.Duration) *sourceFixture {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	store := NewFileStore(t.TempDir())
	c := sampleCredential()
	c.ExpiresAt = clk.Now().Add(expiresIn)
	if _, err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	refresher := newFakeRefresher(clk.Now)
	logs := &bytes.Buffer{}
	source := NewSource(SourceConfig{
		Store:         store,
		Refresher:     refresher,
		RefreshBefore: 10 * time.Minute,
		Now:           clk.Now,
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
	})
	return &sourceFixture{store: store, clock: clk, refresher: refresher, source: source, logs: logs}
}

func TestATokenFarFromExpiryIsUsedAsIs(t *testing.T) {
	f := newSourceFixture(t, time.Hour)
	token, err := f.source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-secret-1" {
		t.Errorf("token = %q, want the stored one", token)
	}
	if n := f.refresher.calls.Load(); n != 0 {
		t.Errorf("refreshed %d times, want none", n)
	}
}

func TestATokenCloseToExpiryIsRefreshedBeforeUse(t *testing.T) {
	f := newSourceFixture(t, 5*time.Minute)
	token, err := f.source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-secret-2" {
		t.Errorf("token = %q, want the refreshed one", token)
	}
	saved, _, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "access-secret-2" || saved.RefreshToken != "refresh-secret-2" {
		t.Errorf("file holds %q / %q, want the refreshed pair", saved.AccessToken, saved.RefreshToken)
	}
}

func TestTheBackgroundCheckRefreshesWithoutARequest(t *testing.T) {
	f := newSourceFixture(t, time.Hour)
	f.source.Maintain(context.Background())
	if n := f.refresher.calls.Load(); n != 0 {
		t.Fatalf("refreshed %d times an hour before expiry", n)
	}
	f.clock.Advance(55 * time.Minute)
	f.source.Maintain(context.Background())
	if n := f.refresher.calls.Load(); n != 1 {
		t.Fatalf("refreshed %d times five minutes before expiry, want 1", n)
	}
	saved, _, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "access-secret-2" {
		t.Errorf("file holds %q, want the refreshed token", saved.AccessToken)
	}
}

func TestConcurrentRequestsUseTheRefreshTokenOnce(t *testing.T) {
	f := newSourceFixture(t, time.Minute)
	f.refresher.delay = 20 * time.Millisecond
	var wg sync.WaitGroup
	tokens := make([]string, 20)
	errs := make([]error, 20)
	for i := range tokens {
		wg.Go(func() { tokens[i], errs[i] = f.source.Token(context.Background()) })
	}
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if tokens[i] != "access-secret-2" {
			t.Errorf("request %d got %q, want the one refreshed token", i, tokens[i])
		}
	}
	if n := f.refresher.calls.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1", n)
	}

	// The next refresh goes out with the new refresh token, not the used one.
	f.clock.Advance(55 * time.Minute)
	if _, err := f.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	for token, uses := range f.refresher.used {
		if uses != 1 {
			t.Errorf("refresh token %q used %d times", token, uses)
		}
	}
}

func TestACancelledRequestDoesNotAbandonARefresh(t *testing.T) {
	f := newSourceFixture(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.source.Token(ctx); err != nil {
		t.Fatalf("token with a cancelled request: %v", err)
	}
	saved, _, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "refresh-secret-2" {
		t.Errorf("file holds refresh token %q, want the new one", saved.RefreshToken)
	}
}

func TestAFailedRefreshKeepsServingAValidToken(t *testing.T) {
	f := newSourceFixture(t, 5*time.Minute)
	f.refresher.fail = errors.New("upstream said no")
	token, err := f.source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-secret-1" {
		t.Errorf("token = %q, want the still valid one", token)
	}
	// Another request right away does not try again.
	if _, err := f.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.refresher.calls.Load(); n != 1 {
		t.Errorf("refreshed %d times within the back-off, want 1", n)
	}
	f.clock.Advance(31 * time.Second)
	if _, err := f.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.refresher.calls.Load(); n != 2 {
		t.Errorf("refreshed %d times after the back-off, want 2", n)
	}
}

func TestAnExpiredTokenThatCannotBeRefreshedIsRefused(t *testing.T) {
	f := newSourceFixture(t, -time.Minute)
	f.refresher.fail = errors.New("upstream said no")
	if _, err := f.source.Token(context.Background()); !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestWithoutALoginThereIsNoToken(t *testing.T) {
	store := NewFileStore(t.TempDir())
	source := NewSource(SourceConfig{Store: store, Refresher: newFakeRefresher(time.Now)})
	if _, err := source.Token(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
}

func TestALoginWrittenWhileServingIsPickedUp(t *testing.T) {
	f := newSourceFixture(t, time.Hour)
	if _, err := f.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := sampleCredential()
	c.AccessToken = "access-from-login"
	c.ExpiresAt = f.clock.Now().Add(time.Hour)
	if _, err := NewFileStore(f.store.dir).Save(c); err != nil {
		t.Fatal(err)
	}
	token, err := f.source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-from-login" {
		t.Errorf("token = %q, want the one the login wrote", token)
	}
}

func TestRefreshingLogsNoToken(t *testing.T) {
	f := newSourceFixture(t, time.Minute)
	if _, err := f.source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Hour)
	f.refresher.fail = errors.New("upstream said no")
	_, _ = f.source.Token(context.Background())
	logs := f.logs.String()
	if logs == "" {
		t.Fatal("nothing was logged")
	}
	for _, secret := range []string{"access-secret-1", "refresh-secret-1", "access-secret-2", "refresh-secret-2"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs carry %q: %s", secret, logs)
		}
	}
}
