package runner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateAcceptsWhatAUserCanReasonablyAskFor(t *testing.T) {
	cases := []Options{
		{Runner: Codex},
		{Runner: Claude, Name: "frontend", Model: "claude-opus-5", Team: "web"},
		{Runner: Codex, WorkingDir: "server/api"},
		{Runner: Codex, Environment: []string{"HTTP_PROXY", "MY_API_BASE_2"}},
	}
	for _, options := range cases {
		t.Run(options.Label(), func(t *testing.T) {
			if err := options.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

func TestValidateRefusesWhatCannotBeHonoured(t *testing.T) {
	cases := []struct {
		name    string
		options Options
	}{
		{"no runner", Options{}},
		{"unknown runner", Options{Runner: "gemini"}},
		{"name with a space", Options{Runner: Codex, Name: "the api"}},
		{"name too long", Options{Runner: Codex, Name: strings.Repeat("a", MaxNameLen+1)}},
		{"model with a slash", Options{Runner: Codex, Model: "anthropic/claude"}},
		{"absolute working directory", Options{Runner: Codex, WorkingDir: "/etc"}},
		{"climbing working directory", Options{Runner: Codex, WorkingDir: "../.."}},
		{"windows separators", Options{Runner: Codex, WorkingDir: `server\api`}},
		{"environment pair", Options{Runner: Codex, Environment: []string{"TOKEN=abc"}}},
		{"lowercase environment name", Options{Runner: Codex, Environment: []string{"token"}}},
		{"environment name starting with a digit", Options{Runner: Codex, Environment: []string{"1PATH"}}},
		{"repeated environment name", Options{Runner: Codex, Environment: []string{"PATH", "PATH"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.options.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, want invalid", err)
			}
		})
	}
}

func TestValidatePrompt(t *testing.T) {
	if err := ValidatePrompt("Implement the invitation API."); err != nil {
		t.Fatalf("Validate of a normal prompt = %v", err)
	}
	for _, prompt := range []string{"", "  \n ", strings.Repeat("a", MaxPromptBytes+1), "bad\x00byte"} {
		if err := ValidatePrompt(prompt); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidatePrompt(%.20q) = %v, want invalid", prompt, err)
		}
	}
}

func TestModeAndLabelFollowTheOptions(t *testing.T) {
	headless := Options{Runner: Codex}
	if headless.Mode() != Headless || headless.Label() != "codex" {
		t.Fatalf("headless = %v / %q", headless.Mode(), headless.Label())
	}
	interactive := Options{Runner: Claude, Name: "review", Interactive: true}
	if interactive.Mode() != Interactive || interactive.Label() != "review" {
		t.Fatalf("interactive = %v / %q", interactive.Mode(), interactive.Label())
	}
	if (Options{}).Dir() != "." || (Options{WorkingDir: "app/"}).Dir() != "app" {
		t.Fatal("Dir must clean the path and default to the repository root")
	}
}

func TestResolveDirKeepsTheAgentInsideTheRepository(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "server", "api")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveDir(root, "server/api")
	if err != nil {
		t.Fatalf("ResolveDir = %v", err)
	}
	if base, _ := filepath.EvalSymlinks(inside); resolved != base {
		t.Fatalf("ResolveDir = %q, want %q", resolved, inside)
	}
	if _, err := ResolveDir(root, ""); err != nil {
		t.Fatalf("ResolveDir of the root = %v", err)
	}
	if _, err := ResolveDir(root, "missing"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ResolveDir of a missing directory = %v, want invalid", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDir(root, "file"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ResolveDir of a file = %v, want invalid", err)
	}
}

// A path with no ".." in it can still leave the repository, and only the
// filesystem knows. This is the case the syntax check cannot catch.
func TestResolveDirRefusesAnEscapeThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDir(root, "escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ResolveDir through a symlink = %v, want invalid", err)
	}
}
