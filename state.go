package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

var ErrAccountBusy = errors.New("account is busy")

type Account struct {
	Provider  string
	Name      string
	NativeDir string
	Err       error
}

type Store struct{ root string }

func storeFromEnv() (*Store, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home := os.Getenv("HOME")
		if home == "" || !filepath.IsAbs(home) {
			return nil, errors.New("HOME must be set to an absolute path when XDG_DATA_HOME is unset")
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	if !filepath.IsAbs(dataHome) {
		return nil, errors.New("XDG_DATA_HOME must be absolute")
	}
	return newStore(dataHome)
}

func newStore(dataHome string) (*Store, error) {
	if !filepath.IsAbs(dataHome) {
		return nil, errors.New("data home must be absolute")
	}
	root := filepath.Join(filepath.Clean(dataHome), "codator")
	dir, err := openPathRoot(root, true, true)
	if err != nil {
		return nil, fmt.Errorf("secure state directory: %w", err)
	}
	dir.Close()
	return &Store{root: root}, nil
}

func (s *Store) EnsureAccount(provider, name string) (Account, error) {
	if !validProvider(provider) || !validLabel(name) {
		return Account{}, errors.New("invalid provider or account name")
	}
	dir, err := s.accountRoot(provider, name, true)
	if err != nil {
		return Account{}, err
	}
	defer dir.Close()
	native, err := openSubRoot(dir, "native", true)
	if err != nil {
		return Account{}, fmt.Errorf("secure native profile: %w", err)
	}
	native.Close()
	return Account{Provider: provider, Name: name, NativeDir: filepath.Join(s.root, provider, name, "native")}, nil
}

func (s *Store) Accounts(provider string) ([]Account, error) {
	if !validProvider(provider) {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	root, err := s.providerRoot(provider, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open provider state: %w", err)
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open provider state: %w", err)
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, fmt.Errorf("read provider state: %w", err)
	}
	accounts := make([]Account, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !validLabel(name) {
			continue
		}
		dir, err := openSubRoot(root, name, false)
		if err != nil {
			accounts = append(accounts, Account{Provider: provider, Name: name, Err: err})
			continue
		}
		native, err := openSubRoot(dir, "native", false)
		dir.Close()
		if err != nil {
			accounts = append(accounts, Account{Provider: provider, Name: name, Err: fmt.Errorf("unsafe native profile: %w", err)})
			continue
		}
		native.Close()
		accounts = append(accounts, Account{Provider: provider, Name: name, NativeDir: filepath.Join(s.root, provider, name, "native")})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts, nil
}

func (s *Store) Lock(provider, name string) (*AccountLock, error) {
	dir, err := s.accountRoot(provider, name, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := openPrivateFile(dir, ".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open account lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAccountBusy
		}
		return nil, fmt.Errorf("lock account: %w", err)
	}
	return &AccountLock{file}, nil
}

func (s *Store) accountRoot(provider, name string, create bool) (*os.Root, error) {
	if !validProvider(provider) || !validLabel(name) {
		return nil, errors.New("invalid provider or account name")
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, fmt.Errorf("open state directory: %w", err)
	}
	providerRoot, err := openSubRoot(root, provider, create)
	root.Close()
	if err != nil {
		return nil, fmt.Errorf("open provider directory: %w", err)
	}
	account, err := openSubRoot(providerRoot, name, create)
	providerRoot.Close()
	if err != nil {
		return nil, fmt.Errorf("open account directory: %w", err)
	}
	return account, nil
}

func (s *Store) providerRoot(provider string, create bool) (*os.Root, error) {
	if !validProvider(provider) {
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	dir, err := openSubRoot(root, provider, create)
	root.Close()
	return dir, err
}

type AccountLock struct{ file *os.File }

func (l *AccountLock) InheritOnExec() error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, l.file.Fd(), syscall.F_SETFD, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func (l *AccountLock) Close() error {
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func openPrivateFile(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && flags&os.O_CREATE != 0 {
		file, createErr := root.OpenFile(name, flags|os.O_EXCL, mode)
		if createErr != nil {
			return nil, createErr
		}
		if err := file.Chmod(mode); err != nil {
			file.Close()
			return nil, err
		}
		return file, nil
	}
	if err != nil {
		return nil, err
	}
	if err := checkPrivateFile(info, mode); err != nil {
		return nil, err
	}
	return root.OpenFile(name, flags&^os.O_CREATE, 0)
}

func checkPrivateFile(info os.FileInfo, mode os.FileMode) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != os.Getuid() || info.Mode().Perm() != mode {
		return errors.New("must be a regular file owned by this user with private permissions")
	}
	return nil
}

func openSubRoot(parent *os.Root, name string, create bool) (*os.Root, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return nil, errors.New("invalid directory component")
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPrivateDir(info); err != nil {
		return nil, err
	}
	return parent.OpenRoot(name)
}

func openPathRoot(path string, create, privateFinal bool) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	path = filepath.Clean(path)
	if path == string(os.PathSeparator) {
		return nil, errors.New("state directory cannot be filesystem root")
	}
	current := string(os.PathSeparator)
	parts := strings.Split(strings.TrimPrefix(path, current), current)
	for i, part := range parts {
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := os.Mkdir(next, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
			info, err = os.Lstat(next)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("%q is not a real directory", next)
		}
		if i == len(parts)-1 && privateFinal {
			if err := checkPrivateDir(info); err != nil {
				return nil, fmt.Errorf("%q: %w", next, err)
			}
		} else if err := checkPathAncestor(info); err != nil {
			return nil, fmt.Errorf("unsafe parent %q: %w", next, err)
		}
		current = next
	}
	return os.OpenRoot(path)
}

func checkPrivateDir(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != os.Getuid() || stat.Mode&07777 != 0700 {
		return fmt.Errorf("must be owned by this user with mode 0700")
	}
	return nil
}

func checkPathAncestor(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != os.Getuid() && stat.Uid != 0 {
		return errors.New("directory is not owned by this user or root")
	}
	mode := stat.Mode & 07777
	if mode&0022 != 0 && mode&01000 == 0 {
		return fmt.Errorf("directory mode %04o is writable by other users", mode)
	}
	return nil
}

func validProvider(provider string) bool { return provider == "codex" || provider == "claude" }

func validLabel(name string) bool {
	if len(name) < 1 || len(name) > 48 || !asciiAlnum(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !asciiAlnum(name[i]) && name[i] != '_' && name[i] != '-' {
			return false
		}
	}
	return true
}

func asciiAlnum(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
