package auth

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"time"
)

var (
	// ErrNoCredential means nobody has logged in yet.
	ErrNoCredential = errors.New("no credential: run `llm-proxy login`")
	// ErrExpired means the access token has expired and could not be refreshed.
	ErrExpired = errors.New("the access token has expired and could not be refreshed")
)

const (
	// refreshBackoff is how long a failed refresh waits before the next try.
	refreshBackoff = 30 * time.Second
	// refreshTimeout bounds one refresh. A refresh is never tied to the
	// request that started it: once the endpoint has rotated the refresh
	// token, giving up on the answer loses the new one.
	refreshTimeout = 30 * time.Second
)

// Refresher exchanges a credential's refresh token for a new credential.
type Refresher interface {
	Refresh(ctx context.Context, c Credential) (Credential, error)
}

// SourceConfig configures a Source.
type SourceConfig struct {
	Store     *FileStore
	Refresher Refresher
	// RefreshBefore is how long before expiry the token is refreshed.
	RefreshBefore time.Duration
	Now           func() time.Time
	Logger        *slog.Logger
}

// Source hands out the current access token, refreshing it before it expires.
// It is the one writer of the token file besides a login (ADR 0002).
type Source struct {
	store         *FileStore
	refresher     Refresher
	refreshBefore time.Duration
	now           func() time.Time
	log           *slog.Logger

	// mu is held across a refresh, so that a refresh token is used once and
	// requests arriving meanwhile wait for its result.
	mu          sync.Mutex
	cred        *Credential
	version     Version
	unsaved     bool
	lastFailure time.Time
}

// NewSource returns a source reading and refreshing the store's token file.
func NewSource(cfg SourceConfig) *Source {
	s := &Source{
		store:         cfg.Store,
		refresher:     cfg.Refresher,
		refreshBefore: cfg.RefreshBefore,
		now:           cfg.Now,
		log:           cfg.Logger,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s
}

// Token returns an access token to put on a request, refreshing it first when
// it is close to expiry.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintain(ctx)
	if s.cred == nil {
		return "", ErrNoCredential
	}
	if !s.now().Before(s.cred.ExpiresAt) {
		return "", ErrExpired
	}
	return s.cred.AccessToken, nil
}

// Maintain does what a request would do to the token, without a request: it
// is what the periodic check calls, so that no request waits for a refresh.
func (s *Source) Maintain(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintain(ctx)
}

// Run calls Maintain every interval until ctx is done.
func (s *Source) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.Maintain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Maintain(ctx)
		}
	}
}

func (s *Source) maintain(ctx context.Context) {
	s.reloadIfChanged()
	if s.cred == nil {
		return
	}
	if s.unsaved {
		s.save()
	}
	now := s.now()
	if now.Add(s.refreshBefore).Before(s.cred.ExpiresAt) {
		return
	}
	if !s.lastFailure.IsZero() && now.Sub(s.lastFailure) < refreshBackoff {
		return
	}
	s.refresh(ctx)
}

func (s *Source) reloadIfChanged() {
	if s.cred != nil {
		changed, err := s.store.Changed(s.version)
		if err != nil {
			s.log.Error("checking the token file failed", "path", s.store.Path(), "error", err)
			return
		}
		if !changed {
			return
		}
	}
	c, version, err := s.store.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		s.log.Error("reading the token file failed", "path", s.store.Path(), "error", err)
		return
	}
	if s.cred != nil {
		s.log.Info("the token file was replaced; using the new credential", "path", s.store.Path(), "expires_at", c.ExpiresAt)
	}
	s.cred, s.version, s.unsaved, s.lastFailure = &c, version, false, time.Time{}
}

func (s *Source) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	next, err := s.refresher.Refresh(ctx, *s.cred)
	if err != nil {
		s.lastFailure = s.now()
		s.log.Error("refreshing the access token failed; if it keeps failing, run `llm-proxy login` again",
			"error", err, "expires_at", s.cred.ExpiresAt)
		return
	}
	s.cred, s.lastFailure = &next, time.Time{}
	s.log.Info("refreshed the access token", "expires_at", next.ExpiresAt)
	s.save()
}

func (s *Source) save() {
	version, err := s.store.Save(*s.cred)
	if err != nil {
		// The old refresh token is spent; keep the new one in memory and
		// try writing again on the next check.
		s.unsaved = true
		s.log.Error("writing the token file failed; the refreshed credential lives only in memory until it is written",
			"path", s.store.Path(), "error", err)
		return
	}
	s.version, s.unsaved = version, false
}
