package main

import (
	"sort"
	"strings"
)

func buildEnv(base []string, deny []string, overrides map[string]string) []string {
	blocked := make(map[string]bool, len(deny))
	for _, key := range deny {
		blocked[key] = true
	}
	values := make(map[string]string, len(base)+len(overrides))
	for _, item := range base {
		key, value, ok := cutEnv(item)
		if !ok || blocked[key] {
			continue
		}
		if _, forced := overrides[key]; !forced {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func cutEnv(item string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(item, "=")
	return key, value, ok && key != ""
}
