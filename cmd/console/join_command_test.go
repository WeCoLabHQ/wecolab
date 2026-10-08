package main

import (
	"strings"
	"testing"
)

func TestJoinCommandRequiresVerifiedLocalRelease(t *testing.T) {
	cmd := (&server{}).joinCommand("wcl2.example-invite")
	if !strings.Contains(cmd, "docs/install.md") || !strings.Contains(cmd, "verify") || !strings.Contains(cmd, `bash "$HOME/wecolab-release/install.sh" 'wcl2.example-invite'`) {
		t.Fatalf("join instruction lacks verified local install: %q", cmd)
	}
	if strings.Contains(cmd, "| sudo") || strings.Contains(cmd, "curl ") {
		t.Fatalf("join command executes unverified network script: %q", cmd)
	}
}
