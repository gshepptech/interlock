package main

import (
	"strings"
	"testing"
)

func TestHandoffRequestCLIIsVisibleDeduplicatedAndResolvable(t *testing.T) {
	dir := bare(t)
	args := []string{"handoff", "request", "--from", "s1", "--target-kind", "agent", "--to", "codex", "--note", "continue API work"}
	created := cli(t, dir, args...)
	if created.exit != 0 || !strings.Contains(created.stdout, "handoff request created") {
		t.Fatalf("request: stdout=%s stderr=%s", created.stdout, created.stderr)
	}
	var id string
	for line := range strings.SplitSeq(created.stdout, "\n") {
		if strings.HasPrefix(line, "request: ") {
			id = strings.TrimSpace(strings.TrimPrefix(line, "request: "))
		}
	}
	if id == "" {
		t.Fatalf("request id missing: %s", created.stdout)
	}
	duplicate := cli(t, dir, args...)
	if duplicate.exit != 0 || !strings.Contains(duplicate.stdout, "already exists") ||
		!strings.Contains(duplicate.stdout, id) {
		t.Fatalf("duplicate: stdout=%s stderr=%s", duplicate.stdout, duplicate.stderr)
	}
	for _, command := range [][]string{{"handoff", "list"}, {"handoff", "show", id}} {
		result := cli(t, dir, command...)
		if result.exit != 0 || !strings.Contains(result.stdout, id) {
			t.Fatalf("%v: stdout=%s stderr=%s", command, result.stdout, result.stderr)
		}
	}
	resolved := cli(t, dir, "handoff", "resolve", id, "--confirm", id)
	if resolved.exit != 0 || !strings.Contains(resolved.stdout, "resolved") {
		t.Fatalf("resolve: stdout=%s stderr=%s", resolved.stdout, resolved.stderr)
	}
	if shown := cli(t, dir, "handoff", "show", id); !strings.Contains(shown.stdout, "status: resolved") {
		t.Fatalf("resolved show: %s%s", shown.stdout, shown.stderr)
	}
}
