package keep

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKeep puts a program called keep on the override, printing whatever
// the test wants `ready --json` to have said. The point of every case below is
// what Interlock does with an answer it did not expect.
func fakeKeep(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBinary, path)
}

func TestReadyReturnsTheQueueAsRanked(t *testing.T) {
	fakeKeep(t, `cat <<'JSON'
[{"id":"rnc-1","type":"task","title":"First","score":31,"paths":"a.go, b.go"},
 {"id":"rnc-2","type":"bug","title":"Second","score":12}]
JSON`)
	items, ok := Ready(context.Background(), t.TempDir())
	if !ok || len(items) != 2 {
		t.Fatalf("Ready = %#v ok=%v", items, ok)
	}
	if items[0].ID != "rnc-1" || items[1].ID != "rnc-2" {
		t.Fatalf("order changed: %#v", items)
	}
	if got := SplitPaths(items[0].Paths); len(got) != 2 || got[1] != "b.go" {
		t.Fatalf("SplitPaths = %#v", got)
	}
}

// Every one of these means the same thing: Interlock has no work list, and
// nothing about that is an error worth surfacing in a repository that never
// asked for one.
func TestEveryFailureIsSilence(t *testing.T) {
	cases := map[string]string{
		"keep is absent":       "",
		"keep fails":           "exit 3",
		"output is not json":   "echo 'keep: something went wrong'",
		"output is an object":  `echo '{"items":[]}'`,
		"an item has no id":    `echo '[{"title":"nameless"}]'`,
		"one bad item spoils":  `echo '[{"id":"rnc-1"},{"title":"nameless"}]'`,
		"it never says a word": "sleep 30",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			if script == "" {
				t.Setenv(EnvBinary, filepath.Join(t.TempDir(), "not-installed"))
			} else {
				fakeKeep(t, script)
			}
			items, ok := Ready(context.Background(), t.TempDir())
			if ok || items != nil {
				t.Fatalf("Ready = %#v ok=%v, want silence", items, ok)
			}
		})
	}
}

// An answer this cannot read must never be used in part. Trusting the half that
// parsed would mean starting an agent on a brief assembled from a shape nobody
// promised.
func TestAPartlyReadableAnswerIsNotUsedAtAll(t *testing.T) {
	fakeKeep(t, `echo '[{"id":"rnc-1","title":"Real"},{"title":"Broken"}]'`)
	if items, ok := Ready(context.Background(), t.TempDir()); ok {
		t.Fatalf("Ready = %#v ok=%v", items, ok)
	}
}

func TestOffDisablesTheJoinFromThisSide(t *testing.T) {
	fakeKeep(t, `echo '[{"id":"rnc-1","title":"Real"}]'`)
	t.Setenv(EnvBinary, "off")
	if _, ok := Ready(context.Background(), t.TempDir()); ok {
		t.Fatal("the queue was consulted with the join turned off")
	}
	if Available() {
		t.Fatal("Available reported a work list with the join turned off")
	}
}

// The brief is what a model reads before it starts. Everything in it should be
// about the work; everything about keep should not.
func TestBriefIsWorkAndNothingElse(t *testing.T) {
	item := Item{
		ID: "rnc-1", Type: "task", Score: 31,
		Title: "Refactor the shared helper", Body: "It has three callers.",
		Acceptance: "Tests pass.", Why: "It blocks the migration.",
		Paths: "internal/shared.go",
	}
	brief := item.Brief()

	for _, want := range []string{
		"Refactor the shared helper", "It has three callers.",
		"Tests pass.", "It blocks the migration.", "internal/shared.go",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
	// Identifiers and scores are how a work store talks about work, not how a
	// person briefs a colleague.
	for _, unwanted := range []string{"rnc-1", "31", "task"} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("brief leaked %q:\n%s", unwanted, brief)
		}
	}
	// An agent given files to touch is told to check them first, because
	// another agent may be in one right now.
	if !strings.Contains(brief, "interlock check") {
		t.Errorf("brief does not tell the agent to check first:\n%s", brief)
	}
}

func TestABareItemStillBriefs(t *testing.T) {
	brief := Item{ID: "rnc-2", Title: "Just a title"}.Brief()
	if brief != "Just a title" {
		t.Fatalf("brief = %q", brief)
	}
}

// TestBinaryLookup covers how keep is found.
//
// $PATH is rewritten to a directory this test owns rather than mocked, so the
// thing under test is the real exec.LookPath and not a stand-in for it. That
// matters: the bug this guards against is a lookup that finds nothing, and a
// fake lookup cannot fail the way a real one does.
func TestBinaryLookup(t *testing.T) {
	// EnvBinary short-circuits the whole search, so it must be cleared or
	// none of these cases reach the code they are about. t.Setenv registers
	// the restore; Unsetenv is what makes it genuinely absent.
	clear := func(t *testing.T, name string) {
		t.Helper()
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	// onPath writes an executable of each given name into a fresh directory
	// and makes that directory the entire $PATH.
	onPath := func(t *testing.T, names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", dir)
		return dir
	}

	t.Run("keep on $PATH is found", func(t *testing.T) {
		clear(t, EnvBinary)
		dir := onPath(t, "keep")
		got, ok := Binary()
		if !ok || got != filepath.Join(dir, "keep") {
			t.Fatalf("Binary() = %q, %v; want the keep on $PATH", got, ok)
		}
	})

	t.Run("the override wins over $PATH", func(t *testing.T) {
		onPath(t, "keep")
		t.Setenv(EnvBinary, "/somewhere/keep")
		got, ok := Binary()
		if !ok || got != "/somewhere/keep" {
			t.Fatalf("Binary() = %q, %v; want the %s override", got, ok, EnvBinary)
		}
	})

	t.Run("the override can turn the join off", func(t *testing.T) {
		onPath(t, "keep")
		t.Setenv(EnvBinary, "off")
		if _, ok := Binary(); ok {
			t.Fatal("the join stayed on with the variable set to off")
		}
	})

	t.Run("not installed is silence", func(t *testing.T) {
		clear(t, EnvBinary)
		onPath(t)
		if _, ok := Binary(); ok {
			t.Fatal("Binary reported a binary with an empty $PATH")
		}
	})
}
