package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreAccountPermissionsAndListing(t *testing.T) {
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("codex", "personal_a")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.root, filepath.Join(store.root, "codex"), filepath.Join(store.root, "codex", "personal_a"), account.NativeDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Errorf("%s mode = %04o, want 0700", path, info.Mode().Perm())
		}
	}
	accounts, err := store.Accounts("codex")
	if err != nil || len(accounts) != 1 || accounts[0].Name != "personal_a" || accounts[0].Err != nil {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
}

func TestStoreRejectsInsecureAndSymlinkState(t *testing.T) {
	dataHome := tempDataHome(t)
	root := filepath.Join(dataHome, "codator")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(dataHome); err == nil {
		t.Fatal("accepted group/world-accessible state directory")
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("insecure directory was changed: mode=%v err=%v", info, err)
	}

	linkHome := tempDataHome(t)
	target := filepath.Join(linkHome, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkHome, "codator")); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(linkHome); err == nil {
		t.Fatal("accepted a symlink state directory")
	}
}

func TestAccountLockIsNonblocking(t *testing.T) {
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAccount("claude", "work"); err != nil {
		t.Fatal(err)
	}
	first, err := store.Lock("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := store.Lock("claude", "work"); !errors.Is(err, ErrAccountBusy) {
		t.Fatalf("second lock error = %v, want ErrAccountBusy", err)
	}
}

func tempDataHome(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidLabel(t *testing.T) {
	for _, name := range []string{"a", "0", "personal_a-2", strings.Repeat("x", 48)} {
		if !validLabel(name) {
			t.Errorf("validLabel(%q) = false", name)
		}
	}
	for _, name := range []string{"", "-a", "_a", "has space", "a/b", "é", strings.Repeat("x", 49)} {
		if validLabel(name) {
			t.Errorf("validLabel(%q) = true", name)
		}
	}
}
