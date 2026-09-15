package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var systemPS, systemPSErr = exec.LookPath("ps")

func TestCodatorSignalChild(t *testing.T) {
	if os.Getenv("CODATOR_SIGNAL_CHILD") == "1" {
		os.Exit(run([]string{"codex"}))
	}
}

func TestSignalDuringLaunchCancelsProbeGroup(t *testing.T) {
	for _, test := range []struct {
		name string
		sig  syscall.Signal
	}{{"SIGINT", syscall.SIGINT}, {"SIGTERM", syscall.SIGTERM}} {
		t.Run(test.name, func(t *testing.T) {
			dataHome := tempDataHome(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a-blocked", "b-other"} {
				if _, err := store.EnsureAccount("codex", name); err != nil {
					t.Fatal(err)
				}
			}

			root := t.TempDir()
			binDir, eventDir := filepath.Join(root, "bin"), filepath.Join(root, "events")
			if err := os.Mkdir(binDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(eventDir, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "blocked.json")
			stub := filepath.Join(binDir, "codex")
			script := `#!/bin/sh
profile=${CODEX_HOME%/native}
profile=${profile##*/}
case "$*" in
  *app-server*)
    printf probe > "$CODATOR_SIGNAL_EVENTS/probe-$profile"
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
        *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","planType":"plus"}}}' ;;
        *'"method":"account/rateLimits/read"'*)
          if [ "$profile" = a-blocked ]; then
            /bin/sleep 120 &
            child=$!
            printf '%s %s %s\n' "$$" "$child" "$$" > "$CODATOR_SIGNAL_MARKER"
            wait
          else
            printf '%s\n' '{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":30,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}'
          fi
          ;;
      esac
    done
    ;;
  *) printf launch > "$CODATOR_SIGNAL_EVENTS/launch-$profile" ;;
esac
`
			if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}

			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorSignalChild$")
			cmd.Dir = root
			cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
				"CODATOR_SIGNAL_CHILD":  "1",
				"CODATOR_SIGNAL_EVENTS": eventDir,
				"CODATOR_SIGNAL_MARKER": marker,
				"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"XDG_DATA_HOME":         dataHome,
			})
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			var probeGroup int
			finished := false
			t.Cleanup(func() {
				if !finished {
					_ = cmd.Process.Kill()
					<-wait
				}
				if probeGroup != 0 {
					_ = syscall.Kill(-probeGroup, syscall.SIGKILL)
				}
			})

			startupDeadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case err := <-wait:
					finished = true
					t.Fatalf("wrapper exited before probe blocked: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				default:
				}
				if time.Now().After(startupDeadline) {
					t.Fatal("fake Codex probe did not block")
				}
				time.Sleep(5 * time.Millisecond)
			}
			pids, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(string(pids))
			if len(fields) != 3 {
				t.Fatalf("invalid blocked-process marker: %q", pids)
			}
			probePID, err := strconv.Atoi(fields[0])
			if err != nil {
				t.Fatal(err)
			}
			childPID, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatal(err)
			}
			probeGroup, err = strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			if probeGroup != probePID {
				t.Fatalf("probe is outside its own process group: pid=%d pgid=%d", probePID, probeGroup)
			}
			for _, pid := range []int{probePID, childPID} {
				process, found, err := psProcess(pid)
				if err != nil || !found || !processLive(process) || process.pgid != probeGroup {
					t.Fatalf("fake probe process %d = %#v found=%t err=%v, want live pgid %d", pid, process, found, err, probeGroup)
				}
			}
			if err := cmd.Process.Signal(test.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-wait:
				finished = true
				if err == nil {
					t.Fatalf("wrapper returned success after %s; it may have selected another account", test.name)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("wrapper did not exit promptly after %s", test.name)
			}

			groupDeadline := time.Now().Add(3 * time.Second)
			members, membersErr := liveProcessGroupMembers(probeGroup)
			for membersErr == nil && len(members) != 0 && time.Now().Before(groupDeadline) {
				time.Sleep(10 * time.Millisecond)
				members, membersErr = liveProcessGroupMembers(probeGroup)
			}
			if membersErr != nil {
				t.Fatalf("%s could not inspect probe process group: %v", test.name, membersErr)
			}
			if len(members) != 0 {
				t.Fatalf("%s left probe process-group members alive: %v", test.name, members)
			}
			if lock, err := store.Lock("codex", "a-blocked"); err != nil {
				t.Fatalf("%s did not release profile lock: %v", test.name, err)
			} else {
				lock.Close()
			}
			for _, event := range []string{"probe-b-other", "launch-a-blocked", "launch-b-other"} {
				if _, err := os.Stat(filepath.Join(eventDir, event)); err == nil {
					t.Fatalf("%s continued selection after cancellation: event %s exists", test.name, event)
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cannot inspect event %s: %v", event, err)
				}
			}
		})
	}
}

type psProcessState struct {
	pid   int
	pgid  int
	state string
}

func liveProcessGroupMembers(pgid int) ([]int, error) {
	processes, err := psProcesses("-ax")
	if err != nil {
		return nil, err
	}
	var members []int
	for _, process := range processes {
		if process.pgid == pgid && processLive(process) {
			members = append(members, process.pid)
		}
	}
	return members, nil
}

func psProcess(pid int) (psProcessState, bool, error) {
	if systemPSErr != nil {
		return psProcessState{}, false, systemPSErr
	}
	output, err := exec.Command(systemPS, "-p", strconv.Itoa(pid), "-o", "pid=", "-o", "pgid=", "-o", "stat=").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return psProcessState{}, false, nil
		}
		return psProcessState{}, false, err
	}
	processes, err := parsePSProcesses(output)
	if err != nil {
		return psProcessState{}, false, err
	}
	for _, process := range processes {
		if process.pid == pid {
			return process, true, nil
		}
	}
	return psProcessState{}, false, nil
}

func psProcesses(args ...string) ([]psProcessState, error) {
	if systemPSErr != nil {
		return nil, systemPSErr
	}
	args = append(args, "-o", "pid=", "-o", "pgid=", "-o", "stat=")
	output, err := exec.Command(systemPS, args...).Output()
	if err != nil {
		return nil, err
	}
	return parsePSProcesses(output)
}

func parsePSProcesses(output []byte) ([]psProcessState, error) {
	var processes []psProcessState
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ps output %q", line)
		}
		pid, pidErr := strconv.Atoi(fields[0])
		pgid, pgidErr := strconv.Atoi(fields[1])
		if pidErr != nil || pgidErr != nil {
			return nil, fmt.Errorf("unexpected ps output %q", line)
		}
		processes = append(processes, psProcessState{pid: pid, pgid: pgid, state: fields[2]})
	}
	return processes, nil
}

func processLive(process psProcessState) bool {
	return process.state != "" && process.state[0] != 'Z' && process.state[0] != 'X'
}

func TestProbeDeadlineKillsShimAndGrandchild(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("codex", "probe")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.Chmod(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(binDir, "child.pid")
	shim := filepath.Join(binDir, "codex")
	script := `#!/bin/sh
sleep 30 &
child=$!
printf '%s' "$child" > "$CODATOR_CHILD_PID_FILE"
wait
`
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODATOR_CHILD_PID_FILE", pidFile)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	started := time.Now()
	deadline := started.Add(3 * time.Second)
	probeDone := make(chan error, 1)
	go func() {
		_, probeErr := probeCodexContext(ctx, account)
		probeDone <- probeErr
	}()
	startupDeadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		select {
		case err = <-probeDone:
			if err == nil {
				t.Fatal("probe without a protocol response succeeded")
			}
			t.Fatalf("probe returned before starting the shim: %v", err)
		default:
		}
		if time.Now().After(startupDeadline) {
			cancel()
			<-probeDone
			t.Fatal("timed out waiting for the probe shim")
		}
		time.Sleep(5 * time.Millisecond)
	}
	err = <-probeDone
	if err == nil {
		t.Fatal("probe without a protocol response succeeded")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("probe exceeded its bounded cleanup: %v", elapsed)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("shim did not start its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("invalid grandchild PID: %q", pidBytes)
	}
	running, runningErr := processRunning(pid)
	if runningErr != nil {
		t.Fatal(runningErr)
	}
	for running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		running, runningErr = processRunning(pid)
		if runningErr != nil {
			t.Fatal(runningErr)
		}
	}
	if running {
		t.Fatalf("probe grandchild %d is still running after cancellation", pid)
	}
}

func processRunning(pid int) (bool, error) {
	process, found, err := psProcess(pid)
	if err != nil || !found {
		return false, err
	}
	return processLive(process), nil
}

func TestExecNativeChild(t *testing.T) {
	if os.Getenv("CODATOR_EXEC_NATIVE_CHILD") != "1" {
		return
	}
	store, err := newStore(os.Getenv("CODATOR_TEST_DATA_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock("codex", "exec")
	if err != nil {
		t.Fatal(err)
	}
	if newProbeSignalScope().stopListening() {
		t.Fatal("unexpected signal before native handoff")
	}
	if err := execNative(lock, os.Getenv("CODATOR_NATIVE_STUB"), []string{os.Getenv("CODATOR_NATIVE_MODE")}, os.Environ()); err != nil {
		t.Fatal(err)
	}
}

func TestExecNativeKeepsLockAndChildSemantics(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAccount("codex", "exec"); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "native-stub")
	exitScript := `#!/bin/sh
printf ready > "$CODATOR_READY_FILE"
while [ ! -f "$CODATOR_RELEASE_FILE" ]; do :; done
exit 23
`
	if err := os.WriteFile(stub, []byte(exitScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Run("exit code and lock lifetime", func(t *testing.T) {
		ready, release := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "release")
		cmd := execNativeChildCommand(t, dataHome, stub, "exit", ready, release)
		waitForFile(t, ready, cmd)
		assertExecLockBusy(t, store)
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("native child exit code was lost")
		} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
			t.Fatalf("native child status = %v, want exit 23", err)
		}
		assertExecLockReleased(t, store)
	})
	t.Run("signal delivery after restoring defaults", func(t *testing.T) {
		signalScript := `#!/bin/sh
printf ready > "$CODATOR_READY_FILE"
exec /bin/sleep 30
`
		if err := os.WriteFile(stub, []byte(signalScript), 0700); err != nil {
			t.Fatal(err)
		}
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(sig.String(), func(t *testing.T) {
				ready, release := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "unused")
				cmd := execNativeChildCommand(t, dataHome, stub, "signal", ready, release)
				wait := make(chan error, 1)
				go func() { wait <- cmd.Wait() }()
				waitForFile(t, ready, cmd)
				assertExecLockBusy(t, store)
				time.Sleep(25 * time.Millisecond)
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				var err error
				select {
				case err = <-wait:
				case <-time.After(2 * time.Second):
					_ = cmd.Process.Kill()
					<-wait
					t.Fatalf("native CLI ignored %s after signal restoration", sig)
				}
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("native child signal status = %v, want %s", err, sig)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != sig {
					t.Fatalf("native child status = %v, want %s", exit.ProcessState, sig)
				}
				assertExecLockReleased(t, store)
			})
		}
	})
}

func execNativeChildCommand(t *testing.T, dataHome, stub, mode, ready, release string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestExecNativeChild$")
	cmd.Env = append(os.Environ(),
		"CODATOR_EXEC_NATIVE_CHILD=1",
		"CODATOR_TEST_DATA_HOME="+dataHome,
		"CODATOR_NATIVE_STUB="+stub,
		"CODATOR_NATIVE_MODE="+mode,
		"CODATOR_READY_FILE="+ready,
		"CODATOR_RELEASE_FILE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitForFile(t *testing.T, path string, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if cmd.ProcessState != nil {
			t.Fatalf("native child exited before ready file: %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("native child did not create ready file: %s", path)
}

func assertExecLockBusy(t *testing.T, store *Store) {
	t.Helper()
	if lock, err := store.Lock("codex", "exec"); !errors.Is(err, ErrAccountBusy) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("native lock error=%v, want busy", err)
	}
}

func assertExecLockReleased(t *testing.T, store *Store) {
	t.Helper()
	lock, err := store.Lock("codex", "exec")
	if err != nil {
		t.Fatalf("native lock not released: %v", err)
	}
	lock.Close()
}
