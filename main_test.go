package main

import (
	"bytes"
	"strings"
	"testing"

	"dockback/internal/version"
)

func TestVersionFlagPrintsTheVersionAndStartsNothing(t *testing.T) {
	for _, flag := range []string{"-version", "--version"} {
		var stdout, stderr bytes.Buffer
		if code := runCommand([]string{flag}, &stdout, &stderr); code != 0 {
			t.Errorf("%s exited %d", flag, code)
		}
		if strings.TrimSpace(stdout.String()) != version.Version {
			t.Errorf("%s printed %q, want %q", flag, stdout.String(), version.Version)
		}
	}
}

// Anything unrecognised is refused instead of falling through to the server.
func TestUnknownArgumentsAreRefused(t *testing.T) {
	for _, arg := range []string{"-v", "serve", "--help", "version"} {
		var stdout, stderr bytes.Buffer
		if code := runCommand([]string{arg}, &stdout, &stderr); code != exitUsage {
			t.Errorf("%q exited %d, want %d", arg, code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "unknown argument") {
			t.Errorf("%q: the refusal must say why: %q", arg, stderr.String())
		}
	}
}
