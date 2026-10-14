package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rivo/uniseg"
)

type watchAccountID struct{ provider, account string }

func watchID(a statusAccount) watchAccountID { return watchAccountID{a.Provider, a.Account} }

type watchView struct {
	visible                               []int
	selected                              watchAccountID
	hasSelection                          bool
	sortMode, providerFilter, stateFilter int
	name, draft                           string
	editing, detail, detailFocus, reveal  bool
	revealAccount                         bool
	detailViewport                        watchViewport
	body, bucketProvider                  string
	bucketState                           int
	bucketReveal                          bool
}

var watchSortNames = []string{"label ↑", "headroom ↓", "reset ↑"}
var watchStateNames = []string{"all", "available", "busy", "exhausted", "unknown", "ineligible"}

func (v *watchView) fresh(s *watchState, a statusAccount, now, wall time.Time) bool {
	if s.failed(now, wall) || !watchFresh(s.snapshot.report.GeneratedAt, wall) || watchAccountStale(a, s.snapshot.report, wall) {
		return false
	}
	for _, p := range s.snapshot.report.Providers {
		if p.Provider == a.Provider {
			return p.Status == "ok"
		}
	}
	return false
}

func (v *watchView) metric(s *watchState, a statusAccount, now, wall time.Time) (float64, bool) {
	if !v.fresh(s, a, now, wall) {
		return 0, false
	}
	if v.sortMode == 1 {
		if watchPercent(a.HeadroomPercent) {
			return -*a.HeadroomPercent, true
		}
		return 0, false
	}
	var reset time.Time
	for _, w := range a.Windows {
		if w.ResetsAt != nil && w.ResetsAt.After(wall) && (reset.IsZero() || w.ResetsAt.Before(reset)) {
			reset = *w.ResetsAt
		}
	}
	if reset.IsZero() {
		return 0, false
	}
	return float64(reset.Unix()), true
}

func (v *watchView) rebuild(s *watchState, now, wall time.Time) {
	v.visible = v.visible[:0]
	if s.snapshot == nil {
		v.hasSelection = false
		return
	}
	r := s.snapshot.report
	if v.providerFilter > len(r.Providers) {
		v.providerFilter = 0
	}
	for pi, p := range r.Providers {
		if v.providerFilter != 0 && v.providerFilter != pi+1 {
			continue
		}
		start := len(v.visible)
		for i, a := range r.Accounts {
			if a.Provider == p.Provider && (v.stateFilter == 0 || a.State == watchStateNames[v.stateFilter]) && strings.Contains(strings.ToLower(a.Account), strings.ToLower(v.name)) {
				v.visible = append(v.visible, i)
			}
		}
		sort.SliceStable(v.visible[start:], func(i, j int) bool {
			a, b := r.Accounts[v.visible[start+i]], r.Accounts[v.visible[start+j]]
			if v.sortMode != 0 {
				x, xok := v.metric(s, a, now, wall)
				y, yok := v.metric(s, b, now, wall)
				if xok != yok {
					return xok
				}
				if xok && x != y {
					return x < y
				}
			}
			return a.Account < b.Account
		})
	}
	found := false
	for _, i := range v.visible {
		if v.hasSelection && watchID(r.Accounts[i]) == v.selected {
			found = true
			break
		}
	}
	if !found {
		v.reveal = true
		v.revealAccount = true
		v.hasSelection = len(v.visible) > 0
		if v.hasSelection {
			v.selected = watchID(r.Accounts[v.visible[0]])
		}
		v.detailViewport = watchViewport{}
	}
}

func (v *watchView) account(s *watchState) (statusAccount, bool) {
	if !v.hasSelection || s.snapshot == nil {
		return statusAccount{}, false
	}
	for _, i := range v.visible {
		a := s.snapshot.report.Accounts[i]
		if watchID(a) == v.selected {
			return a, true
		}
	}
	return statusAccount{}, false
}

func (v *watchView) selectNext(s *watchState, delta int) {
	if !v.hasSelection {
		return
	}
	for at, i := range v.visible {
		if watchID(s.snapshot.report.Accounts[i]) == v.selected {
			next := max(0, min(len(v.visible)-1, at+delta))
			if next != at {
				v.selected = watchID(s.snapshot.report.Accounts[v.visible[next]])
				v.detailViewport = watchViewport{}
			}
			v.reveal = true
			v.revealAccount = true
			return
		}
	}
}

func watchSplit(width, height int) bool { return width >= 132 && height >= 18 }

func (v *watchView) handle(key string, s *watchState, table *watchViewport, width, height int, now, wall time.Time) bool {
	if key == "quit" {
		return true
	}
	if v.editing {
		switch key {
		case "enter":
			v.name, v.editing = v.draft, false
			v.rebuild(s, now, wall)
			v.reveal = true
		case "cancel":
			v.editing = false
		case "clear":
			v.draft = ""
		case "backspace":
			g := uniseg.NewGraphemes(v.draft)
			last := 0
			for g.Next() {
				last, _ = g.Positions()
			}
			v.draft = v.draft[:last]
		default:
			if strings.HasPrefix(key, "text:") && len(v.draft)+len(key[5:]) <= 128 {
				v.draft += key[5:]
			}
		}
		return false
	}
	if strings.HasPrefix(key, "text:") {
		if key == "text:t" || key == "text:a" || key == "text:g" || key == "text:c" {
			body := "timeline"
			if key == "text:a" {
				body = "overview"
			}
			if key == "text:g" {
				body = "sparklines"
			}
			if key == "text:c" {
				body = "feed"
			}
			if v.body == body {
				body = ""
			}
			v.body, v.detail, v.detailFocus = body, false, false
			v.bucketReveal = body == "overview"
			v.detailViewport = watchViewport{}
			return false
		}
		if v.body != "" && (key == "text:[" || key == "text:]") {
			if v.body == "overview" {
				delta := 1
				if key == "text:[" {
					delta = -1
				}
				v.moveBucket(s, delta)
			}
			return false
		}
		switch key[5:] {
		case "q":
			return true
		case "r":
			s.manualNotice = true
		case "h":
			key = "left"
		case "l":
			key = "right"
		case "[":
			v.selectNext(s, -1)
		case "]":
			v.selectNext(s, 1)
		case "s":
			v.sortMode = (v.sortMode + 1) % len(watchSortNames)
			v.rebuild(s, now, wall)
			v.reveal = true
		case "p":
			if s.snapshot != nil {
				v.providerFilter = (v.providerFilter + 1) % (len(s.snapshot.report.Providers) + 1)
				v.rebuild(s, now, wall)
				v.reveal = true
			}
		case "f":
			v.stateFilter = (v.stateFilter + 1) % len(watchStateNames)
			v.rebuild(s, now, wall)
			v.reveal = true
		case "/":
			if v.body == "" && (!v.detail || watchSplit(width, height) && !v.detailFocus) {
				v.draft, v.editing = v.name, true
			}
		}
	}
	switch key {
	case "enter":
		if v.body == "overview" {
			pi, state := v.bucket(s)
			if pi >= 0 {
				v.providerFilter, v.stateFilter, v.name = pi+1, state, ""
				v.body = ""
				v.rebuild(s, now, wall)
				v.reveal = true
			}
			return false
		}
		if v.body != "" {
			return false
		}
		if v.detail || v.hasSelection {
			v.detail = !v.detail
			v.detailFocus = v.detail
			v.detailViewport = watchViewport{}
			v.reveal = true
		}
	case "tab":
		if v.detail && watchSplit(width, height) {
			v.detailFocus = !v.detailFocus
		}
	}
	viewport := table
	if v.body != "" || v.detail && (!watchSplit(width, height) || v.detailFocus) {
		viewport = &v.detailViewport
	}
	switch key {
	case "up":
		viewport.y--
	case "down":
		viewport.y++
	case "left":
		viewport.x--
	case "right":
		viewport.x++
	case "pageup":
		viewport.y -= max(1, height-5)
	case "pagedown":
		viewport.y += max(1, height-5)
	case "home":
		viewport.x, viewport.y = 0, 0
	case "end":
		viewport.y = math.MaxInt
	}
	return false
}

func (v *watchView) summary(s *watchState) string {
	provider := "all"
	if s.snapshot != nil && v.providerFilter > 0 && v.providerFilter <= len(s.snapshot.report.Providers) {
		provider = s.snapshot.report.Providers[v.providerFilter-1].Provider
	}
	selected := "none"
	if v.hasSelection {
		selected = v.selected.provider + "/" + v.selected.account
	}
	return watchClean(fmt.Sprintf("Selected: %s | sort %s | provider %s | state %s | name %q", selected, watchSortNames[v.sortMode], provider, watchStateNames[v.stateFilter], v.name))
}

func (v *watchView) help(s *watchState) []cell {
	return []cell{{{v.summary(s), ansiDim}}, {{"[ ] select | Enter detail | Tab focus | s sort | p provider | f state | / name | t timeline | a overview | g sparklines | c feed", ansiDim}}, {{"Name: Enter apply, Ctrl-G cancel, Ctrl-U clear, Backspace; Esc/Ctrl-C quit.", ansiDim}}, {{"Busy means profile-lock contention, not an active session.", ansiDim}}}
}

func (v *watchView) bucket(s *watchState) (int, int) {
	if s.snapshot == nil || len(s.snapshot.report.Providers) == 0 {
		return -1, 1
	}
	pi, state := 0, v.bucketState
	if state < 1 || state >= len(watchStateNames) {
		state = 1
	}
	for i, p := range s.snapshot.report.Providers {
		if p.Provider == v.bucketProvider {
			pi = i
			break
		}
	}
	return pi, state
}

func (v *watchView) moveBucket(s *watchState, delta int) {
	pi, state := v.bucket(s)
	if pi < 0 {
		return
	}
	count := len(watchStateNames) - 1
	at := max(0, min(len(s.snapshot.report.Providers)*count-1, pi*count+state-1+delta))
	v.bucketProvider = s.snapshot.report.Providers[at/count].Provider
	v.bucketState = at%count + 1
	v.bucketReveal = true
}

type watchResetEntry struct {
	at             time.Time
	account, order int
	kind, label    string
	fresh          bool
}

func (v *watchView) timelineLines(s *watchState, now, wall time.Time) []cell {
	lines := []cell{{{"Reset timeline | t tables | a overview", ansiBold}}, {{v.summary(s), ansiDim}}}
	if s.snapshot == nil {
		return append(lines, cell{{"No successful snapshot; reset times unknown.", ansiDim}})
	}
	r := s.snapshot.report
	lines = append(lines, cell{{"Recorded source: " + watchStamp(r.GeneratedAt) + " (collection start)", ansiDim}}, cell{{"Reset / denial-until / available-at are distinct recorded kinds; no quota inferred.", ansiDim}})
	visible := make([]bool, len(r.Accounts))
	for _, i := range v.visible {
		visible[i] = true
	}
	var entries []watchResetEntry
	for i, a := range r.Accounts {
		if !visible[i] {
			continue
		}
		fresh := v.fresh(s, a, now, wall)
		add := func(at time.Time, kind, label string, qualified bool) {
			entries = append(entries, watchResetEntry{at, i, len(entries), kind, label, qualified})
		}
		for j, w := range a.Windows {
			var at time.Time
			if w.ResetsAt != nil {
				at = *w.ResetsAt
			}
			add(at, "window reset", fmt.Sprintf("%s #%d", w.Label, j+1), fresh && w.Unavailable == "" && watchPercent(w.UsedPercent) && watchPercent(w.RemainingPercent))
		}
		for j, d := range a.KnownExhausted {
			add(d.Until, "denial-until", fmt.Sprintf("%s #%d", d.Scope, j+1), fresh && watchFresh(d.ObservedAt, wall) && d.Until.After(d.ObservedAt))
		}
		var at time.Time
		if a.AvailableAt != nil {
			at = *a.AvailableAt
		}
		add(at, "available-at", "recorded estimate", fresh)
	}
	group := func(at time.Time) int {
		if at.IsZero() {
			return 2
		}
		if !at.After(wall) {
			return 1
		}
		return 0
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		x, y := group(a.at), group(b.at)
		if x != y {
			return x < y
		}
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		return a.order < b.order
	})
	headings := []string{"Future recorded times", "Passed — awaiting fresh data", "Unknown timestamps"}
	last := -1
	for _, e := range entries {
		g := group(e.at)
		if g != last {
			lines = append(lines, nil, cell{{headings[g], ansiBold}})
			last = g
		}
		a := r.Accounts[e.account]
		stamp := "unknown"
		qualifier := ""
		if !e.at.IsZero() {
			stamp = watchStamp(e.at)
			if g == 1 {
				qualifier = " | passed; awaiting fresh data"
			} else if e.fresh {
				qualifier = " | in " + durationText(e.at.Sub(wall)) + " (fresh observation)"
			} else {
				qualifier = " | stale/uncertain; countdown unavailable"
			}
		}
		lines = append(lines, cell{{watchClean(a.Provider + "/" + a.Account + " | " + e.kind + " | " + e.label + " | " + stamp + qualifier), ""}})
	}
	if len(entries) == 0 {
		lines = append(lines, cell{{"No accounts match filters; change p/f filters or return to tables for / name.", ansiDim}})
	}
	return append(lines, nil, cell{{"Arrows/h/l/page/home/end scroll; r gated; q/Esc/Ctrl-C quit.", ansiDim}})
}

func (v *watchView) overviewLines(s *watchState, now, wall time.Time) ([]cell, int) {
	lines := []cell{{{"Availability overview | a tables | t timeline", ansiBold}}, {{"Counts are recorded rows; fresh values exclude stale/uncertain observations per account.", ansiDim}}}
	if s.snapshot == nil {
		return append(lines, cell{{"No successful snapshot; availability unknown.", ansiDim}}), -1
	}
	r := s.snapshot.report
	pi, state := v.bucket(s)
	selectedLine := -1
	lines = append(lines, cell{{"Recorded source: " + watchStamp(r.GeneratedAt) + " (collection start)", ansiDim}})
	for i, p := range r.Providers {
		lines = append(lines, nil, cell{{watchClean(p.Provider + " | recorded status: " + p.Status), ansiBold}})
		if p.Error != "" {
			lines = append(lines, cell{{watchClean("Provider error: " + p.Error), ansiDim}})
		}
		var counts [6]int
		qualified, launchable, credits := 0, 0, 0
		var lowest *float64
		var reset time.Time
		for _, a := range r.Accounts {
			if a.Provider != p.Provider {
				continue
			}
			for n := 1; n < len(watchStateNames); n++ {
				if a.State == watchStateNames[n] {
					counts[n]++
				}
			}
			if a.OnCredits {
				credits++
			}
			if !v.fresh(s, a, now, wall) {
				continue
			}
			qualified++
			if a.Launchable && a.State == "available" {
				launchable++
			}
			if watchPercent(a.HeadroomPercent) && (lowest == nil || *a.HeadroomPercent < *lowest) {
				lowest = a.HeadroomPercent
			}
			for _, w := range a.Windows {
				if w.Unavailable == "" && watchPercent(w.UsedPercent) && watchPercent(w.RemainingPercent) && w.ResetsAt != nil && w.ResetsAt.After(wall) && (reset.IsZero() || w.ResetsAt.Before(reset)) {
					reset = *w.ResetsAt
				}
			}
		}
		for n := 1; n < len(watchStateNames); n++ {
			marker, style := "  ", ""
			if pi == i && state == n {
				marker, style = "> ", ansiBold
				selectedLine = len(lines)
			}
			lines = append(lines, cell{{fmt.Sprintf("%s%s: %d (recorded)", marker, watchStateNames[n], counts[n]), style}})
		}
		freshCount := "unknown (no qualified rows)"
		if qualified > 0 {
			freshCount = fmt.Sprintf("%d (qualified rows only)", launchable)
		}
		quota := "unknown"
		if lowest != nil {
			quota = fmt.Sprintf("%g%% included quota", *lowest)
		}
		nearest := "unknown"
		if !reset.IsZero() {
			nearest = watchStamp(reset) + " | in " + durationText(reset.Sub(wall))
		}
		lines = append(lines, cell{{"Fresh launchable: " + freshCount, ""}}, cell{{"Lowest fresh headroom: " + quota, ""}}, cell{{"Nearest fresh window reset: " + nearest, ""}}, cell{{fmt.Sprintf("On credits: %d (recorded); headroom excludes credit balance", credits), ansiDim}})
		if p.Provider == "claude" {
			lines = append(lines, cell{{"Claude: all-model snapshot; launch model may differ.", ansiDim}})
		}
	}
	return append(lines, nil, cell{{"[ ] bucket | Enter applies provider/state, clears name, returns to tables.", ansiDim}}, cell{{"Busy means profile-lock contention, not an active session; counts are not live activity.", ansiDim}}), selectedLine
}

func (v *watchView) detailLines(s *watchState, now, wall time.Time) []cell {
	a, ok := v.account(s)
	if !ok {
		return []cell{{{"No selected account; change filters and return to tables with Enter.", ansiDim}}}
	}
	var lines []cell
	add := func(label, value string) { lines = append(lines, cell{{watchClean(label + ": " + value), ""}}) }
	text := func(value string) string {
		if value == "" {
			return "unknown"
		}
		return value
	}
	stamp := func(value *time.Time) string {
		if value == nil {
			return "unknown"
		}
		return watchStamp(*value)
	}
	percent := func(value *float64) string {
		if !watchPercent(value) {
			return "unknown"
		}
		return fmt.Sprintf("%g%%", *value)
	}
	lines = append(lines, cell{{"Account detail | Enter tables | Tab focus", ansiBold}})
	add("Account", a.Provider+"/"+a.Account)
	add("Email", text(a.Email))
	freshness := "recorded / stale or uncertain"
	if v.fresh(s, a, now, wall) {
		freshness = "fresh snapshot; not live activity"
	}
	add("Data", freshness)
	add("State", text(a.State))
	add("Launchable (recorded)", fmt.Sprint(a.Launchable))
	add("Reason", text(a.Reason))
	add("Headroom", percent(a.HeadroomPercent))
	add("Credits balance", text(a.CreditsBalance))
	add("On credits (recorded)", fmt.Sprint(a.OnCredits))
	resetCredits := "no positive count reported"
	if a.ResetCredits > 0 {
		resetCredits = fmt.Sprint(a.ResetCredits)
	}
	add("Reset credits", resetCredits)
	if !s.snapshot.report.GeneratedAt.IsZero() && !s.snapshot.wall.IsZero() && watchStamp(s.snapshot.report.GeneratedAt) == watchStamp(s.snapshot.wall) {
		add("Source generated (collection start) / Received completion", watchStamp(s.snapshot.wall)+" (same UTC second)")
	} else {
		add("Source generated (collection start)", watchStamp(s.snapshot.report.GeneratedAt))
		add("Received completion", watchStamp(s.snapshot.wall))
	}
	add("Observed at", stamp(a.ObservedAt))
	add("Next probe at", stamp(a.NextProbeAt))
	add("Available at", stamp(a.AvailableAt))
	add("Login expires at", stamp(a.LoginExpiresAt))
	if a.LoginExpiresAt != nil {
		value, _ := loginExpiryText(*a.LoginExpiresAt, wall)
		add("Login", value)
	}
	if a.Provider == "claude" {
		add("Scope", "all-model snapshot; launch model may differ")
	}
	add("Busy", "profile-lock contention, not an active session")
	for i, w := range a.Windows {
		lines = append(lines, nil)
		add(fmt.Sprintf("Window %d", i+1), text(w.Label))
		// The incumbent bar remains supplementary to full recorded fields.
		w.Label, w.Unavailable = watchClean(w.Label), watchClean(w.Unavailable)
		if !watchPercent(w.RemainingPercent) {
			w.RemainingPercent = nil
		}
		lines = append(lines, statusWindowCell(w, wall))
		add("Used", percent(w.UsedPercent))
		add("Remaining", percent(w.RemainingPercent))
		add("Exhausted (recorded)", fmt.Sprint(w.Exhausted))
		add("Resets at", stamp(w.ResetsAt))
		add("Unavailable", text(w.Unavailable))
		if w.ResetsAt != nil && !w.ResetsAt.After(wall) {
			add("Reset", "passed; awaiting fresh data")
		}
	}
	if len(a.Windows) == 0 {
		add("Windows", "none reported")
	}
	for _, d := range a.KnownExhausted {
		lines = append(lines, nil)
		add("Known exhausted scope", text(d.Scope))
		add("Until", watchStamp(d.Until))
		add("Denial observed at", watchStamp(d.ObservedAt))
	}
	if a.claudeUnavailable != "" {
		add("Usage unavailable", a.claudeUnavailable)
	}
	for _, note := range statusNotes(a, wall) {
		add("Note", note)
	}
	return lines
}
