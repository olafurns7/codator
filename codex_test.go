package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexBucketSelection(t *testing.T) {
	standard := &codexRateLimitSnapshot{LimitID: stringPtr("codex")}
	alternate := &codexRateLimitSnapshot{NormalModelSlug: stringPtr("gpt-5-codex")}
	usage := codexUsageResponse{RateLimits: standard, ByLimitID: map[string]*codexRateLimitSnapshot{"codex": standard, "gpt-5-codex": alternate}}
	got, err := codexSnapshotForModel("", false, usage)
	if err != nil || got != standard {
		t.Fatalf("default bucket=%p err=%v", got, err)
	}
	got, err = codexSnapshotForModel("gpt-5-codex", true, usage)
	if err != nil || got != alternate {
		t.Fatalf("alternate bucket=%p err=%v", got, err)
	}
	if _, err := codexSnapshotForModel("unknown", true, usage); err == nil {
		t.Fatal("accepted an unmapped model")
	}
	if _, err := codexSnapshotForModel("gpt-5-codex", true, codexUsageResponse{RateLimits: alternate}); err == nil {
		t.Fatal("used the compatibility bucket to guess an explicit model")
	}
}

func TestCodexZeroAndStaleWindows(t *testing.T) {
	zero := 0.0
	resetZero := 0.0
	ordinary := true
	for _, resetsAt := range []*float64{nil, &resetZero} {
		valid := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &zero, ResetsAt: resetsAt}}
		used, ok := codexWindowPercentages(valid)
		if !ok || len(used) != 1 || evaluateUsage(&ordinary, nil, used).Headroom != 100 {
			t.Fatalf("valid zero window used=%v ok=%v resetsAt=%v", used, ok, resetsAt)
		}
	}
	missing := &codexRateLimitSnapshot{Primary: &codexRateWindow{ResetsAt: nil}}
	if _, ok := codexWindowPercentages(missing); ok {
		t.Fatal("treated missing utilization as zero")
	}
	usedPercent, past := 10.0, 1.0
	stale := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &usedPercent, ResetsAt: &past}}
	if _, ok := codexWindowPercentages(stale); ok {
		t.Fatal("accepted a stale nonzero usage window")
	}
}

func TestCodexAppServerProbeUsesLocalProtocolFixture(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("codex", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.Chmod(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(binDir, "codex")
	cwdFile := filepath.Join(binDir, "probe-cwd")
	baseURLFile := filepath.Join(binDir, "inherited-base-url")
	argsFile := filepath.Join(binDir, "probe-args")
	t.Setenv("CODATOR_PROBE_CWD_FILE", cwdFile)
	t.Setenv("CODATOR_BASE_URL_FILE", baseURLFile)
	t.Setenv("CODATOR_PROBE_ARGS_FILE", argsFile)
	t.Setenv("OPENAI_BASE_URL", "https://attacker.example")
	script := `#!/bin/sh
pwd -P > "$CODATOR_PROBE_CWD_FILE"
printf '%s\n' "${OPENAI_BASE_URL-}" > "$CODATOR_BASE_URL_FILE"
printf '%s\n' "$@" > "$CODATOR_PROBE_ARGS_FILE"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","email":null,"planType":"plus"},"requiresOpenaiAuth":true}}' ;;
	  *'"method":"account/rateLimits/read"'*)
	    if [ "${CODATOR_RATE_LIMITS_ERROR-}" = 1 ]; then
	      printf '%s\n' '{"id":3,"error":{"code":-1,"message":"fixture failure"}}'
	    else
	      response='{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}'
	      if [ "${CODATOR_SPEND_UNKNOWN-}" = 1 ]; then
	        response=$(printf '%s\n' "$response" | sed 's/"spendControlReached":false/"spendControlReached":null/g')
	      fi
	      printf '%s\n' "$response"
	    fi
	    ;;
  esac
done
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	quota, err := probeCodex(account, "", false)
	if err != nil || !quota.Eligible || quota.Headroom != 40 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	t.Setenv("CODATOR_RATE_LIMITS_ERROR", "1")
	quota, err = probeCodex(account, "", false)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason == "" {
		t.Fatalf("rate-limit failure lost verified identity: quota=%+v err=%v", quota, err)
	}
	t.Setenv("CODATOR_RATE_LIMITS_ERROR", "")
	t.Setenv("CODATOR_SPEND_UNKNOWN", "1")
	quota, err = probeCodex(account, "", false)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason != "spend control status is unknown" {
		t.Fatalf("unknown spend-control status was treated as eligible: quota=%+v err=%v", quota, err)
	}
	if got, err := os.ReadFile(cwdFile); err != nil || strings.TrimSpace(string(got)) != account.NativeDir {
		t.Fatalf("probe cwd=%q err=%v, want %q", got, err, account.NativeDir)
	}
	if got, err := os.ReadFile(baseURLFile); err != nil || strings.TrimSpace(string(got)) != "" {
		t.Fatalf("probe inherited OPENAI_BASE_URL: %q err=%v", got, err)
	}
	gotArgs, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(gotArgs), `model_provider="openai"`) || !strings.Contains(string(gotArgs), `openai_base_url="https://chatgpt.com/backend-api/codex"`) {
		t.Fatalf("probe args=%q err=%v", gotArgs, err)
	}
}

func TestCodexFlagsBlockIdentityOverrides(t *testing.T) {
	for _, args := range [][]string{{"--remote"}, {"-c", "model=other"}, {"-cmodel=other"}, {"--profile=other"}, {"-p", "other"}, {"-pother"}, {"-p=other"}, {"--with-api-key"}} {
		if err := validateCodexLaunchArgs(args); err == nil {
			t.Errorf("accepted unsafe args %q", args)
		}
	}
	if err := validateCodexLaunchArgs([]string{"--", "-c", "literal prompt"}); err != nil {
		t.Fatalf("blocked positional args after --: %v", err)
	}
	if !strings.Contains(codexFileConfig, "cli_auth_credentials_store") {
		t.Fatal("missing isolated file credential store")
	}
	if got := strings.Join(codexCLIArgs("login"), " "); !strings.Contains(got, `model_provider="openai"`) || !strings.Contains(got, `openai_base_url="https://chatgpt.com/backend-api/codex"`) || !strings.Contains(got, `chatgpt_base_url="https://chatgpt.com/backend-api"`) || !strings.Contains(got, `requires_openai_auth=true`) {
		t.Fatalf("subscription provider overrides missing: %s", got)
	}
}

func TestCodexEnvRemovesAuthAndEndpointOverrides(t *testing.T) {
	base := []string{
		"OPENAI_BASE_URL=https://attacker.example",
		"CODEX_REFRESH_TOKEN_URL_OVERRIDE=https://attacker.example/token",
		"CODEX_REVOKE_TOKEN_URL_OVERRIDE=https://attacker.example/revoke",
		"CODEX_APP_SERVER_LOGIN_CLIENT_ID=attacker",
		"CODEX_APP_SERVER_LOGIN_ISSUER=https://attacker.example",
		"CODEX_ACCESS_TOKEN=secret",
		"OPENAI_WORKLOAD_IDENTITY_CONTEXT=metadata",
	}
	got := envMap(codexEnv("/private/profile", base))
	for _, key := range []string{"OPENAI_BASE_URL", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID", "CODEX_APP_SERVER_LOGIN_ISSUER", "CODEX_ACCESS_TOKEN"} {
		if _, ok := got[key]; ok {
			t.Errorf("inherited %s", key)
		}
	}
	if got["CODEX_HOME"] != "/private/profile" || got["CODEX_SQLITE_HOME"] != "/private/profile" || got["OPENAI_WORKLOAD_IDENTITY_CONTEXT"] != "metadata" {
		t.Fatalf("forced or allowed environment is wrong: %+v", got)
	}
}

func stringPtr(value string) *string { return &value }
