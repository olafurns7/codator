package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func main() { os.Exit(run(invocationArgs(os.Args))) }

func invocationArgs(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	name := filepath.Base(argv[0])
	if validProvider(name) {
		return append([]string{name}, argv[1:]...)
	}
	return argv[1:]
}

func run(args []string) int {
	inv, err := parseInvocation(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codator: %v\n\n%s\n", err, usage)
		return 2
	}
	if inv.verb == "help" {
		fmt.Println(usage)
		return 0
	}
	code, err := execute(inv)
	if err == nil {
		return code
	}
	if errors.Is(err, errUsage) {
		fmt.Fprintf(os.Stderr, "codator: %v\n\n%s\n", err, usage)
		return 2
	}
	fmt.Fprintf(os.Stderr, "codator: %v\n", err)
	return 1
}
