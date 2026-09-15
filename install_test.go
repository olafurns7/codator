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
	targets := []struct {
		name    string
		os      string
		machine string
		asset   string
	}{
		{"Linux amd64", "Linux", "x86_64", "codator-linux-amd64"},
		{"Linux arm64", "Linux", "aarch64", "codator-linux-arm64"},
		{"macOS Intel", "Darwin", "amd64", "codator-darwin-amd64"},
		{"macOS Apple Silicon", "Darwin", "arm64", "codator-darwin-arm64"},
	}
	assets := make(map[string][]byte, len(targets))
	var sums strings.Builder
	for _, target := range targets {
		asset := []byte("new Codator release binary for " + target.asset + "\n")
		assets[target.asset] = asset
		if err := os.WriteFile(filepath.Join(fixture, target.asset), asset, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(asset)
		fmt.Fprintf(&sums, "%x  %s\n", sum, target.asset)
	}
	if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(sums.String()), 0600); err != nil {
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
    case "$url" in */codator-*) printf '%s\n' corrupt > "$out"; exit 0 ;; esac
    ;;
esac
asset=${url##*/}
case "$asset" in
  SHA256SUMS|codator-linux-amd64|codator-linux-arm64|codator-darwin-amd64|codator-darwin-arm64)
    cp "$FAKE_RELEASE_FIXTURE/$asset" "$out"
    ;;
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

	isolatedBin := makeIsolatedInstallPath(t, root, fakeBin)
	run := func(t *testing.T, installDir string, extra map[string]string, isolated bool) (string, error) {
		t.Helper()
		log := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log")
		path := fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")
		if isolated {
			path = isolatedBin
		}
		env := buildEnv(os.Environ(), nil, map[string]string{
			"CODATOR_INSTALL_DIR":  installDir,
			"CODATOR_VERSION":      "v9.9.9",
			"FAKE_CURL_LOG":        log,
			"FAKE_RELEASE_FIXTURE": fixture,
			"HOME":                 filepath.Join(root, "home"),
			"PATH":                 path,
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

	installDir := filepath.Join(root, "install dir's space")
	t.Run("installs verified pinned release into path with spaces", func(t *testing.T) {
		output, err := run(t, installDir, nil, false)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		got, err := os.ReadFile(filepath.Join(installDir, "codator"))
		if err != nil || string(got) != string(assets["codator-linux-amd64"]) {
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
		if err != nil || string(piped) != string(assets["codator-linux-amd64"]) {
			t.Fatalf("piped installed binary = %q, err %v", piped, err)
		}
	})

	for _, target := range targets {
		t.Run("maps "+target.name, func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(target.name, " ", "-"))
			output, err := run(t, dir, map[string]string{"FAKE_UNAME_S": target.os, "FAKE_UNAME_M": target.machine}, false)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, output)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(assets[target.asset]) {
				t.Fatalf("installed %s = %q, err %v", target.asset, got, readErr)
			}
			log, logErr := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log"))
			if logErr != nil || !strings.Contains(string(log), "/"+target.asset) {
				t.Fatalf("asset URL was not requested: %q, err %v", log, logErr)
			}
		})
	}

	for _, mode := range []string{"checksum", "network"} {
		t.Run(mode+" failure preserves existing binary", func(t *testing.T) {
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(installDir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			output, err := run(t, installDir, map[string]string{"FAKE_CURL_MODE": mode}, false)
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

	t.Run("shasum fallback works with isolated PATH", func(t *testing.T) {
		dir := filepath.Join(root, "shasum fallback")
		shasumLog := filepath.Join(root, "shasum.log")
		output, err := run(t, dir, map[string]string{
			"FAKE_UNAME_S":    "Darwin",
			"FAKE_UNAME_M":    "arm64",
			"FAKE_SHASUM_LOG": shasumLog,
		}, true)
		if err != nil {
			t.Fatalf("installer failed with only shasum available: %v\n%s", err, output)
		}
		got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
		if readErr != nil || string(got) != string(assets["codator-darwin-arm64"]) {
			t.Fatalf("fallback installed binary = %q, err %v", got, readErr)
		}
		log, logErr := os.ReadFile(shasumLog)
		if logErr != nil || !strings.Contains(string(log), "-a 256") {
			t.Fatalf("shasum fallback was not called with SHA-256: %q, err %v", log, logErr)
		}
	})

	t.Run("unsupported platform does not replace existing binary", func(t *testing.T) {
		old := []byte("old binary\n")
		if err := os.WriteFile(filepath.Join(installDir, "codator"), old, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, installDir, map[string]string{"FAKE_UNAME_M": "mips64"}, false)
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
		output, err := run(t, installDir, nil, false)
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

func makeIsolatedInstallPath(t *testing.T, root, fakeBin string) string {
	t.Helper()
	isolated := filepath.Join(root, "isolated-path")
	if err := os.Mkdir(isolated, 0700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"awk", "chmod", "cp", "mkdir", "mktemp", "mv", "rm", "sed"} {
		path, err := exec.LookPath(command)
		if err != nil {
			t.Fatalf("find %s: %v", command, err)
		}
		if err := os.Symlink(path, filepath.Join(isolated, command)); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"curl", "uname"} {
		if err := os.Symlink(filepath.Join(fakeBin, command), filepath.Join(isolated, command)); err != nil {
			t.Fatal(err)
		}
	}
	shasum, err := exec.LookPath("shasum")
	if err != nil {
		t.Fatalf("find native shasum: %v", err)
	}
	writeInstallFixture(t, filepath.Join(isolated, "shasum"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_SHASUM_LOG\"\nexec "+shellQuote(shasum)+" \"$@\"\n")
	return isolated
}

func writeInstallFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}
