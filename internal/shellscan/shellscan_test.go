package shellscan

import (
	"slices"
	"strings"
	"testing"
)

// The bypass this package exists to close.
func TestInPlaceSedIsCaught(t *testing.T) {
	got := Scan(`sed -i '' 's/old/new/' src/api/users.go`)

	if got.Confidence != Likely {
		t.Errorf("Confidence = %v, want Likely", got.Confidence)
	}
	if !slices.Contains(got.Paths, "src/api/users.go") {
		t.Errorf("Paths = %v, want src/api/users.go", got.Paths)
	}
	// The sed script must not be mistaken for a path.
	for _, p := range got.Paths {
		if p == "s/old/new/" {
			t.Error("the sed script was treated as a path")
		}
	}
}

func TestRedirectionIsCaught(t *testing.T) {
	for _, cmd := range []string{
		`echo hello > src/api/users.go`,
		`cat template >> src/api/users.go`,
		`printf 'x' >src/api/users.go`,
	} {
		got := Scan(cmd)
		if got.Confidence != Likely {
			t.Errorf("Scan(%q).Confidence = %v, want Likely", cmd, got.Confidence)
		}
		if !slices.Contains(got.Paths, "src/api/users.go") {
			t.Errorf("Scan(%q).Paths = %v, want src/api/users.go", cmd, got.Paths)
		}
	}
}

// A command hiding behind a chain must not be missed because the chain starts
// with something harmless.
func TestWriterAfterAChainIsStillFound(t *testing.T) {
	for _, cmd := range []string{
		`ls && sed -i '' 's/a/b/' src/api/users.go`,
		`cd src ; tee ../src/api/users.go`,
		`grep -q x file || cp new.go src/api/users.go`,
	} {
		got := Scan(cmd)
		if got.Confidence == None {
			t.Errorf("Scan(%q) found nothing, want a write", cmd)
		}
		if !slices.Contains(got.Paths, "src/api/users.go") {
			t.Errorf("Scan(%q).Paths = %v, missing src/api/users.go", cmd, got.Paths)
		}
	}
}

// Reading is not writing. Treating every command as a write would block normal
// work and the tool would be turned off.
func TestReadOnlyCommandsAreNotFlaggedAsWrites(t *testing.T) {
	for _, cmd := range []string{
		`ls -la src/api`,
		`cat src/api/users.go`,
		`grep -rn "func" src/api/users.go`,
		`git status`,
		`go test ./...`,
		`wc -l src/api/users.go`,
		`sed -n '1,10p' src/api/users.go < input`,
	} {
		if got := Scan(cmd); got.Confidence == Likely {
			t.Errorf("Scan(%q) = %v paths=%v, want no write detected",
				cmd, got.Confidence, got.Paths)
		}
	}
}

// When the target cannot be resolved, saying so beats guessing. The caller knows
// something is written and does not know what, which is a decision it can make.
func TestUnresolvableTargetsReportOpaque(t *testing.T) {
	for _, cmd := range []string{
		`sed -i '' 's/a/b/' "$TARGET"`,
		`echo x > $(cat which_file)`,
		"tee `find . -name '*.go'`",
		`find . -name '*.go' | xargs sed -i '' 's/a/b/'`,
		`eval "$CMD"`,
	} {
		got := Scan(cmd)
		if got.Confidence != Opaque {
			t.Errorf("Scan(%q).Confidence = %v, want Opaque", cmd, got.Confidence)
		}
		if got.Reason == "" {
			t.Errorf("Scan(%q) gave no reason, and the agent needs to be told why", cmd)
		}
	}
}

func TestSeveralPathsInOneCommand(t *testing.T) {
	got := Scan(`cp src/api/users.go src/api/backup.go`)

	if got.Confidence != Likely {
		t.Fatalf("Confidence = %v, want Likely", got.Confidence)
	}
	for _, want := range []string{"src/api/users.go", "src/api/backup.go"} {
		if !slices.Contains(got.Paths, want) {
			t.Errorf("Paths = %v, missing %v", got.Paths, want)
		}
	}
}

func TestFlagsAreNotPaths(t *testing.T) {
	got := Scan(`rm -rf --preserve-root src/api/users.go`)

	for _, p := range got.Paths {
		if p == "-rf" || p == "--preserve-root" {
			t.Errorf("Paths = %v, contains a flag", got.Paths)
		}
	}
	if !slices.Contains(got.Paths, "src/api/users.go") {
		t.Errorf("Paths = %v, want the real target", got.Paths)
	}
}

// Formatters rewrite files in place and are easy to forget.
func TestFormattersCountAsWriters(t *testing.T) {
	for _, cmd := range []string{
		`gofmt -w src/api/users.go`,
		`prettier --write src/components/Button.tsx`,
		`ruff format src/api/users.go`,
	} {
		if got := Scan(cmd); got.Confidence != Likely {
			t.Errorf("Scan(%q).Confidence = %v, want Likely", cmd, got.Confidence)
		}
	}
}

// A writer invoked by absolute path or through env is the same writer.
func TestWriterFoundThroughAnAbsolutePath(t *testing.T) {
	got := Scan(`/usr/bin/sed -i '' 's/a/b/' src/api/users.go`)
	if got.Confidence != Likely {
		t.Errorf("Confidence = %v, want Likely", got.Confidence)
	}
}

func TestEmptyCommand(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\n"} {
		if got := Scan(cmd); got.Confidence != None {
			t.Errorf("Scan(%q).Confidence = %v, want None", cmd, got.Confidence)
		}
	}
}

// TestRedirectionToADescriptorOrDeviceIsNotAFile.
//
// Found by looking at a real dashboard: two agents were shown holding
// "~/Projects/rounce/&1" and "~/Projects/rounce/dev/null". Both came from
// ordinary commands -- `go build ./... 2>&1` and `rounce init >/dev/null`.
//
// `2>&1` duplicates a descriptor, so there is no file. /dev/null is a
// device every agent writes to constantly, so leasing it would mean every
// agent permanently holds the same thing. Neither is a file two agents can
// collide over, and a lease on one is noise in every view that lists what
// an agent has open.
func TestRedirectionToADescriptorOrDeviceIsNotAFile(t *testing.T) {
	for _, command := range []string{
		"go build ./... 2>&1",
		"go test ./... >/dev/null 2>&1",
		"rounce init >/dev/null",
		"echo hi >/dev/stderr",
	} {
		got := Scan(command)
		for _, p := range got.Paths {
			if strings.HasPrefix(p, "&") || strings.Contains(p, "dev/null") ||
				strings.Contains(p, "dev/stderr") {
				t.Errorf("%q: leased %q, which is not a file", command, p)
			}
		}
	}
}
