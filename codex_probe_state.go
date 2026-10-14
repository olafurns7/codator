package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
)

const codexProbeStateName = ".codex-probe.json"

type codexProbeState struct {
	AttemptedAt  time.Time `json:"attempted_at"`
	AuthRequired bool      `json:"auth_required"`
}

// Codex has no usage cooldown to bypass. Persist only auth rejection and its refresh fence.
func (s *Store) probeCodexAccount(ctx context.Context, account Account, refresh bool) (quota, error) {
	dir, err := s.accountRoot(account.Provider, account.Name, false)
	if err != nil {
		return quota{}, errors.New("Codex local probe state is unavailable")
	}
	defer dir.Close()
	state, err := readCodexProbeState(dir)
	if err != nil {
		return quota{Reason: "Codex local probe state is unsafe or unavailable"}, err
	}
	now := time.Now().UTC()
	if state.AuthRequired && (!refresh || now.Before(state.AttemptedAt.Add(probeRefreshInterval))) {
		return codexProbeFailure(account, quota{}, &probeFailure{Kind: "auth"}), nil
	}
	// Refreshing an auth denial stays fail closed if the process dies before completion.
	if state.AuthRequired {
		state.AttemptedAt = now
		data, _ := json.Marshal(state)
		if err := replaceProbeCacheFile(dir, data, codexProbeStateName, ".codex-probe.tmp"); err != nil {
			return quota{}, errors.New("cannot record Codex auth refresh")
		}
	}
	q, err := probeCodexWithContext(ctx, account)
	if ctx.Err() != nil {
		return quota{}, ctx.Err()
	}
	if q.ProbeFailure == "auth" {
		data, _ := json.Marshal(codexProbeState{AttemptedAt: now, AuthRequired: true})
		if writeErr := replaceProbeCacheFile(dir, data, codexProbeStateName, ".codex-probe.tmp"); writeErr != nil {
			return q, errors.New("cannot persist Codex login failure")
		}
	} else if state.AuthRequired && err == nil && q.Subscription && q.Known && q.ProbeFailure == "" {
		if removeErr := dir.Remove(codexProbeStateName); removeErr != nil {
			return quota{}, errors.New("cannot clear Codex login failure")
		}
	} else if state.AuthRequired {
		// Failed or incomplete usage cannot disprove the previous auth denial.
		return codexProbeFailure(account, quota{}, &probeFailure{Kind: "auth"}), nil
	}
	return q, err
}

func readCodexProbeState(dir *os.Root) (codexProbeState, error) {
	var state codexProbeState
	file, err := openPrivateFile(dir, codexProbeStateName, os.O_RDONLY, 0600)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, errors.New("Codex local probe state is unsafe or unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(data) > 1024 {
		return state, errors.New("Codex local probe state is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF || state.AttemptedAt.IsZero() || state.AttemptedAt.After(time.Now().Add(2*time.Minute)) || !state.AuthRequired {
		return codexProbeState{}, errors.New("Codex local probe state is invalid")
	}
	return state, nil
}

func (s *Store) invalidateCodexProbeOnLogin(account Account) error {
	dir, err := s.accountRoot(account.Provider, account.Name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	if _, err := readCodexProbeState(dir); err != nil {
		return err
	}
	if err := dir.Remove(codexProbeStateName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
