package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeEnvAndArgsSafety(t *testing.T) {
	env := envMap(claudeEnv("profiles/work", []string{
		"PATH=/usr/bin", "TERM=xterm", "GITHUB_TOKEN=keep",
		"CLAUDE_CONFIG_DIR=/tmp/other", "ANTHROPIC_API_KEY=fake",
		"ANTHROPIC_VENDOR_API_KEY=fake", "CLAUDE_CODE_FUTURE_OAUTH_TOKEN=fake",
		"CLAUDE_CODE_USE_FOUNDRY=1", "AWS_SECRET_ACCESS_KEY=fake",
	}))
	if !filepath.IsAbs(env["CLAUDE_CONFIG_DIR"]) || env["CLAUDE_CONFIG_DIR"] == "/tmp/other" {
		t.Fatalf("config dir was not forced absolute: %q", env["CLAUDE_CONFIG_DIR"])
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_VENDOR_API_KEY", "CLAUDE_CODE_FUTURE_OAUTH_TOKEN", "CLAUDE_CODE_USE_FOUNDRY", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := env[key]; ok {
			t.Fatalf("sensitive environment key %q survived", key)
		}
	}
	if env["PATH"] != "/usr/bin" || env["TERM"] != "xterm" || env["GITHUB_TOKEN"] != "keep" {
		t.Fatalf("ordinary environment was not preserved: %#v", env)
	}

	for _, args := range [][]string{
		{"--console"}, {"--api-key=fake"}, {"--bare"}, {"--settings", "/tmp/settings.json"},
		{"--setting-sources=project"}, {"--env", "ANTHROPIC_API_KEY=fake"},
		{"--provider=bedrock"}, {"--base-url=https://example.invalid"},
	} {
		if err := validateClaudeLoginArgs(args); err == nil {
			t.Errorf("login args %q were accepted", args)
		}
	}
	if err := validateClaudeLoginArgs([]string{"--", "--console"}); err != nil {
		t.Fatalf("login separator was ignored: %v", err)
	}
	for _, args := range [][]string{
		{"--settings=/tmp/settings.json"}, {"--env", "ANTHROPIC_API_KEY=fake"},
		{"--no-safe-mode"}, {"--safe-mode=false"}, {"--remote"}, {"--bare"}, {"auth", "login"},
	} {
		if err := validateClaudeLaunchArgs(args); err == nil {
			t.Errorf("launch args %q were accepted", args)
		}
	}
	if err := validateClaudeLaunchArgs([]string{"-p", "print the literal text --settings and --remote"}); err != nil {
		t.Fatalf("harmless prompt text was rejected: %v", err)
	}
	if err := validateClaudeLaunchArgs([]string{"--", "--settings=/tmp/settings.json"}); err != nil {
		t.Fatalf("launch separator was ignored: %v", err)
	}
	if got := strings.Join(claudeCLIArgs("--model", "sonnet"), " "); got != "--safe-mode --model sonnet" {
		t.Fatalf("ordinary launch args = %q", got)
	}
	if got := strings.Join(claudeCLIArgs("auth", "login"), " "); got != "auth login" {
		t.Fatalf("login args = %q", got)
	}
}

func TestParseClaudeQuotaUnknownZeroAndStale(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	future := "2026-09-13T12:00:00Z"
	tests := []struct {
		name         string
		rates        string
		wantEligible bool
		wantKnown    bool
		wantHeadroom float64
	}{
		{"zero with unstarted reset", "\"rate_limits\":{\"five_hour\":{\"utilization\":0,\"resets_at\":null}}", true, true, 100},
		{"missing utilization", "\"rate_limits\":{\"five_hour\":{}}", false, false, 0},
		{"missing reset with nonzero use", "\"rate_limits\":{\"five_hour\":{\"utilization\":20,\"resets_at\":null}}", false, false, 0},
		{"stale nonzero window", "\"rate_limits\":{\"five_hour\":{\"utilization\":20,\"resets_at\":\"2026-09-12T11:00:00Z\"}}", false, false, 0},
		{"exhausted window", "\"rate_limits\":{\"five_hour\":{\"utilization\":100,\"resets_at\":\"" + future + "\"}}", false, true, 0},
		{"absent windows", "\"rate_limits\":{\"five_hour\":null,\"seven_day\":null,\"model_scoped\":[]}", false, false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage("{\"subscription_type\":\"max\",\"rate_limits_available\":true," + test.rates + "}")
			got, err := parseClaudeQuota(raw, "", false, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Eligible != test.wantEligible || got.Known != test.wantKnown || got.Headroom != test.wantHeadroom {
				t.Fatalf("quota = %#v", got)
			}
			if !got.Subscription {
				t.Fatalf("verified plan was not retained: %#v", got)
			}
		})
	}

	malformed, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"team\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":\"bad\"}}}"), "", false, now)
	if err != nil || !malformed.Subscription || malformed.Eligible || malformed.Known {
		t.Fatalf("malformed usage did not preserve subscription: %#v, %v", malformed, err)
	}
	unavailable, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"team\",\"rate_limits_available\":false,\"rate_limits\":null}"), "", false, now)
	if err != nil || !unavailable.Subscription || unavailable.Eligible || unavailable.Known {
		t.Fatalf("unavailable quota lost verified subscription: %#v, %v", unavailable, err)
	}
	unknownPlan, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"free\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":0}}}"), "", false, now)
	if err != nil || unknownPlan.Subscription || unknownPlan.Eligible {
		t.Fatalf("unknown plan was accepted: %#v, %v", unknownPlan, err)
	}
}

func TestClaudeModelWindowSelection(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage("{\"subscription_type\":\"max\",\"rate_limits_available\":true,\"rate_limits\":{" +
		"\"five_hour\":{\"utilization\":10,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"seven_day_opus\":{\"utilization\":100,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"seven_day_sonnet\":{\"utilization\":30,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"model_scoped\":[" +
		"{\"display_name\":\"Opus\",\"utilization\":100,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"{\"display_name\":\"Sonnet\",\"utilization\":25,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"{\"display_name\":\"Haiku\",\"utilization\":15,\"resets_at\":\"2026-09-13T12:00:00Z\"}]}}")
	defaultModel, err := parseClaudeQuota(raw, "", false, now)
	if err != nil || defaultModel.Eligible || !defaultModel.Known {
		t.Fatalf("default model ignored an exhausted model window: %#v, %v", defaultModel, err)
	}
	sonnet, err := parseClaudeQuota(raw, "claude-sonnet-4-5", true, now)
	if err != nil || !sonnet.Eligible || sonnet.Headroom != 70 {
		t.Fatalf("explicit sonnet quota = %#v, %v", sonnet, err)
	}
	haiku, err := parseClaudeQuota(raw, "claude-haiku-4-5", true, now)
	if err != nil || !haiku.Eligible || haiku.Headroom != 85 {
		t.Fatalf("explicit haiku quota = %#v, %v", haiku, err)
	}

	ambiguous := json.RawMessage("{\"subscription_type\":\"pro\",\"rate_limits_available\":true,\"rate_limits\":{" +
		"\"five_hour\":{\"utilization\":10,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"model_scoped\":[{\"display_name\":\"weekly\",\"utilization\":80,\"resets_at\":\"2026-09-13T12:00:00Z\"}]}}")
	conservative, err := parseClaudeQuota(ambiguous, "sonnet", true, now)
	if err != nil || !conservative.Eligible || conservative.Headroom != 20 {
		t.Fatalf("ambiguous model window was ignored: %#v, %v", conservative, err)
	}
}

func TestClaudeControlResponseMatchingAndErrors(t *testing.T) {
	other := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"other\"}}")
	if _, matched, err := decodeClaudeControlFrame(other, "init-1"); err != nil || matched {
		t.Fatalf("unmatched response = matched %v, err %v", matched, err)
	}
	ok := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":{\"subscription_type\":\"pro\"}}}")
	payload, matched, err := decodeClaudeControlFrame(ok, "usage-1")
	if err != nil || !matched || string(payload) != "{\"subscription_type\":\"pro\"}" {
		t.Fatalf("matching response = %s, matched %v, err %v", payload, matched, err)
	}
	failed := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"error\",\"request_id\":\"usage-1\",\"error\":\"secret-token\"}}")
	_, matched, err = decodeClaudeControlFrame(failed, "usage-1")
	if !matched || err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error response leaked native details: matched %v, err %v", matched, err)
	}
}

func TestProbeClaudeHandshakeAndNoUserFrame(t *testing.T) {
	root := t.TempDir()
	profile, bin := filepath.Join(root, "profile"), filepath.Join(root, "bin")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"set -e\n" +
		"[ \"$#\" -eq 10 ] && [ \"$1\" = \"--print\" ] && [ \"$6\" = \"--verbose\" ]\n" +
		"[ \"$7\" = \"--safe-mode\" ] && [ \"$8\" = \"--no-session-persistence\" ]\n" +
		"[ \"$9\" = \"--tools\" ]\n" +
		"shift 9\n" +
		"[ -z \"$1\" ]\n" +
		"case \"$CLAUDE_CONFIG_DIR\" in /*) ;; *) exit 91 ;; esac\n" +
		"[ \"$PWD\" = \"$CLAUDE_CONFIG_DIR\" ]\n" +
		"[ -z \"$ANTHROPIC_API_KEY\" ]\n" +
		"[ \"$DO_NOT_TRACK\" = 1 ] && [ \"$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\" = 1 ]\n" +
		"IFS= read -r init\n" +
		"printf '%s\\n' \"$init\" >> \"$CLAUDE_CONFIG_DIR/frames\"\n" +
		"case \"$init\" in *'\"subtype\":\"initialize\"'*) ;; *) exit 92 ;; esac\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"wrong\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"init-1\"}}'\n" +
		"IFS= read -r usage\n" +
		"printf '%s\\n' \"$usage\" >> \"$CLAUDE_CONFIG_DIR/frames\"\n" +
		"case \"$usage\" in *'\"subtype\":\"get_usage\"'*'\"skip_behaviors\":true'*) ;; *) exit 93 ;; esac\n" +
		"/bin/sleep 60 &\n" +
		"printf '%s\\n' \"$!\" > \"$CLAUDE_CONFIG_DIR/child.pid\"\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":{\"subscription_type\":\"max\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":0,\"resets_at\":null},\"seven_day\":{\"utilization\":15,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"model_scoped\":[]}}}}'\n" +
		"if IFS= read -r extra; then printf '%s\\n' \"$extra\" >> \"$CLAUDE_CONFIG_DIR/frames\"; exit 94; fi\n"
	native := filepath.Join(bin, "claude")
	if err := os.WriteFile(native, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-test-value")
	account := Account{Provider: "claude", Name: "synthetic", NativeDir: profile}
	got, err := probeClaude(account, "", false)
	if err != nil || !got.Eligible || !got.Subscription || got.Headroom != 85 {
		t.Fatalf("probe result = %#v, err %v", got, err)
	}
	frames, err := os.ReadFile(filepath.Join(profile, "frames"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(frames)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "\"subtype\":\"initialize\"") ||
		!strings.Contains(lines[1], "\"subtype\":\"get_usage\"") || !strings.Contains(lines[1], "\"skip_behaviors\":true") {
		t.Fatalf("probe sent unexpected protocol frames: %q", frames)
	}
	if strings.Contains(string(frames), "\"type\":\"user\"") {
		t.Fatalf("probe sent a user frame: %q", frames)
	}
	childBytes, err := os.ReadFile(filepath.Join(profile, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(childBytes)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processRunning(pid) {
		t.Fatalf("Claude probe descendant %d survived cleanup", pid)
	}
}
