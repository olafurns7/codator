package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func viewState() (watchState, time.Time, time.Time) {
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	now := wall
	var s watchState
	s.finish(watchFixture(wall), nil, now, wall)
	return s, now, wall
}

func TestWatchViewRawIdentitySelectionAndReveal(t *testing.T) {
	s, now, wall := viewState()
	r := s.snapshot.report
	a, b := r.Accounts[0], r.Accounts[0]
	a.Account = "same\n"
	b.Account = "same\r"
	a.Reason = "first raw account"
	b.Reason = "second raw account"
	a.Windows = append(a.Windows, statusWindow{Label: "extra", Unavailable: "private note"})
	other := a
	other.Provider = "claude"
	other.Reason = "other provider"
	r.Accounts = []statusAccount{a, b, other}
	r.Providers[0].Status = "unavailable"
	r.Providers[0].Error = "provider message"
	s.finish(r, nil, now, wall)
	s.view.selectNext(&s, 1)
	if s.view.selected != watchID(b) {
		t.Fatal("selection lost raw identity")
	}
	detail := paintLines(s.view.detailLines(&s, now, wall), false)
	if !strings.Contains(detail, "second raw account") || strings.Contains(detail, "first raw account") {
		t.Fatal(detail)
	}
	lines, locations := s.tableLayout(now, wall, s.view.visible, true)
	if locations[watchID(b)] <= locations[watchID(a)]+1 || locations[watchID(other)] == locations[watchID(a)] {
		t.Fatal("variable notes or provider identities lost", locations)
	}
	if !strings.Contains(lines[locations[watchID(b)]].paint(false), "same?") {
		t.Fatal("typed row position incorrect")
	}
	table := watchViewport{x: 100, y: 100}
	s.view.reveal = true
	watchFrame(&s, &table, 40, 10, now, wall, false)
	row := locations[watchID(b)] + 1
	if table.x != 0 || row < table.y || row >= table.y+5 {
		t.Fatal("selection not revealed", table, row)
	}
	table.x = 5
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table.x != 5 {
		t.Fatal("manual horizontal pan overridden")
	}
	// Refresh reorders source and changes note lengths; selection remains raw.
	r.Accounts = []statusAccount{other, b, a}
	r.Accounts[2].Reason = "new note length"
	s.finish(r, nil, now, wall)
	if s.view.selected != watchID(b) {
		t.Fatal("refresh/reorder moved selection")
	}
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table.x != 5 {
		t.Fatal("refresh overrode manual horizontal pan")
	}
	s.view.sortMode = 1
	s.view.rebuild(&s, now, wall)
	if s.view.selected != watchID(b) {
		t.Fatal("sort moved selection")
	}
	s.view.name = "missing"
	s.view.rebuild(&s, now, wall)
	if s.view.hasSelection || len(s.view.visible) != 0 {
		t.Fatal("empty filter retains selection")
	}
	s.view.name = ""
	s.view.rebuild(&s, now, wall)
	s.view.selectNext(&s, 2)
	if s.view.selected != watchID(other) {
		t.Fatal("same label across provider collided")
	}
	r.Accounts = []statusAccount{a, b}
	s.finish(r, nil, now, wall)
	if s.view.selected != watchID(a) {
		t.Fatal("removed account didn't select first visible")
	}
}

func TestWatchViewRefreshPreservesManualPan(t *testing.T) {
	s, now, wall := viewState()
	var table watchViewport
	watchFrame(&s, &table, 40, 10, now, wall, false)
	s.view.handle("end", &s, &table, 40, 10, now, wall)
	table.x = 4
	watchFrame(&s, &table, 40, 10, now, wall, false)
	before, selected := table, s.view.selected
	s.finish(s.snapshot.report, nil, now, wall)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table != before || s.view.selected != selected {
		t.Fatalf("identical refresh interrupted manual pan: %+v -> %+v, selection %v -> %v", before, table, selected, s.view.selected)
	}
	_, oldLocations := s.tableLayout(now, wall, s.view.visible, true)
	r := s.snapshot.report
	r.Accounts = append([]statusAccount(nil), r.Accounts...)
	for i, j := 0, len(r.Accounts)-1; i < j; i, j = i+1, j-1 {
		r.Accounts[i], r.Accounts[j] = r.Accounts[j], r.Accounts[i]
	}
	extra := r.Accounts[len(r.Accounts)-1]
	extra.Account = "aa"
	extra.Reason = "new note before selection"
	extra.Windows = append(append([]statusWindow(nil), extra.Windows...), extra.Windows...)
	r.Accounts = append(r.Accounts, extra)
	s.finish(r, nil, now, wall)
	_, newLocations := s.tableLayout(now, wall, s.view.visible, true)
	if newLocations[selected] == oldLocations[selected] {
		t.Fatal("fixture didn't change selected row offset")
	}
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table != before || s.view.selected != selected {
		t.Fatalf("reordered/changed-note refresh moved pan: %+v -> %+v", before, table)
	}
	// Shrinking content may clamp the viewport, but must not reveal selection.
	a, ok := s.view.account(&s)
	if !ok {
		t.Fatal("selection missing")
	}
	r.Accounts = []statusAccount{a}
	s.finish(r, nil, now, wall)
	lines, _ := s.tableLayout(now, wall, s.view.visible, true)
	expected := before
	expected.clamp(append(lines, s.view.help(&s)...), 39, 5)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table != expected || table.y >= before.y || s.view.selected != selected {
		t.Fatalf("shrink didn't use normal clamp: got %+v want %+v", table, expected)
	}
}

func TestWatchViewRefreshKeepsPendingExplicitReveal(t *testing.T) {
	s, now, wall := viewState()
	var table watchViewport
	watchFrame(&s, &table, 40, 10, now, wall, false)
	s.view.handle("end", &s, &table, 40, 10, now, wall)
	table.x = 4
	watchFrame(&s, &table, 40, 10, now, wall, false)
	s.view.handle("text:]", &s, &table, 40, 10, now, wall)
	r := s.snapshot.report
	r.Accounts = append([]statusAccount(nil), r.Accounts...)
	// A note on the preceding busy account moves the selected unknown row.
	r.Accounts[1].Reason = "new note before selected row"
	s.finish(r, nil, now, wall)
	if !s.view.reveal || !s.view.revealAccount {
		t.Fatal("refresh cleared pending explicit reveal")
	}
	_, locations := s.tableLayout(now, wall, s.view.visible, true)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	row := locations[s.view.selected] + 1
	if table.x != 0 || row < table.y || row >= table.y+5 {
		t.Fatal("pending reveal didn't use current metadata", table, row)
	}
	// Removal replaces selection and must reveal the replacement account.
	s.view.handle("end", &s, &table, 40, 10, now, wall)
	table.x = 4
	watchFrame(&s, &table, 40, 10, now, wall, false)
	r.Accounts = append(r.Accounts[:2:2], r.Accounts[3:]...)
	s.finish(r, nil, now, wall)
	if s.view.selected.account != "busy" || !s.view.reveal || !s.view.revealAccount {
		t.Fatal("removed selection wasn't replaced/revealed")
	}
	_, locations = s.tableLayout(now, wall, s.view.visible, true)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	row = locations[s.view.selected] + 1
	if table.x != 0 || row < table.y || row >= table.y+5 {
		t.Fatal("replacement reveal incorrect", table, row)
	}
}

func TestWatchViewUserActionsStillRequestReveal(t *testing.T) {
	for _, keys := range [][]string{{"text:s"}, {"text:p"}, {"text:f"}, {"text:/", "enter"}, {"enter"}} {
		s, now, wall := viewState()
		var table watchViewport
		watchFrame(&s, &table, 40, 10, now, wall, false)
		s.view.handle("end", &s, &table, 40, 10, now, wall)
		watchFrame(&s, &table, 40, 10, now, wall, false)
		for _, key := range keys {
			s.view.handle(key, &s, &table, 40, 10, now, wall)
		}
		if !s.view.reveal {
			t.Fatal("user action lost reveal", keys)
		}
		s.finish(s.snapshot.report, nil, now, wall)
		if !s.view.reveal {
			t.Fatal("refresh cleared user action reveal", keys)
		}
		_, locations := s.tableLayout(now, wall, s.view.visible, true)
		watchFrame(&s, &table, 40, 10, now, wall, false)
		row := locations[s.view.selected] + 1
		if row < table.y || row >= table.y+5 {
			t.Fatal("user action didn't reveal current row", keys, table, row)
		}
	}
}

func TestStatusPanelTypedOffsetsKeepBytes(t *testing.T) {
	_, _, wall := viewState()
	r := watchFixture(wall)
	rows := r.Accounts[:3]
	for _, p := range []statusProvider{{Provider: "codex", Status: "ok"}, {Provider: "codex", Status: "unavailable", Error: "reason"}} {
		lines, positions := statusPanelLayout(p, rows, wall)
		if !reflect.DeepEqual(lines, statusPanelLines(p, rows, wall)) || len(positions) != len(rows) {
			t.Fatal("compatibility layout changed")
		}
		for i, pos := range positions {
			if !strings.Contains(lines[pos].paint(false), rows[i].Account) {
				t.Fatal("bad row offset", positions)
			}
			if i == 1 && pos <= positions[i-1]+1 {
				t.Fatal("notes weren't counted")
			}
		}
	}
}

func TestWatchViewSortFilterAndImmutableSnapshot(t *testing.T) {
	s, now, wall := viewState()
	before, _ := json.Marshal(s.snapshot.report)
	next := s.nextAccount("codex", now, wall)
	s.view.sortMode = 1
	s.view.rebuild(&s, now, wall)
	if s.snapshot.report.Accounts[s.view.visible[0]].Account != "work" {
		t.Fatal("known headroom didn't precede unknown")
	}
	s.view.providerFilter = 1
	s.view.stateFilter = 1
	s.view.name = "WORK"
	s.view.rebuild(&s, now, wall)
	if len(s.view.visible) != 1 || s.snapshot.report.Accounts[s.view.visible[0]].Account != "work" {
		t.Fatal("intersected filters failed")
	}
	filtered, _ := s.tableLayout(now, wall, s.view.visible, true)
	display := paintLines(filtered, false)
	if strings.Contains(display, "╭─ claude") || strings.Contains(display, "claude: Next") || !strings.Contains(display, "codex: Next from snapshot (estimate): "+next) {
		t.Fatal("provider filter leaked section or changed estimate", display)
	}
	s.view.detailLines(&s, now, wall)
	after, _ := json.Marshal(s.snapshot.report)
	if string(before) != string(after) || next != s.nextAccount("codex", now, wall) {
		t.Fatal("view mutated report/estimate")
	}
	s.view.stateFilter = 2
	s.view.rebuild(&s, now, wall)
	if len(s.view.visible) != 0 {
		t.Fatal("state intersection ignored")
	}
	s.view.name = "busy"
	s.view.rebuild(&s, now, wall)
	filtered, _ = s.tableLayout(now, wall, s.view.visible, true)
	if len(s.view.visible) != 1 || !strings.Contains(paintLines(filtered, false), "codex: Next from snapshot (estimate): work") {
		t.Fatal("filtered-out best account changed displayed estimate")
	}
	s.view.name = ""
	s.view.stateFilter = 0
	r := s.snapshot.report
	a := r.Accounts[0]
	a.Account = "aa"
	b := a
	b.Account = "zz"
	bad := a
	bad.Account = "ab"
	n := math.NaN()
	bad.HeadroomPercent = &n
	r.Accounts = []statusAccount{b, bad, a}
	s.finish(r, nil, now, wall)
	if got := []string{r.Accounts[s.view.visible[0]].Account, r.Accounts[s.view.visible[1]].Account, r.Accounts[s.view.visible[2]].Account}; !reflect.DeepEqual(got, []string{"aa", "zz", "ab"}) {
		t.Fatal("ties/nonfinite", got)
	}
	// Age/clock/provider failure invalidate metrics when an ordering event occurs.
	for _, mode := range []int{1, 2} {
		s.view.sortMode = mode
		s.view.rebuild(&s, now.Add(300*time.Second), wall.Add(300*time.Second))
		if r.Accounts[s.view.visible[1]].Account != "ab" {
			t.Fatal("stale metrics not unknown")
		}
	}
	s.errorText = "failed"
	s.view.rebuild(&s, now, wall)
	if r.Accounts[s.view.visible[1]].Account != "ab" {
		t.Fatal("failure ignored")
	}
	s.errorText = ""
	s.view.rebuild(&s, now, wall.Add(time.Minute))
	if r.Accounts[s.view.visible[1]].Account != "ab" {
		t.Fatal("clock ignored")
	}
	s.snapshot.report.Providers[0].Status = "unavailable"
	s.view.rebuild(&s, now, wall)
	if r.Accounts[s.view.visible[1]].Account != "ab" {
		t.Fatal("provider status ignored")
	}
}

func TestWatchViewResetSortAndEventBasedOrder(t *testing.T) {
	s, now, wall := viewState()
	r := s.snapshot.report
	a := r.Accounts[0]
	a.Account = "zz"
	a.Windows = append([]statusWindow(nil), a.Windows...)
	early := wall.Add(time.Minute)
	a.Windows[0].ResetsAt = &early
	b := r.Accounts[0]
	b.Account = "aa"
	r.Accounts = []statusAccount{b, a}
	s.finish(r, nil, now, wall)
	s.view.sortMode = 2
	s.view.rebuild(&s, now, wall)
	if s.view.visible[0] != 1 {
		t.Fatal("nearest reset not sorted")
	}
	watchFrame(&s, &watchViewport{}, 180, 40, now.Add(2*time.Minute), wall.Add(2*time.Minute), false)
	if s.view.visible[0] != 1 {
		t.Fatal("redraw reshuffled order")
	}
	s.view.rebuild(&s, now.Add(2*time.Minute), wall.Add(2*time.Minute))
	if s.view.visible[0] != 0 {
		t.Fatal("expired reset not last on next event")
	}
}

func TestWatchViewBoundedTextAndModeExit(t *testing.T) {
	s, now, wall := viewState()
	var k watchKeys
	var viewport watchViewport
	input := func(data []byte) bool {
		quit := false
		for _, event := range k.feed(data, now) {
			quit = s.view.handle(event, &s, &viewport, 180, 40, now, wall) || quit
		}
		return quit
	}
	input([]byte("/qrhl"))
	if s.manualNotice || s.view.draft != "qrhl" {
		t.Fatal("entry dispatched commands")
	}
	input([]byte{21})
	input([]byte("e\u0301"))
	input([]byte{127})
	if s.view.draft != "" {
		t.Fatal("grapheme Backspace split cluster")
	}
	input([]byte{0xe7})
	if len(k.pending) != 1 {
		t.Fatal("split rune discarded")
	}
	input([]byte{0x95, 0x8c})
	if s.view.draft != "界" {
		t.Fatal("split UTF-8", s.view.draft)
	}
	input([]byte(strings.Repeat("界", 100)))
	if len(s.view.draft) > 128 || !utf8.ValidString(s.view.draft) {
		t.Fatal("filter not bounded UTF-8")
	}
	input([]byte{7})
	if s.view.editing || s.view.name != "" {
		t.Fatal("cancel applied draft")
	}
	input([]byte("/WORK\r"))
	if s.view.editing || s.view.name != "WORK" || len(s.view.visible) != 1 {
		t.Fatal("apply failed")
	}
	input([]byte("/q"))
	if !input([]byte{3}) {
		t.Fatal("entry Ctrl-C didn't quit")
	}
	s.view.editing = false
	input([]byte("/"))
	input([]byte{27})
	now = now.Add(150 * time.Millisecond)
	if !input(nil) {
		t.Fatal("entry Escape didn't quit")
	}
	s.view.editing = false
	s.view.detail = true
	s.view.hasSelection = false
	input([]byte{13})
	if s.view.detail {
		t.Fatal("empty detail cannot return")
	}
}

func paintLines(lines []cell, color bool) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line.paint(color))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestWatchViewDetailFieldsAndSanitization(t *testing.T) {
	s, now, wall := viewState()
	s.view.selected = watchAccountID{"claude", "exhausted"}
	a := &s.snapshot.report.Accounts[4]
	expired := wall.Add(-time.Second)
	value := 23.456
	a.Email = "full@example.invalid"
	a.HeadroomPercent = &value
	a.CreditsBalance = "123"
	a.OnCredits = true
	a.ResetCredits = 4
	a.LoginExpiresAt = &expired
	a.AvailableAt = &expired
	a.Windows = []statusWindow{{Label: "dup", UsedPercent: &value, RemainingPercent: &value, ResetsAt: &expired}, {Label: "dup", Unavailable: "malformed\x1b[2J\u202e"}}
	got := paintLines(s.view.detailLines(&s, now, wall), false)
	for _, field := range []string{"claude/exhausted", "full@example.invalid", "specific denial", "23.456%", "123", "true", "Reset credits: 4", "Source generated (collection start)", "Received completion", "Observed at", "Next probe at", "Available at", "Login expires at", "login expired", "Window 2: dup", "Exhausted (recorded)", "malformed?[2J?", "passed; awaiting fresh data", "Known exhausted scope: Fable", "Denial observed at", "fixture usage unavailable", "all-model", "profile-lock contention"} {
		if !strings.Contains(got, field) {
			t.Fatal("missing full detail", field, got)
		}
	}
	if strings.ContainsAny(got, "\x1b\u202e") {
		t.Fatal("detail not sanitized")
	}
	s.view.selected = watchAccountID{"codex", "busy"}
	got = paintLines(s.view.detailLines(&s, now, wall), false)
	for _, field := range []string{"Headroom: unknown", "Observed at: unknown", "Login expires at: unknown", "no positive count reported", "Windows: none reported"} {
		if !strings.Contains(got, field) {
			t.Fatal("nil detail invented data", field)
		}
	}
}

var frameLinePattern = regexp.MustCompile(`\[[0-9]+;1H\[2K`)

func TestWatchViewCaptures(t *testing.T) {
	for _, variant := range []string{"wide-split", "narrow-detail", "filtered-sort", "empty", "unicode"} {
		for _, color := range []bool{false, true} {
			s, now, wall := viewState()
			width, height := 180, 40
			s.view.selected = watchAccountID{"claude", "personal"}
			s.view.detail = true
			s.view.detailFocus = true
			switch variant {
			case "narrow-detail":
				width, height = 40, 10
			case "filtered-sort":
				s.view.detail = false
				s.view.providerFilter = 1
				s.view.sortMode = 1
				s.view.stateFilter = 1
				s.view.name = "wo"
				s.view.rebuild(&s, now, wall)
			case "empty":
				s.view.detail = false
				s.view.name = "none"
				s.view.rebuild(&s, now, wall)
			case "unicode":
				s.snapshot.report.Accounts[3].Account = "界e\u0301"
				s.view.selected = watchID(s.snapshot.report.Accounts[3])
				s.view.rebuild(&s, now, wall)
				s.view.editing = true
				s.view.draft = "界e\u0301qr"
			}
			frame := watchFrame(&s, &watchViewport{}, width, height, now, wall, color)
			for _, line := range frameLinePattern.Split(frame, -1) {
				if uniseg.StringWidth(ansiPattern.ReplaceAllString(line, "")) > width-1 {
					t.Fatal("overflow", variant)
				}
			}
			if !color && ansiPattern.MatchString(frame) {
				t.Fatal("NO_COLOR")
			}
			if dir := os.Getenv("CODATOR_RICHER_S1_CAPTURE_DIR"); dir != "" {
				label := "plain"
				if color {
					label = "ansi"
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("richer-s1-%s-%s.txt", variant, label)), []byte(frame), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestWatchViewResponsiveFocusAndEntryTail(t *testing.T) {
	s, now, wall := viewState()
	v := watchViewport{}
	s.view.handle("enter", &s, &v, 180, 40, now, wall)
	s.view.handle("right", &s, &v, 180, 40, now, wall)
	if s.view.detailViewport.x != 1 || v.x != 0 {
		t.Fatal("detail focus scrolls table")
	}
	s.view.handle("tab", &s, &v, 180, 40, now, wall)
	s.view.handle("right", &s, &v, 180, 40, now, wall)
	if v.x != 1 {
		t.Fatal("Tab didn't focus table")
	}
	s.view.detailViewport.x = 0
	for _, size := range [][2]int{{180, 40}, {132, 18}, {131, 18}, {132, 17}, {40, 10}, {20, 8}, {10, 3}} {
		frame := watchFrame(&s, &v, size[0], size[1], now, wall, false)
		for _, line := range frameLinePattern.Split(frame, -1) {
			if uniseg.StringWidth(line) > size[0]-1 {
				t.Fatal("detail overflow", size)
			}
		}
		if size[0] < 20 {
			if !strings.Contains(frame, "Widen") {
				t.Fatal("tiny lost hint")
			}
			continue
		}
		if !strings.Contains(frame, "Account detail") {
			t.Fatal("detail unreachable", size)
		}
	}
	s.view.detail = false
	s.view.editing = true
	s.view.draft = strings.Repeat("x", 123) + "END"
	if frame := watchFrame(&s, &v, 40, 10, now, wall, false); !strings.Contains(frame, "END") || !strings.Contains(frame, "Enter/^G") {
		t.Fatal("entry tail/help hidden", frame)
	}
	// A stale row elsewhere cannot make a fresh Codex account stale.
	stale := wall.Add(-time.Hour)
	s.snapshot.report.Accounts[3].ObservedAt = &stale
	if !s.view.fresh(&s, s.snapshot.report.Accounts[0], now, wall) {
		t.Fatal("other provider's stale row leaked")
	}
	bad := math.Inf(1)
	s.snapshot.report.Accounts[0].Windows[0].RemainingPercent = &bad
	s.view.selected = watchID(s.snapshot.report.Accounts[0])
	if detail := paintLines(s.view.detailLines(&s, now, wall), false); !strings.Contains(detail, "Remaining: unknown") {
		t.Fatal("malformed percentage invented data")
	}
}

func TestWatchTimelineKindsOrderFiltersAndFreshness(t *testing.T) {
	s, now, wall := viewState()
	r := s.snapshot.report
	r.Accounts = append([]statusAccount(nil), r.Accounts...)
	future, past := wall.Add(time.Hour), wall.Add(-time.Minute)
	r.Accounts[0].AvailableAt = &future
	r.Accounts[0].KnownExhausted = []statusKnownExhausted{{Scope: "scope", Until: future, ObservedAt: wall.Add(-time.Minute)}}
	r.Accounts[4].AvailableAt = &past
	s.finish(r, nil, now, wall)
	before, _ := json.Marshal(s.snapshot.report)
	next := s.nextAccount("codex", now, wall)
	got := paintLines(s.view.timelineLines(&s, now, wall), false)
	for _, field := range []string{"Future recorded times", "window reset | Weekly limit #1", "window reset | Weekly limit #2", "denial-until | scope #1", "available-at | recorded estimate", "Passed — awaiting fresh data", "passed; awaiting fresh data", "Unknown timestamps", "Weekly (Fable) #3 | unknown", "in 1h 0m (fresh observation)"} {
		if !strings.Contains(got, field) {
			t.Fatal("missing timeline semantics", field, got)
		}
	}
	first := strings.Index(got, "codex/work | window reset | Weekly limit #1")
	second := strings.Index(got, "codex/work | window reset | Weekly limit #2")
	denial := strings.Index(got, "codex/work | denial-until | scope #1")
	available := strings.Index(got, "codex/work | available-at")
	if !(first < second && second < denial && denial < available) {
		t.Fatal("tie didn't preserve raw window/kind order")
	}
	for i, j := 0, len(s.view.visible)-1; i < j; i, j = i+1, j-1 {
		s.view.visible[i], s.view.visible[j] = s.view.visible[j], s.view.visible[i]
	}
	if got != paintLines(s.view.timelineLines(&s, now, wall), false) {
		t.Fatal("display sorting changed timeline raw tie order")
	}
	s.view.providerFilter = 1
	s.view.name = "WORK"
	s.view.rebuild(&s, now, wall)
	filtered := paintLines(s.view.timelineLines(&s, now, wall), false)
	if strings.Contains(filtered, "claude/personal") || !strings.Contains(filtered, "codex/work | window reset") {
		t.Fatal("timeline ignored filters")
	}
	after, _ := json.Marshal(s.snapshot.report)
	if string(before) != string(after) || next != s.nextAccount("codex", now, wall) {
		t.Fatal("timeline mutated snapshot/estimate")
	}
	for _, test := range []struct {
		name           string
		change         func(*watchState)
		elapsed, clock time.Duration
	}{
		{"aged", func(*watchState) {}, 300 * time.Second, 300 * time.Second},
		{"clock", func(*watchState) {}, 0, time.Minute},
		{"failure", func(s *watchState) { s.errorText = "failed" }, 0, 0},
		{"provider", func(s *watchState) { s.snapshot.report.Providers[0].Status = "unavailable" }, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := s
			copy.snapshot = &watchSnapshot{report: s.snapshot.report, received: now, wall: wall}
			copy.snapshot.report.Providers = append([]statusProvider(nil), s.snapshot.report.Providers...)
			test.change(&copy)
			text := paintLines(copy.view.timelineLines(&copy, now.Add(test.elapsed), wall.Add(test.clock)), false)
			if strings.Contains(text, "(fresh observation)") || !strings.Contains(text, "countdown unavailable") {
				t.Fatal("uncertain timeline counted down", text)
			}
		})
	}
	s.view.name = "no match"
	s.view.rebuild(&s, now, wall)
	if !strings.Contains(paintLines(s.view.timelineLines(&s, now, wall), false), "No accounts match filters") {
		t.Fatal("missing empty timeline")
	}
}

func TestWatchTimelineQualifiedPerAccountAndDenialObservation(t *testing.T) {
	s, now, wall := viewState()
	past := wall.Add(-time.Minute)
	s.snapshot.report.Accounts[4].AvailableAt = &past
	if !s.stale(now, wall) {
		t.Fatal("fixture must be globally stale")
	}
	got := paintLines(s.view.timelineLines(&s, now, wall), false)
	if !strings.Contains(got, "codex/work | window reset | Weekly limit #1 | 2026-10-14T13:00:00Z | in 1h 0m") {
		t.Fatal("other account stale suppressed qualified countdown", got)
	}
	a := &s.snapshot.report.Accounts[0]
	a.KnownExhausted = []statusKnownExhausted{{Scope: "old evidence", Until: wall.Add(time.Hour), ObservedAt: wall.Add(-time.Hour)}}
	got = paintLines(s.view.timelineLines(&s, now, wall), false)
	if !strings.Contains(got, "denial-until | old evidence #1 | 2026-10-14T13:00:00Z | stale/uncertain") {
		t.Fatal("cached denial became fresh", got)
	}
	s.snapshot.report.Accounts[3].ObservedAt = nil
	got = paintLines(s.view.timelineLines(&s, now, wall), false)
	if !strings.Contains(got, "claude/personal | window reset | Current session #1 | 2026-10-14T13:00:00Z | stale/uncertain") {
		t.Fatal("missing Claude observation fabricated freshness")
	}
}

func TestWatchOverviewRecordedCountsFreshAggregatesAndCredits(t *testing.T) {
	s, now, wall := viewState()
	r := s.snapshot.report
	zero := 0.0
	r.Accounts[0].HeadroomPercent = &zero
	r.Accounts[0].OnCredits = true
	r.Accounts[3].ObservedAt = nil
	r.Accounts[4].ObservedAt = nil
	s.finish(r, nil, now, wall)
	before, _ := json.Marshal(r)
	next := s.nextAccount("codex", now, wall)
	s.view.providerFilter = 2
	s.view.name = "no match"
	s.view.rebuild(&s, now, wall)
	lines, selected := s.view.overviewLines(&s, now, wall)
	got := paintLines(lines, false)
	if strings.Contains(got, "Qualified rows:") || strings.Contains(got, "rows excluded:") {
		t.Fatal("unrequested bookkeeping count remains", got)
	}
	for _, field := range []string{"codex | recorded status: ok", "> available: 1 (recorded)", "busy: 1 (recorded)", "unknown: 1 (recorded)", "Fresh launchable: 1 (qualified rows only)", "Lowest fresh headroom: 0% included quota", "Nearest fresh window reset: 2026-10-14T13:00:00Z | in 1h 0m", "On credits: 1 (recorded); headroom excludes credit balance", "Claude: all-model snapshot", "Fresh launchable: unknown (no qualified rows)", "Busy means profile-lock contention"} {
		if !strings.Contains(got, field) {
			t.Fatal("missing overview", field, got)
		}
	}
	if selected < 0 || !strings.Contains(lines[selected].paint(false), "> available") {
		t.Fatal("bucket location missing")
	}
	after, _ := json.Marshal(s.snapshot.report)
	if string(before) != string(after) || next != s.nextAccount("codex", now, wall) {
		t.Fatal("overview mutated source/estimate")
	}
	for _, clock := range []time.Duration{time.Minute, 300 * time.Second} {
		elapsed := time.Duration(0)
		if clock == 300*time.Second {
			elapsed = clock
		}
		lines, _ = s.view.overviewLines(&s, now.Add(elapsed), wall.Add(clock))
		text := paintLines(lines, false)
		if strings.Contains(text, "0% included quota") || strings.Contains(text, "| in 1h") || !strings.Contains(text, "available: 1 (recorded)") {
			t.Fatal("uncertain overview fabricated fresh metrics", text)
		}
	}
	s.errorText = "retained failure"
	lines, _ = s.view.overviewLines(&s, now, wall)
	if strings.Contains(paintLines(lines, false), "Fresh launchable: 1") {
		t.Fatal("failed report has fresh aggregate")
	}
	s.errorText = ""
	s.snapshot.report.Providers[0].Status = "unavailable"
	s.snapshot.report.Providers[0].Error = "failure\x1b[2J\u202e"
	lines, _ = s.view.overviewLines(&s, now, wall)
	text := paintLines(lines, false)
	if strings.ContainsAny(text, "\x1b\u202e") || !strings.Contains(text, "Provider error: failure?[2J?") || strings.Contains(text, "0% included quota") {
		t.Fatal("provider failure/sanitization", text)
	}
}

func TestWatchOverviewUnknownPercentNotZero(t *testing.T) {
	s, now, wall := viewState()
	r := s.snapshot.report
	r.Accounts = r.Accounts[1:3]
	s.finish(r, nil, now, wall)
	lines, _ := s.view.overviewLines(&s, now, wall)
	text := paintLines(lines, false)
	if !strings.Contains(text, "Lowest fresh headroom: unknown") || !strings.Contains(text, "Nearest fresh window reset: unknown") || strings.Contains(text, "0%") {
		t.Fatal("missing percentage became zero", text)
	}
	bad := math.NaN()
	s.snapshot.report.Accounts[0].HeadroomPercent = &bad
	lines, _ = s.view.overviewLines(&s, now, wall)
	if strings.Contains(paintLines(lines, false), "NaN") {
		t.Fatal("nonfinite percentage displayed")
	}
}

func TestWatchPairTwoKeysLinkAndViewportPreservation(t *testing.T) {
	s, now, wall := viewState()
	var table watchViewport
	watchFrame(&s, &table, 40, 10, now, wall, false)
	s.view.handle("end", &s, &table, 40, 10, now, wall)
	table.x = 4
	watchFrame(&s, &table, 40, 10, now, wall, false)
	before := table
	s.view.handle("text:t", &s, &table, 40, 10, now, wall)
	s.view.handle("down", &s, &table, 40, 10, now, wall)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table != before || s.view.detailViewport.y != 1 {
		t.Fatal("auxiliary body moved table", table, before)
	}
	s.finish(s.snapshot.report, nil, now, wall)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if table != before {
		t.Fatal("auxiliary refresh changed table")
	}
	s.view.handle("text:t", &s, &table, 40, 10, now, wall)
	watchFrame(&s, &table, 40, 10, now, wall, false)
	if s.view.body != "" || table != before {
		t.Fatal("active view didn't return/preserve table")
	}
	s.view.name = "work"
	s.view.providerFilter = 1
	s.view.rebuild(&s, now, wall)
	s.view.handle("text:a", &s, &table, 40, 10, now, wall)
	for i := 0; i < 6; i++ {
		s.view.handle("text:]", &s, &table, 40, 10, now, wall)
	}
	frame := watchFrame(&s, &table, 40, 10, now, wall, false)
	if !strings.Contains(frame, "> busy: 0 (recorded)") {
		t.Fatal("bucket navigation not revealed", frame)
	}
	s.view.handle("enter", &s, &table, 40, 10, now, wall)
	if s.view.body != "" || s.view.providerFilter != 2 || s.view.stateFilter != 2 || s.view.name != "" || s.view.hasSelection {
		t.Fatal("bucket link didn't apply empty intersection/clear name")
	}
	frame = watchFrame(&s, &table, 180, 40, now, wall, false)
	if !strings.Contains(frame, "No accounts match filters") || strings.Contains(frame, "╭─ codex") {
		t.Fatal("empty bucket didn't reach normal table", frame)
	}
	s.view.handle("text:/", &s, &table, 180, 40, now, wall)
	s.view.handle("text:t", &s, &table, 180, 40, now, wall)
	s.view.handle("text:a", &s, &table, 180, 40, now, wall)
	if s.view.body != "" || s.view.draft != "ta" {
		t.Fatal("view commands escaped name mode")
	}
}

func TestWatchPairTwoCapturesAndBounds(t *testing.T) {
	for _, variant := range []string{"timeline", "timeline-passed-stale", "overview", "overview-unknown", "linked-filter"} {
		for _, size := range [][2]int{{180, 40}, {40, 10}, {20, 8}, {10, 3}} {
			for _, color := range []bool{false, true} {
				s, now, wall := viewState()
				s.view.body = "timeline"
				switch variant {
				case "timeline-passed-stale":
					past := wall.Add(-time.Minute)
					s.snapshot.report.Accounts[0].Windows = append([]statusWindow(nil), s.snapshot.report.Accounts[0].Windows...)
					s.snapshot.report.Accounts[0].Windows[0].ResetsAt = &past
					s.snapshot.report.Accounts[4].AvailableAt = &past
				case "overview":
					s.view.body = "overview"
				case "overview-unknown":
					s.view.body = "overview"
					s.errorText = "retained failure"
					s.snapshot.report.Providers[1].Status = "unavailable"
					s.snapshot.report.Providers[1].Error = "fixture provider unavailable"
				case "linked-filter":
					s.view.body = "overview"
					s.view.name = "no match"
					s.view.bucketProvider = "claude"
					s.view.bucketState = 1
					s.view.handle("enter", &s, &watchViewport{}, size[0], size[1], now, wall)
				}
				frame := watchFrame(&s, &watchViewport{}, size[0], size[1], now, wall, color)
				for _, line := range frameLinePattern.Split(frame, -1) {
					if uniseg.StringWidth(ansiPattern.ReplaceAllString(line, "")) > size[0]-1 {
						t.Fatal("pair2 overflow", variant, size)
					}
				}
				if !color && ansiPattern.MatchString(frame) {
					t.Fatal("pair2 NO_COLOR")
				}
				if size[0] >= 40 {
					if dir := os.Getenv("CODATOR_RICHER_S2_CAPTURE_DIR"); dir != "" {
						label := "plain"
						if color {
							label = "ansi"
						}
						p := filepath.Join(dir, fmt.Sprintf("richer-s2-%s-%dx%d-%s.txt", variant, size[0], size[1], label))
						if err := os.WriteFile(p, []byte(frame), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
}

func TestWatchTimestampCollapseKeepsEventMeaning(t *testing.T) {
	s, now, wall := viewState()
	s.lastAttempt = wall.Add(10 * time.Millisecond)
	s.lastSuccess = wall.Add(20 * time.Millisecond)
	text := paintLines(s.timeLines(), false)
	if strings.Count(text, watchStamp(wall)) != 1 || !strings.Contains(text, "same UTC second") {
		t.Fatal("identical displayed timestamps not collapsed", text)
	}
	for _, label := range []string{"Attempt", "completion (OK)", "source (start)"} {
		if !strings.Contains(text, label) {
			t.Fatal("collapsed timestamp lost event meaning", text)
		}
	}
	s.lastAttempt = wall.Add(time.Second)
	s.lastSuccess = wall.Add(2 * time.Second)
	text = paintLines(s.timeLines(), false)
	if len(s.timeLines()) != 3 || !strings.Contains(text, "Last attempt: "+watchStamp(s.lastAttempt)) || !strings.Contains(text, "Last successful completion: "+watchStamp(s.lastSuccess)) || !strings.Contains(text, "Source generated (collection start): "+watchStamp(wall)) {
		t.Fatal("different event times collapsed", text)
	}
	detail := paintLines(s.view.detailLines(&s, now, wall), false)
	if strings.Count(detail, watchStamp(wall)) != 1 || !strings.Contains(detail, "Source generated (collection start) / Received completion") {
		t.Fatal("duplicate detail timestamp retained", detail)
	}
	s.snapshot.wall = wall.Add(time.Second)
	detail = paintLines(s.view.detailLines(&s, now, wall), false)
	if !strings.Contains(detail, "Source generated (collection start): "+watchStamp(wall)) || !strings.Contains(detail, "Received completion: "+watchStamp(s.snapshot.wall)) {
		t.Fatal("different detail times collapsed", detail)
	}
}

func TestWatchPairTwoLayoutCaptures(t *testing.T) {
	for _, variant := range []string{"split-132x18", "table-40x18", "timestamps-equal", "timestamps-different"} {
		for _, color := range []bool{false, true} {
			s, now, wall := viewState()
			width, height := 180, 40
			r := s.snapshot.report
			r.Accounts = append([]statusAccount(nil), r.Accounts...)
			w := r.Accounts[0].Windows[0]
			w.Label = "5-hour limit"
			r.Accounts[0].Windows = append(append([]statusWindow(nil), r.Accounts[0].Windows...), w)
			r.Accounts[0].Email = "long-fixture-address@example.invalid"
			s.finish(r, nil, now, wall)
			s.lastAttempt = wall
			s.view.selected = watchID(r.Accounts[0])
			s.view.detail = true
			s.view.detailFocus = true
			switch variant {
			case "split-132x18":
				width, height = 132, 18
			case "table-40x18":
				width, height = 40, 18
				s.view.detail = false
			case "timestamps-different":
				s.lastAttempt = wall.Add(-2 * time.Second)
				s.snapshot.report.GeneratedAt = wall.Add(-time.Second)
			}
			frame := watchFrame(&s, &watchViewport{}, width, height, now, wall, color)
			for _, line := range frameLinePattern.Split(frame, -1) {
				if uniseg.StringWidth(ansiPattern.ReplaceAllString(line, "")) > width-1 {
					t.Fatal("layout capture overflow", variant)
				}
			}
			if variant == "split-132x18" {
				if !strings.Contains(frame, "…") {
					t.Fatal("split detail isn't marked")
				}
				for _, line := range frameLinePattern.Split(frame, -1) {
					text := ansiPattern.ReplaceAllString(line, "")
					left := watchClip(cell{{text, ""}}, 0, 87).paint(false)
					if strings.HasPrefix(left, "╭") && !strings.HasSuffix(left, "╮") || strings.HasPrefix(left, "╰") && !strings.HasSuffix(left, "╯") {
						t.Fatal("split corner cut", left)
					}
				}
			}
			if dir := os.Getenv("CODATOR_RICHER_S2_CAPTURE_DIR"); dir != "" {
				label := "plain"
				if color {
					label = "ansi"
				}
				p := filepath.Join(dir, "richer-s2-layout-"+variant+"-"+label+".txt")
				if err := os.WriteFile(p, []byte(frame), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
