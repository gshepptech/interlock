package rounce

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRounce puts a program called rounce on the override, printing whatever
// the test wants `ready --json` to have said. The point of every case below is
// what Interlock does with an answer it did not expect.
func fakeRounce(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rounce")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBinary, path)
}

func TestReadyReturnsTheQueueAsRanked(t *testing.T) {
	fakeRounce(t, `cat <<'JSON'
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
		"rounce is absent":     "",
		"rounce fails":         "exit 3",
		"output is not json":   "echo 'rounce: something went wrong'",
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
				fakeRounce(t, script)
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
	fakeRounce(t, `echo '[{"id":"rnc-1","title":"Real"},{"title":"Broken"}]'`)
	if items, ok := Ready(context.Background(), t.TempDir()); ok {
		t.Fatalf("Ready = %#v ok=%v", items, ok)
	}
}

func TestOffDisablesTheJoinFromThisSide(t *testing.T) {
	fakeRounce(t, `echo '[{"id":"rnc-1","title":"Real"}]'`)
	t.Setenv(EnvBinary, "off")
	if _, ok := Ready(context.Background(), t.TempDir()); ok {
		t.Fatal("the queue was consulted with the join turned off")
	}
	if Available() {
		t.Fatal("Available reported a work list with the join turned off")
	}
}

// The brief is what a model reads before it starts. Everything in it should be
// about the work; everything about rounce should not.
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
