package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDoctorChecksOnlyLocalPrerequisites(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "native-ran")
	writeInstallFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\nprintf ran > "+shellQuote(marker)+"\nexit 99\n")
	t.Setenv("PATH", bin)

	t.Run("missing bwrap", func(t *testing.T) {
		var out bytes.Buffer
		if doctorProvider(context.Background(), &out, "codex", "linux") {
			t.Fatal("doctor accepted a missing bwrap")
		}
		if !strings.Contains(out.String(), "installed") || !strings.Contains(out.String(), "bwrap is missing") || !strings.Contains(out.String(), sandboxGuide) {
			t.Fatalf("doctor output=%q", out.String())
		}
	})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("doctor executed native Codex: %v", err)
	}

	t.Run("Darwin bypasses bwrap", func(t *testing.T) {
		var out bytes.Buffer
		if !doctorProvider(context.Background(), &out, "codex", "darwin") {
			t.Fatalf("Darwin doctor failed: %q", out.String())
		}
		if !strings.Contains(out.String(), "bwrap is not required") {
			t.Fatalf("doctor output=%q", out.String())
		}
	})
}

func TestDoctorRunsBeforeProfileStoreSetup(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\nexit 99\n")
	writeInstallFixture(t, filepath.Join(bin, "bwrap"), "#!/bin/sh\nexit 0\n")
	dataHome := filepath.Join(root, "missing-data-home")
	t.Setenv("PATH", bin)
	t.Setenv("XDG_DATA_HOME", dataHome)
	if code, err := execute(invocation{verb: "doctor", provider: "codex"}); err != nil || code != 0 {
		t.Fatalf("doctor code=%d err=%v", code, err)
	}
	if _, err := os.Stat(dataHome); !os.IsNotExist(err) {
		t.Fatalf("doctor initialized a profile store: %v", err)
	}
}

func TestDoctorLinuxSandboxReportsReadyBlockedAndTimeout(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\nexit 99\n")
	bwrap := filepath.Join(bin, "bwrap")
	t.Setenv("PATH", bin)

	t.Run("ready", func(t *testing.T) {
		child := filepath.Join(root, "ready-child")
		writeInstallFixture(t, bwrap, "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s' \"$!\" > "+shellQuote(child)+"\nexit 0\n")
		var out bytes.Buffer
		if !doctorProvider(context.Background(), &out, "codex", "linux") || !strings.Contains(out.String(), "sandbox ready") {
			t.Fatalf("doctor output=%q", out.String())
		}
		assertProbeChildStopped(t, child)
	})
	t.Run("denied namespaces", func(t *testing.T) {
		writeInstallFixture(t, bwrap, "#!/bin/sh\nexit 1\n")
		var out bytes.Buffer
		if doctorProvider(context.Background(), &out, "codex", "linux") || !strings.Contains(out.String(), "sandbox blocked") || !strings.Contains(out.String(), "Ubuntu 24.04") {
			t.Fatalf("doctor output=%q", out.String())
		}
	})
	t.Run("timeout kills descendants", func(t *testing.T) {
		child := filepath.Join(root, "bwrap-child")
		writeInstallFixture(t, bwrap, "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s' \"$!\" > "+shellQuote(child)+"\nwait\n")
		oldTimeout := doctorProbeTimeout
		doctorProbeTimeout = 80 * time.Millisecond
		t.Cleanup(func() { doctorProbeTimeout = oldTimeout })
		var out bytes.Buffer
		if doctorProvider(context.Background(), &out, "codex", "linux") || !strings.Contains(out.String(), "timed out") {
			t.Fatalf("doctor output=%q", out.String())
		}
		assertProbeChildStopped(t, child)
	})
}

func TestDoctorSignalCancelsBwrapGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("public doctor uses bwrap only on Linux")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	writeInstallFixture(t, filepath.Join(bin, "codex"), "#!/bin/sh\nexit 99\n")
	writeInstallFixture(t, filepath.Join(bin, "bwrap"), "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s' \"$!\" > "+shellQuote(child)+"\nwait\n")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCodatorDoctorSignalChild$")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CODATOR_DOCTOR_SIGNAL_CHILD=1"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForFile(t, child, cmd)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("doctor succeeded after SIGTERM")
	}
	assertProbeChildStopped(t, child)
}

func TestCodatorDoctorSignalChild(t *testing.T) {
	if os.Getenv("CODATOR_DOCTOR_SIGNAL_CHILD") == "1" {
		os.Exit(run([]string{"doctor", "codex"}))
	}
}

func assertProbeChildStopped(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for running, _ := processRunning(pid); running && time.Now().Before(deadline); running, _ = processRunning(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if running, err := processRunning(pid); err != nil || running {
		t.Fatalf("probe descendant is still running=%t err=%v", running, err)
	}
}
