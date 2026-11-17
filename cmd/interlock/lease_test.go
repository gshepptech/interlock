package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaseReleaseCLIUsesConfirmedSharedControl(t *testing.T) {
	dir := bare(t)
	path := filepath.Join(dir, "src/api/users.go")
	if res := hook(t, dir, "PreToolUse", "claude", write("release-session", path)); res.exit != 0 {
		t.Fatalf("hook failed: %s", res.stderr)
	}
	wrong := cli(t, dir, "lease", "release", "release-session", "--confirm", "wrong")
	if wrong.exit == 0 || !strings.Contains(wrong.stdout, "confirmation") {
		t.Fatalf("wrong confirmation: stdout=%s stderr=%s", wrong.stdout, wrong.stderr)
	}
	if status := cli(t, dir, "status"); !strings.Contains(status.stdout, "src/api/users.go") {
		t.Fatal("wrong confirmation released the lease")
	}
	released := cli(t, dir, "lease", "release", "release-session", "--confirm", "release-session")
	if released.exit != 0 || !strings.Contains(released.stdout, "released 1 leases") {
		t.Fatalf("release: stdout=%s stderr=%s", released.stdout, released.stderr)
	}
	if status := cli(t, dir, "status"); strings.Contains(status.stdout, "src/api/users.go") {
		t.Fatal("confirmed release left the lease")
	}
}
