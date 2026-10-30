package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleCredential() Credential {
	return Credential{
		AccessToken:  "access-secret-1",
		RefreshToken: "refresh-secret-1",
		ExpiresAt:    time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		ClientID:     "client-issued",
		Scopes:       []string{"openid", "chatgpt.tokens.use.direct"},
	}
}

func TestSavedCredentialIsReadBackUnchanged(t *testing.T) {
	store := NewFileStore(t.TempDir())
	want := sampleCredential()
	if _, err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
		!got.ExpiresAt.Equal(want.ExpiresAt) || got.ClientID != want.ClientID ||
		strings.Join(got.Scopes, " ") != strings.Join(want.Scopes, " ") {
		t.Errorf("loaded %#v, want the saved credential", got)
	}
}

func TestLoadingWithoutAFileReportsThatThereIsNone(t *testing.T) {
	_, _, err := NewFileStore(t.TempDir()).Load()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestTheTokenFileIsReadableByItsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := NewFileStore(dir)
	if _, err := store.Save(sampleCredential()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %v, want 0700", perm)
	}
}

func TestSavingLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	for range 3 {
		if _, err := store.Save(sampleCredential()); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "auth.json" {
			t.Errorf("left behind %q", entry.Name())
		}
	}
}

func TestAReaderNeverSeesAHalfWrittenFile(t *testing.T) {
	store := NewFileStore(t.TempDir())
	if _, err := store.Save(sampleCredential()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var failures []string
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(store.Path())
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			if !json.Valid(data) {
				failures = append(failures, fmt.Sprintf("invalid JSON: %q", data))
			}
		}
	}()
	for i := range 200 {
		c := sampleCredential()
		c.AccessToken = strings.Repeat(fmt.Sprintf("a%d", i), 50+i)
		if _, err := store.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
	if len(failures) > 0 {
		t.Errorf("a reader saw %d broken reads, first: %s", len(failures), failures[0])
	}
}

func TestAFailedSaveLeavesThePreviousFileIntact(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	store := NewFileStore(dir)
	if _, err := store.Save(sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	next := sampleCredential()
	next.AccessToken = "access-secret-2"
	if _, err := store.Save(next); err == nil {
		t.Fatal("saving into a read-only directory succeeded")
	}
	got, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access-secret-1" {
		t.Errorf("access token = %q, want the previous one", got.AccessToken)
	}
}

func TestReplacingTheFileIsNoticed(t *testing.T) {
	store := NewFileStore(t.TempDir())
	version, err := store.Save(sampleCredential())
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Changed(version); err != nil || changed {
		t.Fatalf("changed = %v, %v right after saving", changed, err)
	}
	if _, err := NewFileStore(store.dir).Save(sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Changed(version); err != nil || !changed {
		t.Errorf("changed = %v, %v after another writer replaced the file", changed, err)
	}
}

func TestTheDeviceIDIsAUUIDKeptAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := NewFileStore(dir).DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first) {
		t.Errorf("device ID %q is not a version 4 UUID", first)
	}
	second, err := NewFileStore(dir).DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("device ID changed from %q to %q", first, second)
	}
}

func TestOnlyOneProcessHoldsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDir(dir); err == nil {
		t.Error("a second lock on the same directory succeeded")
	}
	unlock()
	again, err := LockDir(dir)
	if err != nil {
		t.Fatalf("locking after unlock: %v", err)
	}
	again()
}

func TestACredentialNeverPrintsItsSecrets(t *testing.T) {
	c := sampleCredential()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("credential", "value", c, "pointer", &c)
	encoded, _ := json.Marshal(c)
	outputs := []string{
		fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprintf("%v", &c),
		string(encoded), logs.String(),
	}
	for _, out := range outputs {
		if strings.Contains(out, c.AccessToken) || strings.Contains(out, c.RefreshToken) {
			t.Errorf("output carries a token: %s", out)
		}
	}
}
