package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	watchInterval       = 300 * time.Second
	watchBackoff        = 900 * time.Second
	watchDeadline       = 120 * time.Second
	watchCleanupGrace   = 5 * time.Second
	watchCleanupWarning = "codator: probe cleanup uncertain; further refreshes disabled"
)

type watchSnapshot struct {
	report         statusReport
	received, wall time.Time
}

type watchState struct {
	view                             watchView
	history                          watchHistory
	snapshot                         *watchSnapshot
	lastAttempt, lastSuccess, next   time.Time
	cleanupUntil                     time.Time
	inflight, disabled, manualNotice bool
	errorText                        string
}

func (s *watchState) begin(now, wall time.Time) bool {
	if s.disabled || s.inflight || now.Before(s.next) {
		return false
	}
	s.inflight, s.manualNotice = true, false
	s.lastAttempt = wall
	return true
}

func (s *watchState) finish(report statusReport, err error, now, wall time.Time) {
	s.inflight = false
	s.cleanupUntil = time.Time{}
	if s.disabled {
		return
	}
	if err != nil {
		s.history.failure(now, wall)
		s.errorText = "status collection failed; retained data"
		if errors.Is(err, context.DeadlineExceeded) {
			s.errorText = "collection timed out; retained data"
		}
		s.next = now.Add(watchBackoff)
		return
	}
	s.snapshot = &watchSnapshot{report: report, received: now, wall: wall}
	s.lastSuccess, s.next, s.errorText = wall, now.Add(watchInterval), ""
	s.history.observe(s, now, wall)
	s.view.rebuild(s, now, wall)
}

func (s *watchState) checkCleanup(now time.Time, ctxErr error) bool {
	if !s.inflight || ctxErr == nil || s.disabled {
		return false
	}
	if s.cleanupUntil.IsZero() {
		s.history.failure(now, now.UTC())
		s.cleanupUntil = now.Add(watchCleanupGrace)
		s.errorText = "collection timed out; cleaning up"
		return true
	}
	if !now.Before(s.cleanupUntil) {
		s.disabled = true
		return true
	}
	return false
}

func watchFresh(observed, now time.Time) bool {
	return !observed.IsZero() && !observed.After(now) && now.Sub(observed) < watchInterval
}

func watchHasQuota(a statusAccount) bool {
	return a.HeadroomPercent != nil || len(a.Windows) > 0 || len(a.KnownExhausted) > 0
}

func watchAccountStale(a statusAccount, report statusReport, wall time.Time) bool {
	observed := report.GeneratedAt
	if a.Provider == "claude" {
		if a.ObservedAt == nil {
			return true
		}
		observed = *a.ObservedAt
	}
	if !watchFresh(observed, wall) {
		return true
	}
	for _, w := range a.Windows {
		if w.ResetsAt != nil && (!w.ResetsAt.After(wall) || !w.ResetsAt.After(observed)) {
			return true
		}
	}
	for _, d := range a.KnownExhausted {
		if !d.Until.After(wall) || d.ObservedAt.After(wall) {
			return true
		}
	}
	return a.AvailableAt != nil && !a.AvailableAt.After(wall)
}

func (s *watchState) failed(now, wall time.Time) bool {
	if s.disabled || s.errorText != "" || s.snapshot == nil {
		return true
	}
	expected := s.snapshot.wall.Add(now.Sub(s.snapshot.received))
	drift := wall.Sub(expected)
	return drift > 5*time.Second || drift < -5*time.Second
}

func (s *watchState) stale(now, wall time.Time) bool {
	if s.failed(now, wall) || !watchFresh(s.snapshot.report.GeneratedAt, wall) {
		return true
	}
	for _, a := range s.snapshot.report.Accounts {
		if watchHasQuota(a) && watchAccountStale(a, s.snapshot.report, wall) {
			return true
		}
	}
	return false
}

func watchPercent(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= 100
}

func (s *watchState) nextAccount(provider string, now, wall time.Time) string {
	if s.failed(now, wall) || !watchFresh(s.snapshot.report.GeneratedAt, wall) {
		return "unknown / stale"
	}
	r := s.snapshot.report
	ok := false
	for _, p := range r.Providers {
		ok = ok || p.Provider == provider && p.Status == "ok"
	}
	if !ok {
		return "unknown"
	}
	for _, a := range r.Accounts {
		if a.Provider == provider && watchHasQuota(a) && watchAccountStale(a, r, wall) {
			return "unknown / stale"
		}
	}
	var best *statusAccount
	for i := range r.Accounts {
		a := &r.Accounts[i]
		if a.Provider != provider || a.State != "available" || !a.Launchable || watchAccountStale(*a, r, wall) || !watchPercent(a.HeadroomPercent) || len(a.KnownExhausted) != 0 {
			continue
		}
		h := *a.HeadroomPercent
		valid := true
		if a.OnCredits {
			valid = provider == "codex" && h == 0
		} else {
			valid = h > 0
			for _, w := range a.Windows {
				// Claude's absent informational buckets are displayed but do not gate launches.
				absent := provider == "claude" && w.UsedPercent == nil && w.RemainingPercent == nil && w.Unavailable == "not reported" && w.ResetsAt == nil
				valid = valid && !w.Exhausted && (absent || w.Unavailable == "" && watchPercent(w.UsedPercent) && watchPercent(w.RemainingPercent) && *w.RemainingPercent >= h)
			}
		}
		if valid && (best == nil || h > *best.HeadroomPercent || h == *best.HeadroomPercent && a.Account < best.Account) {
			best = a
		}
	}
	if best == nil {
		return "none known"
	}
	return watchClean(best.Account)
}

func watchClean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u061c' || r >= '\u200b' && r <= '\u200f' || r >= '\u2028' && r <= '\u202e' || r >= '\u2060' && r <= '\u206f' || r == '\ufeff' {
			return '?'
		}
		return r
	}, s)
}

func watchPanelLines(p statusProvider, rows []statusAccount, wall time.Time) []cell {
	lines, _ := watchPanelLayout(p, rows, wall)
	return lines
}

func watchPanelLayout(p statusProvider, rows []statusAccount, wall time.Time) ([]cell, []int) {
	p.Provider, p.Error = watchClean(p.Provider), watchClean(p.Error)
	clean := append([]statusAccount(nil), rows...)
	for i := range clean {
		a := &clean[i]
		a.Account, a.Email, a.Reason = watchClean(a.Account), watchClean(a.Email), watchClean(a.Reason)
		a.CreditsBalance, a.claudeUnavailable = watchClean(a.CreditsBalance), watchClean(a.claudeUnavailable)
		a.Windows = append([]statusWindow(nil), a.Windows...)
		for j := range a.Windows {
			a.Windows[j].Label, a.Windows[j].Unavailable = watchClean(a.Windows[j].Label), watchClean(a.Windows[j].Unavailable)
		}
		a.KnownExhausted = append([]statusKnownExhausted(nil), a.KnownExhausted...)
		for j := range a.KnownExhausted {
			a.KnownExhausted[j].Scope = watchClean(a.KnownExhausted[j].Scope)
		}
	}
	return statusPanelLayout(p, clean, wall)
}

func watchStamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *watchState) lines(now, wall time.Time) []cell {
	lines, _ := s.tableLayout(now, wall, nil, false)
	return lines
}

func (s *watchState) tableLayout(now, wall time.Time, indices []int, filtered bool) ([]cell, map[watchAccountID]int) {
	locations := make(map[watchAccountID]int)
	lines := s.timeLines()
	if s.errorText != "" {
		lines = append(lines, cell{{"ERROR / STALE: " + s.errorText, ansiRed}})
	}
	if s.disabled {
		lines = append(lines, cell{{watchCleanupWarning, ansiRed}})
	}
	if s.snapshot == nil {
		return append(lines, cell{{"No successful snapshot yet; quota unknown.", ""}}), locations
	}
	r := s.snapshot.report
	lines = append(lines, cell{{"Claude: all-model snapshot; launch model may differ.", ansiDim}})
	for _, p := range r.Providers {
		if filtered && s.view.providerFilter > 0 && r.Providers[s.view.providerFilter-1].Provider != p.Provider {
			continue
		}
		label := "Next from snapshot (estimate): "
		if p.Provider == "claude" {
			label = "Next from all-model snapshot (estimate): "
		}
		lines = append(lines, nil, cell{{watchClean(p.Provider) + ": " + label + s.nextAccount(p.Provider, now, wall), ansiDim}})
		var rows []statusAccount
		var ids []watchAccountID
		order := indices
		if !filtered {
			order = make([]int, len(r.Accounts))
			for i := range order {
				order[i] = i
			}
		}
		for _, i := range order {
			a := r.Accounts[i]
			if a.Provider == p.Provider {
				rows = append(rows, a)
				ids = append(ids, watchID(a))
			}
		}
		panel, offsets := watchPanelLayout(p, rows, wall)
		for i, offset := range offsets {
			locations[ids[i]] = len(lines) + offset
		}
		lines = append(lines, panel...)
	}
	if filtered && len(indices) == 0 {
		lines = append(lines, cell{{"No accounts match filters; p/f or / changes filters.", ansiDim}})
	}
	return append(lines, nil, cell{{"r refresh (gated); q/Esc/Ctrl-C quit; arrows/h/l, PgUp/PgDn, Home/End scroll.", ansiDim}}), locations
}

func (s *watchState) timeLines() []cell {
	values := []struct {
		label string
		at    time.Time
	}{{"Last attempt", s.lastAttempt}, {"Last successful completion", s.lastSuccess}}
	if s.snapshot != nil {
		values = append(values, struct {
			label string
			at    time.Time
		}{"Source generated (collection start)", s.snapshot.report.GeneratedAt})
	}
	var lines []cell
	var used [3]bool
	short := []string{"Attempt", "completion (OK)", "source (start)"}
	for i, value := range values {
		if used[i] {
			continue
		}
		label, stamp := value.label, watchStamp(value.at)
		compact := short[i]
		merged := false
		for j := i + 1; j < len(values); j++ {
			if !value.at.IsZero() && !values[j].at.IsZero() && watchStamp(values[j].at) == stamp {
				compact += " / " + short[j]
				used[j] = true
				merged = true
			}
		}
		if merged {
			label = compact
			stamp += " (same UTC second)"
		}
		lines = append(lines, cell{{label + ": " + stamp, ansiDim}})
	}
	return lines
}

type watchOutcome struct {
	report         statusReport
	err            error
	finished, wall time.Time
}

func watchStatus(store *Store, in, out *os.File, providers []string) (err error) {
	signals := newProbeSignalScope()
	defer signals.stopListening()
	extra := make(chan os.Signal, 2)
	signal.Notify(extra, syscall.SIGWINCH, syscall.SIGQUIT)
	defer signal.Stop(extra)
	restore, err := enterWatchTerminal(in, out)
	if err != nil {
		return err
	}
	state := watchState{next: time.Now()}
	var viewport watchViewport
	var keys watchKeys
	results := make(chan watchOutcome, 1)
	var probeCtx context.Context
	var cancel context.CancelFunc
	defer func() {
		signals.cancel()
		if cancel != nil {
			cancel()
		}
		err = errors.Join(err, restore())
		if state.inflight {
			timer := time.NewTimer(watchCleanupGrace)
			defer timer.Stop()
			select {
			case <-results:
			case <-timer.C:
				state.disabled = true
			}
		}
		if state.disabled {
			_, _ = fmt.Fprintln(os.Stderr, watchCleanupWarning)
		}
	}()
	dirty := true
	var lastFrame time.Time
	for {
		if signals.ctx.Err() != nil {
			return nil
		}
		select {
		case sig := <-extra:
			if sig == syscall.SIGQUIT {
				return nil
			}
			dirty = true
		default:
		}
		now := time.Now()
		wall := now.UTC()
		select {
		case outcome := <-results:
			if cancel != nil {
				cancel()
			}
			state.finish(outcome.report, outcome.err, outcome.finished, outcome.wall)
			probeCtx = nil
			dirty = true
		default:
		}
		if probeCtx != nil {
			dirty = state.checkCleanup(now, probeCtx.Err()) || dirty
		}
		width, height, sizeErr := watchTerminalSize(out)
		if sizeErr != nil {
			return sizeErr
		}
		if width >= 20 && height >= 8 && state.begin(now, wall) {
			ctx, stopProbe := context.WithTimeout(signals.ctx, watchDeadline)
			probeCtx, cancel = ctx, stopProbe
			go func() {
				defer stopProbe()
				report, collectErr := collectStatusContext(ctx, store, providers, nil)
				if ctx.Err() != nil {
					collectErr = ctx.Err()
				}
				finished := time.Now()
				results <- watchOutcome{report: report, err: collectErr, finished: finished, wall: finished.UTC()}
			}()
			dirty = true
		}
		if (dirty && now.Sub(lastFrame) >= 250*time.Millisecond) || now.Sub(lastFrame) >= time.Second {
			if err := drawWatch(out, &state, &viewport, width, height, now, wall); err != nil {
				return err
			}
			lastFrame, dirty = now, false
		}
		input, readErr := readWatchInput(in, 50)
		if readErr != nil {
			return readErr
		}
		for _, key := range keys.feed(input, time.Now()) {
			if state.view.handle(key, &state, &viewport, width, height, now, wall) {
				return nil
			}
			dirty = true
		}
	}
}
