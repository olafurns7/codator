package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type probeSignalScope struct {
	ctx      context.Context
	signals  chan os.Signal
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	cancel   context.CancelFunc
}

func newProbeSignalScope() *probeSignalScope {
	ctx, cancel := context.WithCancel(context.Background())
	scope := &probeSignalScope{
		ctx:     ctx,
		signals: make(chan os.Signal, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		cancel:  cancel,
	}
	signal.Notify(scope.signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		defer close(scope.stopped)
		select {
		case <-scope.stop:
		case <-scope.signals:
			scope.cancel()
		}
	}()
	return scope
}

func (s *probeSignalScope) stopListening() bool {
	s.stopOnce.Do(func() {
		signal.Stop(s.signals)
		select {
		case <-s.signals:
			s.cancel()
		default:
		}
		close(s.stop)
		<-s.stopped
	})
	return s.ctx.Err() != nil
}

func findNative(provider string) (string, error) {
	path, err := exec.LookPath(provider)
	if err != nil {
		return "", fmt.Errorf("cannot find native %s CLI on PATH", provider)
	}
	return path, nil
}

// The lock fd survives exec, preserving the native CLI's terminal signals, job control, cwd, and exit status.
// ponytail: a native descendant may inherit the lock too; add a lock broker only if it causes stale busy accounts.
func execNative(lock *AccountLock, path string, args, env []string) error {
	if err := lock.InheritOnExec(); err != nil {
		return fmt.Errorf("prepare account lock: %w", err)
	}
	argv := make([]string, 1, len(args)+1)
	argv[0] = filepath.Base(path)
	argv = append(argv, args...)
	if err := syscall.Exec(path, argv, env); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return errors.New("native CLI executable disappeared before launch")
		}
		return fmt.Errorf("launch native CLI: %w", err)
	}
	return errors.New("native CLI exec returned unexpectedly")
}

func probeCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd
}

func finishProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = cmd.Wait()
}
