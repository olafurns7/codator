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
		asset := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_BINARY_LOG\"\ncase \"${1:-}\" in\n--help|help)\n  printf '%s\\n' 'Usage:' '  codator doctor [codex|claude]'\n  exit 0\n  ;;\ndoctor)\n  printf '%s\\n' \"$*\" >> \"$FAKE_DOCTOR_LOG\"\n  [ \"${FAKE_DOCTOR_RESULT:-ok}\" = fail ] && exit 1\n  exit 0\n  ;;\n*) exit 99 ;;\nesac\n")
		assets[target.asset] = asset
		if err := os.WriteFile(filepath.Join(fixture, target.asset), asset, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(asset)
		fmt.Fprintf(&sums, "%x  %s\n", sum, target.asset)
	}
	for _, shortcut := range []string{"cdx", "cdl"} {
		body := []byte("#!/bin/sh\nexec codator " + shortcut + " \"$@\"\n")
		assets[shortcut] = body
		if err := os.WriteFile(filepath.Join(fixture, shortcut), body, 0700); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(body), shortcut)
	}
	if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(sums.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "attestations.jsonl"), []byte("signed bundle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	installScript, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "install.sh"), installScript, 0700); err != nil {
		t.Fatal(err)
	}
	legacyFixture := filepath.Join(root, "legacy-fixture")
	if err := os.Mkdir(legacyFixture, 0700); err != nil {
		t.Fatal(err)
	}
	legacyAsset := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_BINARY_LOG\"\ncase \"${1:-}\" in\n--help|help)\n  printf '%s\\n' 'Usage:' '  codator status [codex|claude]'\n  exit 0\n  ;;\ndoctor) exit 2 ;;\n*) exit 99 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(legacyFixture, "codator-linux-amd64"), legacyAsset, 0700); err != nil {
		t.Fatal(err)
	}
	legacySum := sha256.Sum256(legacyAsset)
	if err := os.WriteFile(filepath.Join(legacyFixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  codator-linux-amd64\n", legacySum)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyFixture, "attestations.jsonl"), []byte("signed legacy bundle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unsignedFixture := filepath.Join(root, "unsigned-fixture")
	if err := os.Mkdir(unsignedFixture, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsignedFixture, "codator-linux-amd64"), legacyAsset, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsignedFixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  codator-linux-amd64\n", legacySum)), 0600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
set -eu
out=
write_out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) out=$2; shift 2 ;;
    --write-out) write_out=$2; shift 2 ;;
    --location|--fail|--silent|--show-error|--tlsv1.2) shift ;;
    --proto) shift 2 ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "${FAKE_CURL_MODE:-}" in
  network) exit 22 ;;
  partial)
    printf '%s\n' '#!/bin/sh' 'printf "%s\\n" executed > "$README_INSTALLER_MARKER"' > "$out"
    exit 22
    ;;
esac
if [ "$url" = "https://github.com/olafurns7/codator/releases/latest" ]; then
  [ "$out" = /dev/null ] || exit 22
  [ "$write_out" = '%{url_effective}' ] || exit 22
  case "${FAKE_CURL_MODE:-}" in
    malformed-latest) printf '%s' 'https://github.com/olafurns7/codator/releases/download/v9.9.9' ;;
    wronghost-latest) printf '%s' 'https://evil.example/releases/tag/v9.9.9' ;;
    *) printf '%s' "${FAKE_LATEST_REDIRECT:-https://github.com/olafurns7/codator/releases/tag/v9.9.9}" ;;
  esac
  exit 0
fi
case "${FAKE_CURL_MODE:-}" in
  network) exit 22 ;;
  partial)
    printf '%s\n' '#!/bin/sh' 'printf "%s\\n" executed > "$README_INSTALLER_MARKER"' > "$out"
    exit 22
    ;;
  checksum)
    case "$url" in */codator-*) printf '%s\n' corrupt > "$out"; exit 0 ;; esac
    ;;
  tampered)
    case "$url" in */SHA256SUMS) cp "$FAKE_TAMPERED_SUMS" "$out"; exit 0 ;; esac
    case "$url" in */codator-*) cp "$FAKE_TAMPERED_ASSET" "$out"; exit 0 ;; esac
    ;;
  tampered-asset)
    case "$url" in */codator-*) cp "$FAKE_TAMPERED_ASSET" "$out"; exit 0 ;; esac
    ;;
  missingbundle)
    case "$url" in */attestations.jsonl) exit 22 ;; esac
    ;;
  tampered-shortcut)
    case "$url" in */cdl) printf '%s\n' corrupt > "$out"; exit 0 ;; esac
    ;;
esac
asset=${url##*/}
case "$asset" in
  cdx|cdl|SHA256SUMS|attestations.jsonl|install.sh|codator-linux-amd64|codator-linux-arm64|codator-darwin-amd64|codator-darwin-arm64)
    cp "$FAKE_RELEASE_FIXTURE/$asset" "$out"
    ;;
  *) exit 22 ;;
esac
`)
	writeInstallFixture(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
if [ "${1:-}" = version ]; then
  printf 'gh version %s (fixture)\n' "${FAKE_GH_VERSION:-2.86.0}"
  exit 0
fi
[ "${FAKE_GH_MODE:-}" != reject ] || exit 42
[ "$#" -eq 14 ] || exit 43
[ "$1" = attestation ] && [ "$2" = verify ] || exit 44
case "$3" in */SHA256SUMS) ;; *) exit 45 ;; esac
[ "$4" = --bundle ] || exit 46
case "$5" in */attestations.jsonl) ;; *) exit 47 ;; esac
[ "$6" = --repo ] || exit 48
[ "$7" = "${FAKE_GH_EXPECTED_REPO:-olafurns7/codator}" ] || exit 49
[ "$8" = --cert-identity ] || exit 50
[ "$9" = "${FAKE_GH_EXPECTED_CERT_IDENTITY:-https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/${FAKE_GH_EXPECTED_TAG:-v9.9.9}}" ] || exit 51
[ "${10}" = --source-ref ] || exit 52
[ "${11}" = "${FAKE_GH_EXPECTED_SOURCE_REF:-refs/tags/${FAKE_GH_EXPECTED_TAG:-v9.9.9}}" ] || exit 53
[ "${12}" = --deny-self-hosted-runners ] || exit 54
[ "${13}" = --hostname ] || exit 55
[ "${14}" = github.com ] || exit 56
exit 0
`)
	writeInstallFixture(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "${FAKE_UNAME_S:-Linux}" ;;
  -m) printf '%s\n' "${FAKE_UNAME_M:-x86_64}" ;;
  *) exit 1 ;;
esac
`)
	for _, native := range []string{"codex", "claude"} {
		writeInstallFixture(t, filepath.Join(fakeBin, native), "#!/bin/sh\nexit 99\n")
	}

	isolatedBin := makeIsolatedInstallPath(t, root, fakeBin)
	missingVerifierRoot := filepath.Join(root, "missing-verifier-root")
	if err := os.Mkdir(missingVerifierRoot, 0700); err != nil {
		t.Fatal(err)
	}
	missingVerifierBin := makeIsolatedInstallPath(t, missingVerifierRoot, fakeBin)
	if err := os.Remove(filepath.Join(missingVerifierBin, "gh")); err != nil {
		t.Fatal(err)
	}
	oneProviderRoot := filepath.Join(root, "one-provider-root")
	if err := os.Mkdir(oneProviderRoot, 0700); err != nil {
		t.Fatal(err)
	}
	oneProviderBin := makeIsolatedInstallPath(t, oneProviderRoot, fakeBin)
	if err := os.Symlink(filepath.Join(fakeBin, "codex"), filepath.Join(oneProviderBin, "codex")); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, installDir string, extra map[string]string, isolated bool) (string, error) {
		t.Helper()
		name := strings.ReplaceAll(t.Name(), "/", "_")
		curlLog := filepath.Join(root, name+".curl-log")
		ghLog := filepath.Join(root, name+".gh-log")
		path := fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")
		if isolated {
			path = isolatedBin
		}
		if extraPath, ok := extra["CODATOR_TEST_PATH"]; ok {
			path = extraPath
		}
		env := buildEnv(os.Environ(), nil, map[string]string{
			"CODATOR_INSTALL_DIR":        installDir,
			"CODATOR_VERIFY_ATTESTATION": "0",
			"CODATOR_VERSION":            "v9.9.9",
			"FAKE_BINARY_LOG":            filepath.Join(root, name+".binary-log"),
			"FAKE_CURL_LOG":              curlLog,
			"FAKE_CURL_MODE":             "",
			"FAKE_DOCTOR_LOG":            filepath.Join(root, name+".doctor-log"),
			"FAKE_GH_EXPECTED_TAG":       "v9.9.9",
			"FAKE_GH_LOG":                ghLog,
			"FAKE_GH_MODE":               "",
			"FAKE_RELEASE_FIXTURE":       fixture,
			"FAKE_UNAME_M":               "x86_64",
			"FAKE_UNAME_S":               "Linux",
			"HOME":                       filepath.Join(root, "home"),
			"PATH":                       path,
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
		env := buildEnv(os.Environ(), []string{"CODATOR_VERSION"}, map[string]string{
			"CODATOR_VERSION":            "v9.9.9",
			"CODATOR_VERIFY_ATTESTATION": "0",
			"FAKE_BINARY_LOG":            filepath.Join(root, "piped.binary-log"),
			"FAKE_CURL_LOG":              filepath.Join(root, "piped.curl-log"),
			"FAKE_CURL_MODE":             "",
			"FAKE_DOCTOR_LOG":            filepath.Join(root, "piped.doctor-log"),
			"FAKE_GH_EXPECTED_TAG":       "v9.9.9",
			"FAKE_GH_LOG":                filepath.Join(root, "piped.gh-log"),
			"FAKE_GH_MODE":               "",
			"FAKE_RELEASE_FIXTURE":       fixture,
			"FAKE_UNAME_M":               "x86_64",
			"FAKE_UNAME_S":               "Linux",
			"HOME":                       filepath.Join(root, "home"),
			"PATH":                       fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
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
	t.Run("shortcuts are opt-in, verified, and never clobber unrelated commands", func(t *testing.T) {
		dir := filepath.Join(root, "shortcuts")
		if output, err := run(t, dir, nil, true); err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		for _, shortcut := range []string{"cdx", "cdl"} {
			if _, err := os.Stat(filepath.Join(dir, shortcut)); !os.IsNotExist(err) {
				t.Fatalf("%s installed without CODATOR_SHORTCUTS: %v", shortcut, err)
			}
		}

		oldCdx := []byte("#!/bin/sh\nexec codator codex \"$@\"\n")
		foreignCdl := []byte("#!/bin/sh\necho someone else's cdl\n")
		if err := os.WriteFile(filepath.Join(dir, "cdx"), oldCdx, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cdl"), foreignCdl, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, dir, map[string]string{"CODATOR_SHORTCUTS": "1"}, true)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "cdx")); string(got) != string(assets["cdx"]) {
			t.Fatalf("old Codator cdx was not replaced: %q", got)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "cdl")); string(got) != string(foreignCdl) {
			t.Fatalf("unrelated cdl was overwritten: %q", got)
		}
		if !strings.Contains(output, "Skipped cdl") || !strings.Contains(output, "bypass ALL sandboxing and approval prompts") {
			t.Fatalf("missing skip notice or warning:\n%s", output)
		}

		fresh := filepath.Join(root, "shortcuts tampered")
		output, err = run(t, fresh, map[string]string{"CODATOR_SHORTCUTS": "1", "FAKE_CURL_MODE": "tampered-shortcut"}, true)
		if err == nil || !strings.Contains(output, "checksum verification failed for cdl") {
			t.Fatalf("tampered shortcut accepted: %v\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(fresh, "codator")); !os.IsNotExist(err) {
			t.Fatalf("binary installed before all downloads verified: %v", err)
		}

		if output, err := run(t, fresh, map[string]string{"CODATOR_SHORTCUTS": "yes"}, true); err == nil || !strings.Contains(output, "CODATOR_SHORTCUTS must be 0 or 1") {
			t.Fatalf("invalid CODATOR_SHORTCUTS accepted: %v\n%s", err, output)
		}
	})
	t.Run("installs verified pinned release into path with spaces", func(t *testing.T) {
		output, err := run(t, installDir, nil, false)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		if log, err := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".doctor-log")); err != nil || string(log) != "doctor codex\ndoctor claude\n" {
			t.Fatalf("installer did not run doctor for installed native CLIs: %q err=%v", log, err)
		}
		got, err := os.ReadFile(filepath.Join(installDir, "codator"))
		if err != nil || string(got) != string(assets["codator-linux-amd64"]) {
			t.Fatalf("installed binary = %q, err %v", got, err)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".gh-log"))
		log, err := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log"))
		if err != nil || !strings.Contains(string(log), "/download/v9.9.9/codator-linux-amd64") || !strings.Contains(string(log), "/download/v9.9.9/SHA256SUMS") || strings.Contains(string(log), "/download/v9.9.9/attestations.jsonl") {
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
		if log, err := os.ReadFile(filepath.Join(root, "piped.doctor-log")); err != nil || string(log) != "doctor codex\ndoctor claude\n" {
			t.Fatalf("piped installer did not run doctor: %q err=%v", log, err)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, "piped.gh-log"))
		piped, err := os.ReadFile(filepath.Join(pipedDir, "codator"))
		if err != nil || string(piped) != string(assets["codator-linux-amd64"]) {
			t.Fatalf("piped installed binary = %q, err %v", piped, err)
		}
	})

	t.Run("opt-in verifies the signed manifest before install", func(t *testing.T) {
		dir := filepath.Join(root, "opt-in install")
		output, err := run(t, dir, map[string]string{"CODATOR_VERIFY_ATTESTATION": "1"}, false)
		if err != nil {
			t.Fatalf("opt-in installer failed: %v\n%s", err, output)
		}
		assertAttestationArgs(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".gh-log"), "v9.9.9", 1)
		log, readErr := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".curl-log"))
		if readErr != nil || !strings.Contains(string(log), "/download/v9.9.9/attestations.jsonl") {
			t.Fatalf("opt-in did not download the attestation bundle: %q, err %v", log, readErr)
		}
	})

	t.Run("default checksum install succeeds without gh", func(t *testing.T) {
		dir := filepath.Join(root, "default without gh")
		output, err := run(t, dir, map[string]string{"CODATOR_TEST_PATH": missingVerifierBin}, false)
		if err != nil {
			t.Fatalf("default installer unexpectedly needs gh: %v\n%s", err, output)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".gh-log"))
		if _, readErr := os.Stat(filepath.Join(dir, "codator")); readErr != nil {
			t.Fatalf("default installer did not install without gh: %v", readErr)
		}
	})

	for _, value := range []string{"", "2"} {
		t.Run("invalid attestation setting "+value, func(t *testing.T) {
			dir := filepath.Join(root, "invalid-attestation-"+strings.ReplaceAll(value, "", "empty"))
			output, err := run(t, dir, map[string]string{"CODATOR_VERIFY_ATTESTATION": value}, false)
			if err == nil || !strings.Contains(output, "CODATOR_VERIFY_ATTESTATION must be 0 or 1") {
				t.Fatalf("invalid setting result=%v output=%q", err, output)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("invalid setting touched install directory: %v", statErr)
			}
		})
	}

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

	t.Run("latest resolves once and downloads every byte from the resolved tag", func(t *testing.T) {
		dir := filepath.Join(root, "latest install")
		output, err := run(t, dir, map[string]string{"CODATOR_VERSION": "latest"}, false)
		if err != nil {
			t.Fatalf("installer failed: %v\n%s", err, output)
		}
		name := strings.ReplaceAll(t.Name(), "/", "_")
		log, readErr := os.ReadFile(filepath.Join(root, name+".curl-log"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if got := strings.Count(string(log), "https://github.com/olafurns7/codator/releases/latest\n"); got != 1 {
			t.Fatalf("latest redirect requests=%d, log=%q", got, log)
		}
		if strings.Contains(string(log), "/releases/latest/download") || !strings.Contains(string(log), "/download/v9.9.9/SHA256SUMS") || strings.Contains(string(log), "/download/v9.9.9/attestations.jsonl") {
			t.Fatalf("latest did not use one resolved tag: %q", log)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, name+".gh-log"))
	})

	t.Run("one installed provider is sufficient even when doctor needs attention", func(t *testing.T) {
		dir := filepath.Join(root, "one-provider")
		output, err := run(t, dir, map[string]string{
			"CODATOR_TEST_PATH":  oneProviderBin,
			"FAKE_DOCTOR_RESULT": "fail",
		}, false)
		if err != nil {
			t.Fatalf("installer treated optional provider or doctor warning as an install failure: %v\n%s", err, output)
		}
		if !strings.Contains(output, "Installed Codator") || !strings.Contains(output, "codex prerequisites need attention") || strings.Contains(output, "Checking claude prerequisites") {
			t.Fatalf("installer output=%q", output)
		}
		log, readErr := os.ReadFile(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".doctor-log"))
		if readErr != nil || string(log) != "doctor codex\n" {
			t.Fatalf("doctor log=%q err=%v", log, readErr)
		}
	})

	t.Run("older release skips unsupported doctor", func(t *testing.T) {
		dir := filepath.Join(root, "legacy-release")
		output, err := run(t, dir, map[string]string{
			"CODATOR_VERSION":      "v0.2.0",
			"FAKE_GH_EXPECTED_TAG": "v0.2.0",
			"FAKE_RELEASE_FIXTURE": legacyFixture,
		}, false)
		if err != nil {
			t.Fatalf("legacy installer failed: %v\n%s", err, output)
		}
		if !strings.Contains(output, "does not support doctor; skipping prerequisite checks") || strings.Contains(output, "Checking codex prerequisites") {
			t.Fatalf("legacy installer output=%q", output)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".gh-log"))
		got, err := os.ReadFile(filepath.Join(dir, "codator"))
		if err != nil || string(got) != string(legacyAsset) {
			t.Fatalf("legacy installed binary=%q err=%v", got, err)
		}
	})

	t.Run("no native provider gives a next step", func(t *testing.T) {
		dir := filepath.Join(root, "no-provider")
		output, err := run(t, dir, map[string]string{"CODATOR_TEST_PATH": isolatedBin}, false)
		if err != nil {
			t.Fatalf("installer failed without optional native CLIs: %v\n%s", err, output)
		}
		if !strings.Contains(output, "Install the Codex or Claude native CLI") || !strings.Contains(output, "codator doctor") {
			t.Fatalf("installer output=%q", output)
		}
		if _, err := os.Stat(filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".doctor-log")); !os.IsNotExist(err) {
			t.Fatalf("installer ran doctor without a native CLI: %v", err)
		}
	})

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
			assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
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

	t.Run("tampered binary with attacker-replaced checksum is rejected before install", func(t *testing.T) {
		tamperedAsset := []byte("#!/bin/sh\nprintf 'attacker ran\\n' >> \"$FAKE_BINARY_LOG\"\n")
		tamperedPath := filepath.Join(root, "tampered-asset")
		if err := os.WriteFile(tamperedPath, tamperedAsset, 0700); err != nil {
			t.Fatal(err)
		}
		tamperedSum := sha256.Sum256(tamperedAsset)
		tamperedSumsPath := filepath.Join(root, "tampered-SHA256SUMS")
		if err := os.WriteFile(tamperedSumsPath, []byte(fmt.Sprintf("%x  codator-linux-amd64\n", tamperedSum)), 0600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "tampered destination")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		old := []byte("old binary\n")
		if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, dir, map[string]string{
			"FAKE_CURL_MODE":             "tampered",
			"CODATOR_VERIFY_ATTESTATION": "1",
			"FAKE_GH_MODE":               "reject",
			"FAKE_TAMPERED_ASSET":        tamperedPath,
			"FAKE_TAMPERED_SUMS":         tamperedSumsPath,
		}, false)
		if err == nil {
			t.Fatalf("tampered installer unexpectedly succeeded: %s", output)
		}
		got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
		if readErr != nil || string(got) != string(old) {
			t.Fatalf("existing binary changed to %q, err %v", got, readErr)
		}
		assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
	})

	t.Run("tampered binary fails the signed manifest checksum", func(t *testing.T) {
		tamperedAsset := []byte("#!/bin/sh\nprintf 'attacker ran\\n' >> \"$FAKE_BINARY_LOG\"\n")
		tamperedPath := filepath.Join(root, "tampered-signed-asset")
		if err := os.WriteFile(tamperedPath, tamperedAsset, 0700); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "tampered signed destination")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		old := []byte("old binary\n")
		if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, dir, map[string]string{
			"CODATOR_VERIFY_ATTESTATION": "1",
			"FAKE_CURL_MODE":             "tampered-asset",
			"FAKE_TAMPERED_ASSET":        tamperedPath,
		}, false)
		if err == nil {
			t.Fatalf("tampered binary unexpectedly passed signed checksum: %s", output)
		}
		got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
		if readErr != nil || string(got) != string(old) {
			t.Fatalf("existing binary changed to %q, err %v", got, readErr)
		}
		assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
	})

	for _, test := range []struct {
		name  string
		extra map[string]string
	}{
		{"missing bundle", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_CURL_MODE": "missingbundle"}},
		{"unsigned bundle", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_GH_MODE": "reject"}},
		{"unsupported verifier", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_GH_VERSION": "2.85.0"}},
	} {
		t.Run(test.name+" preserves old destination and executes nothing", func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+" destination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			output, err := run(t, dir, test.extra, false)
			if err == nil {
				t.Fatalf("installer unexpectedly succeeded: %s", output)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("existing binary changed to %q, err %v", got, readErr)
			}
			assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
		})
	}

	t.Run("default checksum mode accepts a release without provenance bundle", func(t *testing.T) {
		dir := filepath.Join(root, "unsigned default release")
		output, err := run(t, dir, map[string]string{
			"CODATOR_VERSION":      "v0.3.2",
			"FAKE_RELEASE_FIXTURE": unsignedFixture,
		}, false)
		if err != nil {
			t.Fatalf("default checksum install unexpectedly failed without a bundle: %v\n%s", err, output)
		}
		assertNoGitHubCLICall(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".gh-log"))
	})

	t.Run("opt-in release without provenance bundle fails closed", func(t *testing.T) {
		dir := filepath.Join(root, "unsigned release")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		old := []byte("old binary\n")
		if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := run(t, dir, map[string]string{
			"CODATOR_VERSION":            "v0.3.2",
			"CODATOR_VERIFY_ATTESTATION": "1",
			"FAKE_GH_EXPECTED_TAG":       "v0.3.2",
			"FAKE_RELEASE_FIXTURE":       unsignedFixture,
		}, false)
		if err == nil {
			t.Fatalf("unsigned release unexpectedly succeeded: %s", output)
		}
		got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
		if readErr != nil || string(got) != string(old) {
			t.Fatalf("existing binary changed to %q, err %v", got, readErr)
		}
		assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
	})

	for _, test := range []struct {
		name  string
		extra map[string]string
	}{
		{"wrong repository", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_GH_EXPECTED_REPO": "attacker/repo"}},
		{"wrong workflow", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_GH_EXPECTED_CERT_IDENTITY": "https://github.com/olafurns7/codator/.github/workflows/other.yml@refs/tags/v9.9.9"}},
		{"wrong tag", map[string]string{"CODATOR_VERIFY_ATTESTATION": "1", "FAKE_GH_EXPECTED_TAG": "v8.8.8"}},
	} {
		t.Run("wrong attestation "+test.name+" is rejected", func(t *testing.T) {
			dir := filepath.Join(root, "wrong-"+strings.ReplaceAll(test.name, " ", "-")+" destination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			output, err := run(t, dir, test.extra, false)
			if err == nil {
				t.Fatalf("installer unexpectedly accepted wrong %s: %s", test.name, output)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("existing binary changed to %q, err %v", got, readErr)
			}
			assertNoBinaryExecution(t, filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "_")+".binary-log"))
		})
	}

	t.Run("missing verifier fails before release download", func(t *testing.T) {
		dir := filepath.Join(root, "missing verifier")
		output, err := run(t, dir, map[string]string{"CODATOR_TEST_PATH": missingVerifierBin, "CODATOR_VERIFY_ATTESTATION": "1"}, false)
		if err == nil || !strings.Contains(output, "required command not found: gh") {
			t.Fatalf("missing verifier error=%v output=%q", err, output)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("missing verifier touched install directory: %v", statErr)
		}
	})

	for _, test := range []struct {
		name string
		mode string
	}{
		{"malformed latest redirect", "malformed-latest"},
		{"wrong host latest redirect", "wronghost-latest"},
	} {
		t.Run(test.name+" fails before downloads", func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+" destination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			output, err := run(t, dir, map[string]string{
				"CODATOR_VERSION": "latest",
				"FAKE_CURL_MODE":  test.mode,
			}, false)
			if err == nil {
				t.Fatalf("installer unexpectedly accepted %s: %s", test.name, output)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("existing binary changed to %q, err %v", got, readErr)
			}
			name := strings.ReplaceAll(t.Name(), "/", "_")
			log, logErr := os.ReadFile(filepath.Join(root, name+".curl-log"))
			if logErr != nil || strings.Count(string(log), "releases/latest\n") != 1 || strings.Contains(string(log), "/download/") {
				t.Fatalf("latest failure downloaded a release: %q err=%v", log, logErr)
			}
			assertNoBinaryExecution(t, filepath.Join(root, name+".binary-log"))
		})
	}
}

func TestREADMEBootstrap(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	if err := os.Mkdir(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	installScript, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "install.sh"), installScript, 0700); err != nil {
		t.Fatal(err)
	}
	asset := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_BINARY_LOG\"\ncase \"${1:-}\" in\n--help|help)\n  printf '%s\\n' 'Usage:' '  codator doctor [codex|claude]'\n  exit 0\n  ;;\ndoctor)\n  printf '%s\\n' \"$*\" >> \"$FAKE_DOCTOR_LOG\"\n  exit 0\n  ;;\n*) exit 99 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(fixture, "codator-linux-amd64"), asset, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(asset)
	installSum := sha256.Sum256(installScript)
	if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  install.sh\n%x  codator-linux-amd64\n", installSum, sum)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "attestations.jsonl"), []byte("signed bootstrap bundle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
set -eu
out=
write_out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output|-o) out=$2; shift 2 ;;
    --write-out) write_out=$2; shift 2 ;;
    --location|--fail|--silent|--show-error|--tlsv1.2) shift ;;
    --proto) shift 2 ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
if [ "$url" = "https://github.com/olafurns7/codator/releases/latest" ]; then
  [ "$out" = /dev/null ] && [ "$write_out" = '%{url_effective}' ] || exit 22
  printf '%s' 'https://github.com/olafurns7/codator/releases/tag/v9.9.9'
  exit 0
fi
case "${FAKE_CURL_MODE:-}" in
  network) exit 22 ;;
  partial)
    printf '%s\n' '#!/bin/sh' 'printf "%s\\n" executed > "$README_INSTALLER_MARKER"' > "$out"
    exit 22
    ;;
esac
asset=${url##*/}
case "$asset" in
  install.sh|attestations.jsonl|SHA256SUMS|codator-linux-amd64) cp "$FAKE_RELEASE_FIXTURE/$asset" "$out" ;;
  *) exit 22 ;;
esac
`)
	writeInstallFixture(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
if [ "${1:-}" = version ]; then
  printf 'gh version 2.86.0 (fixture)\n'
  exit 0
fi
[ "${FAKE_GH_MODE:-}" != reject ] || exit 42
[ "$#" -eq 14 ] || exit 43
[ "$1" = attestation ] && [ "$2" = verify ] || exit 44
case "$3" in */SHA256SUMS) ;; *) exit 45 ;; esac
[ "$4" = --bundle ] || exit 46
case "$5" in */attestations.jsonl) ;; *) exit 47 ;; esac
[ "$6" = --repo ] && [ "$7" = olafurns7/codator ] || exit 48
[ "$8" = --cert-identity ] && [ "$9" = "https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/v9.9.9" ] || exit 49
[ "${10}" = --source-ref ] && [ "${11}" = refs/tags/v9.9.9 ] || exit 50
[ "${12}" = --deny-self-hosted-runners ] || exit 51
[ "${13}" = --hostname ] && [ "${14}" = github.com ] || exit 52
`)
	writeInstallFixture(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' Linux ;;
  -m) printf '%s\n' x86_64 ;;
  *) exit 1 ;;
esac
`)
	for _, native := range []string{"codex", "claude"} {
		writeInstallFixture(t, filepath.Join(fakeBin, native), "#!/bin/sh\nexit 99\n")
	}

	readmePath := makeIsolatedInstallPath(t, root, fakeBin)
	if err := os.Remove(filepath.Join(readmePath, "gh")); err != nil {
		t.Fatal(err)
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readmeText := string(readme)
	start := strings.Index(readmeText, "~~~sh\n(\nset -eu\n")
	if start < 0 {
		t.Fatal("README bootstrap code block not found")
	}
	start += len("~~~sh\n")
	end := strings.Index(readmeText[start:], "\n~~~")
	if end < 0 {
		t.Fatal("README bootstrap code block is not closed")
	}
	bootstrap := readmeText[start : start+end]
	installDir := filepath.Join(root, "README install")
	name := "readme-bootstrap"
	env := buildEnv(os.Environ(), []string{"CODATOR_VERSION", "CODATOR_INSTALL_DIR"}, map[string]string{
		"CODATOR_INSTALL_DIR":  installDir,
		"FAKE_BINARY_LOG":      filepath.Join(root, name+".binary-log"),
		"FAKE_CURL_LOG":        filepath.Join(root, name+".curl-log"),
		"FAKE_DOCTOR_LOG":      filepath.Join(root, name+".doctor-log"),
		"FAKE_GH_LOG":          filepath.Join(root, name+".gh-log"),
		"FAKE_GH_MODE":         "",
		"FAKE_RELEASE_FIXTURE": fixture,
		"FAKE_SHASUM_LOG":      filepath.Join(root, name+".shasum-log"),
		"HOME":                 filepath.Join(root, "home"),
		"PATH":                 readmePath,
	})
	cmd := exec.Command("/bin/sh", "-c", bootstrap)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("README bootstrap failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(filepath.Join(installDir, "codator"))
	if err != nil || string(got) != string(asset) {
		t.Fatalf("README bootstrap installed=%q err=%v", got, err)
	}
	log, err := os.ReadFile(filepath.Join(root, name+".curl-log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "releases/latest") || strings.Contains(string(log), "/releases/latest/download") || !strings.Contains(string(log), "/download/v0.5.4/install.sh") || strings.Contains(string(log), "/download/v0.5.4/attestations.jsonl") || !strings.Contains(string(log), "/download/v0.5.4/SHA256SUMS") || !strings.Contains(string(log), "/download/v0.5.4/codator-linux-amd64") {
		t.Fatalf("README bootstrap mixed release URLs: %q", log)
	}
	assertNoGitHubCLICall(t, filepath.Join(root, name+".gh-log"))

	t.Run("optional security bootstrap verifies the manifest chain", func(t *testing.T) {
		security, err := os.ReadFile("SECURITY.md")
		if err != nil {
			t.Fatal(err)
		}
		securityText := string(security)
		start := strings.Index(securityText, "~~~sh\n(\nset -eu\numask 077\n")
		if start < 0 {
			t.Fatal("SECURITY bootstrap code block not found")
		}
		start += len("~~~sh\n")
		end := strings.Index(securityText[start:], "\n~~~")
		if end < 0 {
			t.Fatal("SECURITY bootstrap code block is not closed")
		}
		bootstrap := securityText[start : start+end]
		secureDir := filepath.Join(root, "secure install")
		secureEnv := buildEnv(os.Environ(), []string{"CODATOR_INSTALL_DIR"}, map[string]string{
			"CODATOR_INSTALL_DIR":  secureDir,
			"CODATOR_VERSION":      "v9.9.9",
			"FAKE_BINARY_LOG":      filepath.Join(root, "secure.binary-log"),
			"FAKE_CURL_LOG":        filepath.Join(root, "secure.curl-log"),
			"FAKE_CURL_MODE":       "",
			"FAKE_GH_EXPECTED_TAG": "v9.9.9",
			"FAKE_GH_LOG":          filepath.Join(root, "secure.gh-log"),
			"FAKE_GH_MODE":         "",
			"FAKE_RELEASE_FIXTURE": fixture,
			"HOME":                 filepath.Join(root, "secure-home"),
			"PATH":                 fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
		cmd := exec.Command("/bin/sh", "-c", bootstrap)
		cmd.Env = secureEnv
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("optional security bootstrap failed: %v\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(secureDir, "codator")); err != nil {
			t.Fatalf("optional security bootstrap did not install: %v", err)
		}
		assertAttestationArgs(t, filepath.Join(root, "secure.gh-log"), "v9.9.9", 2)

		t.Run("manifest attestation rejection does not execute installer", func(t *testing.T) {
			marker := filepath.Join(root, "secure-rejected-installer-executed")
			markerInstaller := []byte("#!/bin/sh\nprintf '%s\\n' executed > \"$README_INSTALLER_MARKER\"\n")
			if err := os.WriteFile(filepath.Join(fixture, "install.sh"), markerInstaller, 0700); err != nil {
				t.Fatal(err)
			}
			markerSum := sha256.Sum256(markerInstaller)
			if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  install.sh\n%x  codator-linux-amd64\n", markerSum, sum)), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "secure rejected")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			rejectedEnv := buildEnv(secureEnv, nil, map[string]string{
				"CODATOR_INSTALL_DIR":     dir,
				"FAKE_GH_LOG":             filepath.Join(root, "secure-rejected.gh-log"),
				"FAKE_GH_MODE":            "reject",
				"README_INSTALLER_MARKER": marker,
			})
			rejectedCmd := exec.Command("/bin/sh", "-c", bootstrap)
			rejectedCmd.Env = rejectedEnv
			output, err := rejectedCmd.CombinedOutput()
			if err == nil {
				t.Fatalf("security bootstrap accepted a rejected manifest: %s", output)
			}
			assertAttestationArgs(t, filepath.Join(root, "secure-rejected.gh-log"), "v9.9.9", 1)
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("rejected manifest executed installer: stat=%v", statErr)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("rejected manifest changed old destination to %q, err %v", got, readErr)
			}
		})

		t.Run("installer checksum mismatch does not execute installer", func(t *testing.T) {
			marker := filepath.Join(root, "secure-mismatched-installer-executed")
			dir := filepath.Join(root, "secure mismatched")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte("old binary\n")
			if err := os.WriteFile(filepath.Join(dir, "codator"), old, 0700); err != nil {
				t.Fatal(err)
			}
			mismatchInstaller := []byte("#!/bin/sh\nprintf '%s\\n' executed > \"$README_INSTALLER_MARKER\"\n")
			if err := os.WriteFile(filepath.Join(fixture, "install.sh"), mismatchInstaller, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS"), []byte(fmt.Sprintf("%x  install.sh\n%x  codator-linux-amd64\n", installSum, sum)), 0600); err != nil {
				t.Fatal(err)
			}
			mismatchEnv := buildEnv(secureEnv, nil, map[string]string{
				"CODATOR_INSTALL_DIR":     dir,
				"FAKE_GH_LOG":             filepath.Join(root, "secure-mismatch.gh-log"),
				"FAKE_GH_MODE":            "",
				"README_INSTALLER_MARKER": marker,
			})
			mismatchCmd := exec.Command("/bin/sh", "-c", bootstrap)
			mismatchCmd.Env = mismatchEnv
			output, err := mismatchCmd.CombinedOutput()
			if err == nil {
				t.Fatalf("security bootstrap accepted a checksum-mismatched installer: %s", output)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("checksum-mismatched installer executed: stat=%v", statErr)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "codator"))
			if readErr != nil || string(got) != string(old) {
				t.Fatalf("checksum-mismatched installer changed old destination to %q, err %v", got, readErr)
			}
		})
	})

	t.Run("complete installer download failure never executes it", func(t *testing.T) {
		marker := filepath.Join(root, "failed-installer-executed")
		rejectedInstaller := []byte("#!/bin/sh\nprintf '%s\\n' executed > \"$README_INSTALLER_MARKER\"\n")
		if err := os.WriteFile(filepath.Join(fixture, "install.sh"), rejectedInstaller, 0700); err != nil {
			t.Fatal(err)
		}
		rejectedDir := filepath.Join(root, "failed install")
		rejectedEnv := buildEnv(env, nil, map[string]string{
			"CODATOR_INSTALL_DIR":     rejectedDir,
			"FAKE_CURL_MODE":          "partial",
			"README_INSTALLER_MARKER": marker,
		})
		rejectedCmd := exec.Command("/bin/sh", "-c", bootstrap)
		rejectedCmd.Env = rejectedEnv
		output, err := rejectedCmd.CombinedOutput()
		if err == nil {
			t.Fatalf("README bootstrap accepted an incomplete installer: %s", output)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("incomplete installer executed: stat=%v", statErr)
		}
		if _, statErr := os.Stat(rejectedDir); !os.IsNotExist(statErr) {
			t.Fatalf("incomplete bootstrap touched install directory: %v", statErr)
		}
	})
}

func assertAttestationArgs(t *testing.T, logPath, tag string, verifies int) {
	t.Helper()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read GitHub CLI log: %v", err)
	}
	text := string(log)
	if got := strings.Count(text, "attestation verify"); got != verifies {
		t.Fatalf("attestation verifies=%d, want %d; log=%q", got, verifies, text)
	}
	if verifies > 0 && !strings.Contains(text, "/SHA256SUMS") {
		t.Fatalf("attestation did not verify the signed manifest: %q", text)
	}
	for _, want := range []string{
		"--bundle",
		"--repo olafurns7/codator",
		"--cert-identity https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/" + tag,
		"--source-ref refs/tags/" + tag,
		"--deny-self-hosted-runners",
		"--hostname github.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("GitHub CLI log missing %q: %q", want, text)
		}
	}
}

func assertNoGitHubCLICall(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("default path invoked GitHub CLI: stat=%v", err)
	}
}

func assertNoBinaryExecution(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("release binary executed before verification: stat=%v", err)
	}
}

func makeIsolatedInstallPath(t *testing.T, root, fakeBin string) string {
	t.Helper()
	isolated := filepath.Join(root, "isolated-path")
	if err := os.Mkdir(isolated, 0700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"awk", "chmod", "cp", "mkdir", "mktemp", "mv", "rm", "sed", "sh"} {
		path, err := exec.LookPath(command)
		if err != nil {
			t.Fatalf("find %s: %v", command, err)
		}
		if err := os.Symlink(path, filepath.Join(isolated, command)); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"curl", "gh", "uname"} {
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
