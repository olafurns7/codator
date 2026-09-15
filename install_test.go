package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallScript(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	if err := os.Mkdir(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	asset := []byte("new Codator release binary\n")
	if err := os.WriteFile(filepath.Join(fixture, "codator-linux-amd64"), asset, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(asset)
	if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  codator-linux-amd64\n", sum)), 0600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) out=$2; shift 2 ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "${FAKE_CURL_MODE:-}" in
  network) exit 22 ;;
  checksum)
    case "$url" in */codator-linux-*) printf '%s\n' corrupt > "$out"; exit 0 ;; esac
    ;;
esac
case "$url" in
  */SHA256SUMS) cp "$FAKE_RELEASE_FIXTURE/SHA256SUMS" "$out" ;;
  */codator-linux-amd64) cp "$FAKE_RELEASE_FIXTURE/codator-linux-amd64" "$out" ;;
  *) exit 22 ;;
esac
`)
	writeInstallFixture(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "${FAKE_UNAME_S:-Linux}" ;;
  -m) printf '%s\n' "${FAKE_UNAME_M:-x86_64}" ;;
  *) exit 1 ;;
esac
`)

	installDir := filepath.Join(root, "install dir's space")
	run := func(t *testing.T, extra map[string]string) (string, error) {
		t.Helper()
		log := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log")
		env := buildEnv(os.Environ(), nil, map[string]string{
			"CODATOR_INSTALL_DIR":  installDir,
			"CODATOR_VERSION":      "v9.9.9",
			"FAKE_CURL_LOG":        log,
			"FAKE_RELEASE_FIXTURE": fixture,
			"HOME":                 filepath.Join(root, "home"),
			"PATH":                 fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		for key, value := range extra {
			env = buildEnv(env, nil, map[string]string{key: value})
		}
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", filepath.Join(wd, "install.sh"))
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	runPiped := func(t *testing.T, dir string) (string, error) {
		t.Helper()
		env := buildEnv(os.Environ(), nil, map[string]string{
			"FAKE_CURL_LOG":        filepath.Join(root, "piped.curl-log"),
			"FAKE_CURL_MODE":       "",
			"FAKE_RELEASE_FIXTURE": fixture,
			"FAKE_UNAME_M":         "x86_64",
			"FAKE_UNAME_S":         "Linux",
			"HOME":                 filepath.Join(root, "home"),
			"PATH":                 fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", "cat \"$1\" | CODATOR_INSTALL_DIR=\"$2\" CODATOR_VERSION=v9.9.9 sh", "sh", filepath.Join(wd, "install.sh"), dir)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	t.Run("installs verified pinned release into path with spaces", func(t *testing.T) {
		output, err := run(t, nil)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		got, err := os.ReadFile(filepath.Join(installDir, "codator"))
		if err != nil || string(got) != string(asset) {
			t.Fatalf("installed binary = %q, err %v", got, err)
		}
		log, err := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log"))
		if err != nil || !strings.Contains(string(log), "/download/v9.9.9/codator-linux-amd64") {
			t.Fatalf("pinned release URL was not requested: %q, err %v", log, err)
		}
		var pathCommand string
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "  export PATH=") {
				pathCommand = line
			}
		}
		pathCheck := exec.Command("/bin/sh", "-c", pathCommand+"; printf '%s' \"$PATH\"")
		pathCheck.Env = []string{"PATH=/usr/bin:/bin"}
		path, err := pathCheck.Output()
		if err != nil || string(path) != installDir+":/usr/bin:/bin" {
			t.Fatalf("PATH command output = %q, err %v", path, err)
		}
		pipedDir := filepath.Join(root, "piped install")
		pipedOutput, err := runPiped(t, pipedDir)
		if err != nil {
			t.Fatalf("piped installer failed: %v\n%s", err, pipedOutput)
		}
		piped, err := os.ReadFile(filepath.Join(pipedDir, "codator"))
		if err != nil || string(piped) != string(asset) {
			t.Fatalf("piped installed binary = %q, err %v", piped, err)
		}
	})

	for _, mode := range []string{"checksum", "network"} {
		t.Run(mode+" failure preserves existing binary", func(t *testing.T) {
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(installDir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			output, err := run(t, map[string]string{"FAKE_CURL_MODE": mode})
			if err == nil {
				t.Fatalf("installer unexpectedly succeeded: %s", output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("installer error = %v, want exit error", err)
			}
			got, readErr := os.ReadFile(filepath.Join(installDir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("existing binary changed to %q, err %v", got, readErr)
			}
		})
	}

	t.Run("unsupported platform does not replace existing binary", func(t *testing.T) {
		old := []byte("old binary\n")
		if err := os.WriteFile(filepath.Join(installDir, "codator"), old, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, map[string]string{"FAKE_UNAME_M": "mips64"})
		if err == nil || !strings.Contains(output, "unsupported architecture") {
			t.Fatalf("installer error = %v, output %q", err, output)
		}
		got, readErr := os.ReadFile(filepath.Join(installDir, "codator"))
		if readErr != nil || string(got) != string(old) {
			t.Fatalf("existing binary changed to %q, err %v", got, readErr)
		}
	})

	t.Run("directory destination is preserved", func(t *testing.T) {
		target := filepath.Join(installDir, "codator")
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, nil)
		if err == nil || !strings.Contains(output, "is a directory") {
			t.Fatalf("installer error = %v, output %q", err, output)
		}
		info, statErr := os.Stat(target)
		entries, readErr := os.ReadDir(target)
		if statErr != nil || !info.IsDir() || readErr != nil || len(entries) != 0 {
			t.Fatalf("directory target changed: stat=%v entries=%v read=%v", statErr, entries, readErr)
		}
	})
}

func writeInstallFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}
