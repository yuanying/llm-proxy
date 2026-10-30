package auth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	tokenFile    = "auth.json"
	deviceIDFile = "device-id"
	lockFile     = "serve.lock"
)

// FileStore reads and writes the token file of one data directory.
type FileStore struct {
	dir string
}

// NewFileStore returns the store of the data directory dir.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// Path is where the token file lives.
func (s *FileStore) Path() string { return filepath.Join(s.dir, tokenFile) }

// Version identifies one written state of the token file. A file replaced
// by rename is a different file, whatever its time stamp says.
type Version struct{ info fs.FileInfo }

// fileFormat is what the token file holds.
type fileFormat struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	ClientID     string    `json:"client_id"`
	Scopes       []string  `json:"scopes"`
}

// Load reads the token file. It returns an error matching fs.ErrNotExist when
// nobody has logged in yet.
func (s *FileStore) Load() (Credential, Version, error) {
	f, err := os.Open(s.Path())
	if err != nil {
		return Credential{}, Version{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Credential{}, Version{}, err
	}
	var stored fileFormat
	if err := json.NewDecoder(f).Decode(&stored); err != nil {
		// The decoder's message can quote the file; keep it out.
		return Credential{}, Version{}, fmt.Errorf("%s is not a token file", s.Path())
	}
	return Credential(stored), Version{info: info}, nil
}

// Save replaces the token file atomically: readers see the old content or the
// new, never a part of either (ADR 0002).
func (s *FileStore) Save(c Credential) (Version, error) {
	data, err := json.MarshalIndent(fileFormat(c), "", "  ")
	if err != nil {
		return Version{}, err
	}
	if err := writeFileAtomic(s.dir, tokenFile, append(data, '\n')); err != nil {
		return Version{}, err
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		return Version{}, err
	}
	return Version{info: info}, nil
}

// Changed reports whether the token file is no longer the one of v.
func (s *FileStore) Changed(v Version) (bool, error) {
	info, err := os.Stat(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return v.info != nil, nil
	}
	if err != nil {
		return false, err
	}
	return v.info == nil || !os.SameFile(v.info, info), nil
}

// DeviceID returns the installation's ID, creating it on first use. It stays
// when the token file is removed, so that a new login is the same
// installation (ADR 0003).
func (s *FileStore) DeviceID() (string, error) {
	path := filepath.Join(s.dir, deviceIDFile)
	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	id := newUUID()
	if err := writeFileAtomic(s.dir, deviceIDFile, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeFileAtomic writes a temporary file in dir and renames it over name.
// The temporary file is in the same directory because a rename across file
// systems is not atomic.
func writeFileAtomic(dir, name string, data []byte) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	// The rename is durable only once the directory is.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// LockDir takes the data directory for the one process that may refresh the
// token (ADR 0002). It fails when another process holds it.
func LockDir(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
