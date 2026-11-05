package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const claudeStatusProcessSettings = `{"model":"opus[1m]","advisorModel":"fable","modelSettings":{"claude-opus-5-5":{"effortLevel":"xhigh"}},"switchModelsOnFlag":false}`

func claudeStatusProcessPayload(session, weekly, fable int, sessionReset, weeklyReset, fableReset time.Time) string {
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	return fmt.Sprintf(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":%d,"resets_at":%q},"seven_day":{"utilization":%d,"resets_at":%q},"model_scoped":[{"display_name":"Fable","utilization":%d,"resets_at":%q}]}}`,
		session, f(sessionReset), weekly, f(weeklyReset), fable, f(fableReset))
}

func installClaudeStatusProcessFake(t *testing.T, binDir string) {
	t.Helper()
	initFrame := `{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}`
	script := "#!/bin/sh\nprofile=${CLAUDE_CONFIG_DIR%/native}\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '2.1.280 (Claude Code)'; exit 0; fi\n" +
		"if [ \"$1\" = \"--print\" ]; then\n  printf 'probe\\n' >> \"$profile/.probe-count\"\n  while IFS= read -r line; do\n    case \"$line\" in\n" +
		"      *init-1*) printf '%s\\n' " + shellQuote(initFrame) + " ;;\n" +
		"      *usage-1*) printf '%s' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":'; cat \"$profile/.payload\"; printf '}}\\n'; exit 0 ;;\n" +
		"    esac\n  done\n  exit 92\nfi\nprintf 'launch\\n' >> \"$profile/.launch-count\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func newClaudeStatusProcessProfiles(t *testing.T, payloads map[string]string) (dataHome, home, binDir string, accounts map[string]Account) {
	t.Helper()
	dataHome = tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	accounts = map[string]Account{}
	for name, payload := range payloads {
		a, err := store.EnsureAccount("claude", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.NativeDir, "settings.json"), []byte(claudeStatusProcessSettings), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(a.NativeDir), ".payload"), []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		accounts[name] = a
	}
	home = t.TempDir()
	binDir = filepath.Join(home, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	installClaudeStatusProcessFake(t, binDir)
	return
}

func TestClaudeStatusAvailableAtFollowsDefaultModel(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	session, weekly, fable := now.Add(2*time.Hour), now.Add(72*time.Hour), now.Add(120*time.Hour)
	dataHome, home, binDir, _ := newClaudeStatusProcessProfiles(t, map[string]string{"a": claudeStatusProcessPayload(100, 85, 100, session, weekly, fable)})
	code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "--json", "claude"})
	var report statusReport
	if code != 0 || json.Unmarshal([]byte(stdout), &report) != nil || len(report.Accounts) != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	row := report.Accounts[0]
	t.Logf("state=%s launchable=%v reason=%q available_at=%v (session reset %v, fable reset %v)", row.State, row.Launchable, row.Reason, row.AvailableAt, session, fable)
	if row.State != "exhausted" || row.Launchable || row.AvailableAt == nil || !row.AvailableAt.Equal(session) {
		t.Errorf("available_at=%v, want the session reset %v: the Fable window does not gate this Opus default", row.AvailableAt, session)
	}
}
