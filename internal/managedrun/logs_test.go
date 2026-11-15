package managedrun

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func logStore(t *testing.T) (*Store, string) {
	t.Helper()
	store, _ := testStore(t)
	record := mustCreate(t, store)
	return store, record.ID()
}

func write(t *testing.T, w *LogWriter, text string) {
	t.Helper()
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestLogsRotateAndStayBounded(t *testing.T) {
	store, id := logStore(t)
	writer, err := store.OpenLog(id, Stdout)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer writer.Close()
	writer.max = 1024

	for i := range 10 {
		write(t, writer, strings.Repeat(fmt.Sprintf("%d", i), 400))
	}

	for _, path := range []string{store.LogPath(id, Stdout), store.RotatedLogPath(id, Stdout)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() > 2*writer.max {
			t.Errorf("%s is %d bytes, want a bounded file", path, info.Size())
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
	}
	// Two files and no more: the whole point of rotation is that a chatty agent
	// cannot fill a disk while nobody is watching.
	entries, err := os.ReadDir(store.Dir(id))
	if err != nil {
		t.Fatal(err)
	}
	logs := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stdout.log") {
			logs++
		}
	}
	if logs != 2 {
		t.Fatalf("%d stdout files, want 2", logs)
	}
}

func TestTailReadsTheEndAndSaysWhenItSkipped(t *testing.T) {
	store, id := logStore(t)
	writer, _ := store.OpenLog(id, Stdout)
	defer writer.Close()
	write(t, writer, strings.Repeat("a", 100)+"tail")

	chunk, cursor, err := store.Tail(id, Stdout, 4)
	if err != nil || string(chunk.Bytes) != "tail" || !chunk.Truncated {
		t.Fatalf("Tail = %q truncated=%v err=%v", chunk.Bytes, chunk.Truncated, err)
	}
	if !cursor.Started() || cursor.offset != 104 {
		t.Fatalf("cursor = %#v", cursor)
	}

	full, _, err := store.Tail(id, Stdout, 1000)
	if err != nil || full.Truncated || len(full.Bytes) != 104 {
		t.Fatalf("Tail of everything = %d bytes truncated=%v err=%v", len(full.Bytes), full.Truncated, err)
	}
}

func TestTailReachesIntoTheRotatedFile(t *testing.T) {
	store, id := logStore(t)
	writer, _ := store.OpenLog(id, Stdout)
	defer writer.Close()
	writer.max = 16
	write(t, writer, "older output\n")
	write(t, writer, "newer output\n")

	chunk, _, err := store.Tail(id, Stdout, 1000)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got := string(chunk.Bytes); got != "older output\nnewer output\n" {
		t.Fatalf("Tail = %q; a rotated file is still recent output", got)
	}
}

func TestFollowSeesEveryByteExactlyOnceAcrossRotation(t *testing.T) {
	store, id := logStore(t)
	writer, _ := store.OpenLog(id, Stdout)
	defer writer.Close()
	writer.max = 64

	var want, got bytes.Buffer
	_, cursor, err := store.Tail(id, Stdout, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		line := fmt.Sprintf("line %02d ..........\n", i)
		write(t, writer, line)
		want.WriteString(line)

		chunk, next, err := store.Follow(id, Stdout, cursor, 4096)
		if err != nil {
			t.Fatalf("Follow: %v", err)
		}
		got.Write(chunk.Bytes)
		cursor = next
	}
	if got.String() != want.String() {
		t.Fatalf("followed %q\nwant %q", got.String(), want.String())
	}

	// Nothing new means nothing returned, rather than the same bytes again.
	chunk, _, err := store.Follow(id, Stdout, cursor, 4096)
	if err != nil || len(chunk.Bytes) != 0 {
		t.Fatalf("idle Follow = %q err=%v", chunk.Bytes, err)
	}
}

func TestFollowReportsOutputItCouldNotShow(t *testing.T) {
	store, id := logStore(t)
	writer, _ := store.OpenLog(id, Stdout)
	defer writer.Close()
	write(t, writer, "first\n")

	_, cursor, err := store.Tail(id, Stdout, 4096)
	if err != nil {
		t.Fatal(err)
	}
	write(t, writer, strings.Repeat("x", 100))
	chunk, cursor, err := store.Follow(id, Stdout, cursor, 10)
	if err != nil || !chunk.Truncated || len(chunk.Bytes) != 10 {
		t.Fatalf("bounded Follow = %d bytes truncated=%v err=%v", len(chunk.Bytes), chunk.Truncated, err)
	}

	// A log that shrank was not rotated; it was replaced. Saying output was lost
	// is better than reading the middle of a new file as the end of an old one.
	if err := os.WriteFile(store.LogPath(id, Stdout), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	chunk, _, err = store.Follow(id, Stdout, cursor, 4096)
	if err != nil || !chunk.Truncated || string(chunk.Bytes) != "short" {
		t.Fatalf("Follow after truncation = %q truncated=%v err=%v", chunk.Bytes, chunk.Truncated, err)
	}
}

func TestFollowAndWriteDoNotRace(t *testing.T) {
	store, id := logStore(t)
	var wg sync.WaitGroup
	for _, stream := range []Stream{Stdout, Stderr} {
		writer, err := store.OpenLog(id, stream)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			defer writer.Close()
			writer.max = 256
			for i := range 200 {
				if _, err := writer.Write(fmt.Appendf(nil, "%s %03d\n", stream, i)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		})
		wg.Go(func() {
			cursor := Cursor{}
			for range 200 {
				chunk, next, err := store.Follow(id, stream, cursor, 4096)
				if err != nil {
					t.Errorf("Follow: %v", err)
					return
				}
				_ = chunk
				cursor = next
			}
		})
	}
	wg.Wait()
}

func TestChunkTextIsSafeToDisplay(t *testing.T) {
	chunk := LogChunk{Bytes: []byte("\x1b[31mred\x1b[0m\nplain\ttext\n")}
	if got := chunk.Text(); got != "red\nplain text\n" {
		t.Fatalf("Text = %q", got)
	}
}

func TestRedactHidesTheTwoCasesThatHappen(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		values []string
		want   string
	}{
		{"an environment value echoed back", "using key EXAMPLE-not-a-real-key today",
			[]string{"EXAMPLE-not-a-real-key"}, "using key [redacted] today"},
		{"a short value is not a credential", "the id is abc", []string{"abc"}, "the id is abc"},
		{"an assignment", `GITHUB_TOKEN=EXAMPLE_NOT_A_REAL_TOKEN`, nil, "GITHUB_TOKEN=[redacted]"},
		{"a json field", `{"api_key": "abcdefgh"}`, nil, `{"api_key":[redacted]`},
		{"ordinary output", "wrote server/api.go", nil, "wrote server/api.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.text, tc.values); got != tc.want {
				t.Fatalf("Redact = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogReadsRefuseUnknownRunsAndStreams(t *testing.T) {
	store, id := logStore(t)
	if _, _, err := store.Tail("../escape", Stdout, 10); err == nil {
		t.Fatal("Tail accepted a traversal id")
	}
	if _, _, err := store.Tail(id, "prompt", 10); err == nil {
		t.Fatal("Tail accepted an unknown stream")
	}
	if _, err := store.OpenLog(id, "prompt"); err == nil {
		t.Fatal("OpenLog accepted an unknown stream")
	}
}
