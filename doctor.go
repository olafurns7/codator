package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

const sandboxGuide = "https://learn.chatgpt.com/docs/sandboxing#prerequisites"

var doctorProbeTimeout = 5 * time.Second

func doctor(out io.Writer, providers ...string) (int, error) {
	signals := newProbeSignalScope()
	defer signals.stopListening()
	failed := false
	for _, provider := range providers {
		if err := signals.ctx.Err(); err != nil {
			return 1, err
		}
		if !doctorProvider(signals.ctx, out, provider, runtime.GOOS) {
			failed = true
		}
	}
	if err := signals.ctx.Err(); err != nil {
		return 1, err
	}
	if failed {
		return 1, errors.New("selected checks need attention")
	}
	return 0, nil
}

func doctorProvider(ctx context.Context, out io.Writer, provider, platform string) bool {
	path, err := findNative(provider)
	if err != nil {
		fmt.Fprintf(out, "%s: missing native CLI (%v)\n", provider, err)
		return false
	}
	fmt.Fprintf(out, "%s: installed (%s)\n", provider, path)
	if provider != "codex" {
		return true
	}
	switch platform {
	case "darwin":
		fmt.Fprintln(out, "codex: sandbox ready (macOS uses Seatbelt; bwrap is not required)")
		return true
	case "linux":
		return doctorLinuxSandbox(ctx, out)
	default:
		fmt.Fprintf(out, "codex: sandbox prerequisites are not checked on %s; see %s\n", platform, sandboxGuide)
		return true
	}
}

func doctorLinuxSandbox(parent context.Context, out io.Writer) bool {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		fmt.Fprintf(out, "codex: sandbox blocked (bwrap is missing; install the bubblewrap package with your Linux distribution package manager: %s)\n", sandboxGuide)
		return false
	}
	ctx, cancel := context.WithTimeout(parent, doctorProbeTimeout)
	defer cancel()
	cmd := probeCommand(ctx, bwrap, "--unshare-user", "--unshare-net", "--die-with-parent", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "/bin/true")
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(out, "codex: sandbox blocked (cannot start bwrap; see %s)\n", sandboxGuide)
		return false
	}
	if err := waitProbe(cmd); err != nil {
		reason := "bwrap could not create an isolated user and network namespace"
		if ctx.Err() != nil {
			reason = "bwrap namespace check timed out"
		}
		fmt.Fprintf(out, "codex: sandbox blocked (%s; on Ubuntu 24.04 install apparmor-profiles and apparmor-utils, copy /usr/share/apparmor/extra-profiles/bwrap-userns-restrict to /etc/apparmor.d/, and load it with apparmor_parser -r; see %s)\n", reason, sandboxGuide)
		return false
	}
	fmt.Fprintln(out, "codex: sandbox ready (bwrap can create isolated user and network namespaces)")
	return true
}
