package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

func execute(inv invocation) (int, error) {
	if inv.verb == "launch" && nativeInfoArgs(inv.provider, inv.args) {
		return 0, nativeInfo(inv)
	}
	if inv.verb == "doctor" {
		providers := []string{inv.provider}
		if inv.provider == "" {
			providers = []string{"codex", "claude"}
		}
		return doctor(os.Stdout, providers...)
	}
	store, err := storeFromEnv()
	if err != nil {
		return 1, err
	}
	switch inv.verb {
	case "mcp-share":
		signals := newProbeSignalScope()
		defer signals.stopListening()
		_, err := syncSharedMCP(signals.ctx, store, true, os.Stdout)
		return 0, err
	case "mcp-login":
		return 0, loginMCP(store, inv, os.Stdin, os.Stdout)
	case "login":
		return 0, login(store, inv)
	case "status":
		providers := []string{inv.provider}
		if inv.provider == "" {
			providers = []string{"codex", "claude"}
		}
		return 0, status(store, os.Stdout, providers...)
	case "launch":
		return 0, launch(store, inv)
	default:
		return 2, fmt.Errorf("%w: unknown operation", errUsage)
	}
}

func nativeInfoArgs(provider string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch provider {
	case "codex":
		return args[0] == "-h" || args[0] == "--help" || args[0] == "-V" || args[0] == "--version"
	case "claude":
		return args[0] == "-h" || args[0] == "--help" || args[0] == "-v" || args[0] == "--version"
	default:
		return false
	}
}

func nativeInfo(inv invocation) error {
	path, err := findNative(inv.provider)
	if err != nil {
		return err
	}
	var env []string
	if inv.provider == "codex" {
		env = codexEnv("", os.Environ())
	} else {
		env = claudeEnv("", os.Environ())
	}
	return execNative(nil, path, inv.args, env)
}

func login(store *Store, inv invocation) error {
	path, err := findNative(inv.provider)
	if err != nil {
		return err
	}
	account, err := store.EnsureAccount(inv.provider, inv.account)
	if err != nil {
		return err
	}
	signals := newProbeSignalScope()
	err = shareConfigForSession(signals.ctx, store, inv.provider, os.Stderr)
	if err == nil && inv.provider == "codex" {
		err = syncSharedMCPForSession(signals.ctx, store, nil, os.Stderr)
	}
	if signals.stopListening() && err == nil {
		err = context.Canceled
	}
	if err != nil {
		return err
	}
	lock, err := store.Lock(inv.provider, inv.account)
	if err != nil {
		return err
	}
	defer lock.Close()
	if inv.provider == "claude" {
		if err := store.invalidateClaudeCacheOnLogin(account); err != nil {
			return errors.New("cannot safely invalidate Claude usage state")
		}
	}
	if err := os.Chdir(account.NativeDir); err != nil {
		return errors.New("cannot enter the isolated native profile")
	}
	if inv.provider == "codex" {
		args := append([]string{"login"}, inv.args...)
		return execNative(lock, path, args, codexEnv(account.NativeDir, os.Environ()))
	}
	args := append([]string{"auth", "login"}, inv.args...)
	return execNative(lock, path, args, claudeEnv(account.NativeDir, os.Environ()))
}

// Ordinary sessions keep each profile's existing MCP settings when sharing
// fails. Native MCP commands stay strict so they never act on unshared settings.
func syncSharedMCPForSession(ctx context.Context, store *Store, args []string, out io.Writer) error {
	_, err := syncSharedMCP(ctx, store, false, out)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "mcp" {
			return err
		}
	}
	fmt.Fprintf(out, "codator: warning: MCP settings were not shared (%v); continuing with each profile's existing MCP settings\n", err)
	return nil
}

func status(store *Store, out io.Writer, providers ...string) error {
	signals := newProbeSignalScope()
	defer signals.stopListening()
	for _, provider := range providers {
		if err := signals.ctx.Err(); err != nil {
			return err
		}
		accounts, err := store.Accounts(provider)
		if err != nil {
			fmt.Fprintf(out, "%s: unavailable (%v)\n", provider, err)
			continue
		}
		if len(accounts) == 0 {
			fmt.Fprintf(out, "%s: no accounts enrolled\n", provider)
			continue
		}
		for _, account := range accounts {
			if err := signals.ctx.Err(); err != nil {
				return err
			}
			if account.Err != nil {
				fmt.Fprintf(out, "%s %s: unknown (unsafe profile directory)\n", provider, account.Name)
				continue
			}
			lock, err := store.Lock(provider, account.Name)
			if errors.Is(err, ErrAccountBusy) {
				fmt.Fprintf(out, "%s %s: busy\n", provider, account.Name)
				continue
			}
			if err != nil {
				fmt.Fprintf(out, "%s %s: unknown (cannot lock profile)\n", provider, account.Name)
				continue
			}
			q, err := probeAccount(signals.ctx, store, provider, account, "")
			now := time.Now()
			var claudeUsage string
			if provider == "claude" {
				state, found, cacheErr := store.readClaudeProbeCache(account, now.UTC())
				claudeUsage = claudeStatus(state, found, cacheErr, now)
			}
			lock.Close()
			if signalErr := signals.ctx.Err(); signalErr != nil {
				return signalErr
			}
			if provider == "claude" {
				fmt.Fprintf(out, "%s %s: %s\n", provider, account.Name, claudeUsage)
			} else {
				fmt.Fprintf(out, "%s %s: %s\n", provider, account.Name, quotaStatus(q, err, now))
			}
		}
	}
	return nil
}

func quotaStatus(q quota, err error, now time.Time) string {
	lines := []string{quotaHeadline(q, err, now)}
	for _, window := range q.Windows {
		lines = append(lines, window.Label+": "+windowText(window.Used, window.ResetsAt, now))
	}
	if q.ResetCredits > 0 {
		lines = append(lines, fmt.Sprintf("Rate-limit resets available in Codex: %d", q.ResetCredits))
	}
	return strings.Join(lines, "\n  ")
}

func quotaHeadline(q quota, err error, now time.Time) string {
	if err != nil {
		return "unknown (usage check failed)"
	}
	if q.Eligible {
		return fmt.Sprintf("%.1f%% headroom", q.Headroom)
	}
	if q.Reason == "" {
		q.Reason = "usage is unknown"
	}
	if !q.Known {
		return "unknown (" + q.Reason + ")"
	}
	// An exhausted account is usable again once its last full window resets.
	var until time.Time
	for _, window := range q.Windows {
		if window.Used >= 100 && window.ResetsAt.After(until) {
			until = window.ResetsAt
		}
	}
	if until.After(now) {
		return "limit reached until " + whenText(until, now)
	}
	return "ineligible (" + q.Reason + ")"
}

func launch(store *Store, inv invocation) error {
	signals := newProbeSignalScope()
	defer signals.stopListening()
	if err := signals.ctx.Err(); err != nil {
		return err
	}
	path, err := findNative(inv.provider)
	if err != nil {
		return err
	}
	if err := shareConfigForSession(signals.ctx, store, inv.provider, os.Stderr); err != nil {
		return err
	}
	if inv.provider == "codex" {
		if err := syncSharedMCPForSession(signals.ctx, store, inv.args, os.Stderr); err != nil {
			return err
		}
	}
	accounts, err := store.Accounts(inv.provider)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no %s accounts enrolled; run codator login %s NAME", inv.provider, inv.provider)
	}
	if err := signals.ctx.Err(); err != nil {
		return err
	}
	defaultModel := sync.OnceValue(func() claudeModelFamily { return claudeRuntimeDefault(signals.ctx, path) })
	if inv.account != "" {
		return launchExplicit(signals, store, path, inv, accounts, defaultModel)
	}
	return launchBest(signals, store, path, inv, accounts, defaultModel)
}

func launchExplicit(signals *probeSignalScope, store *Store, path string, inv invocation, accounts []Account, defaultModel func() claudeModelFamily) error {
	var selected *Account
	for i := range accounts {
		if accounts[i].Name == inv.account {
			selected = &accounts[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("no %s account named %q", inv.provider, inv.account)
	}
	if selected.Err != nil {
		return errors.New("selected profile has an unsafe native directory")
	}
	lock, err := store.Lock(inv.provider, selected.Name)
	if err != nil {
		return err
	}
	defer lock.Close()
	q, err := probeAccount(signals.ctx, store, inv.provider, *selected, launchClaudeModel(inv.provider, inv.args, *selected, defaultModel))
	if signalErr := signals.ctx.Err(); signalErr != nil {
		return signalErr
	}
	if err != nil && !q.Subscription {
		return claudeVerificationError("cannot verify the selected subscription profile", q)
	}
	if err != nil && q.Known {
		return claudeVerificationError("cannot verify usage for the selected subscription profile", q)
	}
	if !q.Subscription {
		return claudeVerificationError("selected profile is not verified as a subscription account", q)
	}
	if !canLaunchExplicit(q) {
		return fmt.Errorf("selected account is ineligible: %s", q.Reason)
	}
	return execSelected(signals, store, lock, path, inv.provider, inv.args, *selected, q)
}

type lockedCandidate struct {
	candidate
	lock *AccountLock
}

func launchBest(signals *probeSignalScope, store *Store, path string, inv invocation, accounts []Account, defaultModel func() claudeModelFamily) error {
	var usable []lockedCandidate
	var skipped []string
	for _, account := range accounts {
		if err := signals.ctx.Err(); err != nil {
			closeCandidateLocks(usable)
			return err
		}
		if account.Err != nil {
			skipped = append(skipped, account.Name+": unsafe profile")
			continue
		}
		lock, err := store.Lock(inv.provider, account.Name)
		if errors.Is(err, ErrAccountBusy) {
			skipped = append(skipped, account.Name+": busy")
			continue
		}
		if err != nil {
			skipped = append(skipped, account.Name+": lock failed")
			continue
		}
		q, err := probeAccount(signals.ctx, store, inv.provider, account, launchClaudeModel(inv.provider, inv.args, account, defaultModel))
		if signalErr := signals.ctx.Err(); signalErr != nil {
			lock.Close()
			closeCandidateLocks(usable)
			return signalErr
		}
		if err != nil {
			lock.Close()
			skipped = append(skipped, account.Name+": usage check failed")
			continue
		}
		if !q.Subscription || !q.Eligible {
			lock.Close()
			skipped = append(skipped, account.Name+": "+quotaReason(q))
			continue
		}
		usable = append(usable, lockedCandidate{candidate: candidate{Account: account, Quota: q}, lock: lock})
	}
	if err := signals.ctx.Err(); err != nil {
		closeCandidateLocks(usable)
		return err
	}
	if len(usable) == 0 {
		if len(skipped) == 0 {
			return errors.New("no eligible subscription accounts")
		}
		return fmt.Errorf("no eligible %s accounts (%s)", inv.provider, strings.Join(skipped, "; "))
	}
	items := make([]candidate, len(usable))
	for i := range usable {
		items[i] = usable[i].candidate
	}
	winner, _ := bestCandidate(items)
	winnerLock := usable[0].lock
	for _, item := range usable {
		if item.Account.Name == winner.Account.Name {
			winnerLock = item.lock
		} else {
			item.lock.Close()
		}
	}
	defer winnerLock.Close()
	return execSelected(signals, store, winnerLock, path, inv.provider, inv.args, winner.Account, winner.Quota)
}

func closeCandidateLocks(candidates []lockedCandidate) {
	for _, item := range candidates {
		item.lock.Close()
	}
}

func quotaReason(q quota) string {
	if claudeRetryHint(q.Reason) != "" {
		return q.Reason
	}
	if q.Reason == "" {
		return "usage unknown"
	}
	if q.Known && !q.Eligible {
		return q.Reason
	}
	return "usage unknown"
}

func claudeVerificationError(message string, q quota) error {
	if retry := claudeRetryHint(q.Reason); retry != "" {
		return fmt.Errorf("%s: %s", message, retry)
	}
	return errors.New(message)
}

func probeAccount(ctx context.Context, store *Store, provider string, account Account, model claudeModelFamily) (quota, error) {
	switch provider {
	case "codex":
		return probeCodexWithContext(ctx, account)
	case "claude":
		return store.probeClaudeAccount(ctx, account, model)
	default:
		return quota{}, errors.New("unknown usage provider")
	}
}

func execSelected(signals *probeSignalScope, store *Store, lock *AccountLock, path, provider string, nativeArgs []string, account Account, q quota) error {
	if provider == "claude" {
		if err := recoverClaudeSetup(signals.ctx, store, account); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(os.Stderr, "codator: Claude setup recovery skipped: %v\n", err)
		}
		if err := signals.ctx.Err(); err != nil {
			return err
		}
	}
	if signals.stopListening() {
		return context.Canceled
	}
	usage := ""
	if !q.Known {
		usage = " (usage unknown)"
	}
	fmt.Fprintf(os.Stderr, "codator: using %s account %s%s\n", provider, account.Name, usage)
	if !credentialMutation(provider, nativeArgs) {
		if err := lock.Close(); err != nil {
			return errors.New("cannot release session lock")
		}
		lock = nil
	}
	if provider == "codex" {
		return execNative(lock, path, nativeArgs, codexEnv(account.NativeDir, os.Environ()))
	}
	return execNative(lock, path, nativeArgs, claudeEnv(account.NativeDir, os.Environ()))
}

func credentialMutation(provider string, args []string) bool {
	for _, arg := range args {
		// Claude can still dispatch credential subcommands after --.
		if arg == "--" && provider == "codex" {
			return false
		}
		if arg == "login" || arg == "logout" {
			return true
		}
		if provider == "claude" && arg == "setup-token" {
			return true
		}
	}
	return false
}
