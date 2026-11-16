package runner

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installFakeAgents puts an executable of each supported name on PATH, so the
// contract tests exercise real executable resolution without needing a real
// agent installed.
func installFakeAgents(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range Names() {
		path := filepath.Join(dir, string(name))
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func testEnvironment(t *testing.T, root string) Environment {
	t.Helper()
	values := map[string]string{
		"HOME":            "/home/tester",
		"PATH":            os.Getenv("PATH"),
		"LANG":            "en_GB.UTF-8",
		"HTTP_PROXY":      "http://proxy.internal:3128",
		"AWS_SECRET_KEY":  "not for the agent",
		"GITHUB_TOKEN":    "not for the agent either",
		"XDG_CONFIG_HOME": "/home/tester/.config",
	}
	return Environment{
		Root:   root,
		Lookup: func(name string) (string, bool) { value, ok := values[name]; return value, ok },
		Link:   []string{"INTERLOCK_RUN_ID=run-0123456789abcdef01234567", "INTERLOCK_AGENT=codex"},
	}
}

// Every adapter answers the same questions, so every adapter is asked them in
// the same test. A new agent is added by adding a case here.
func adapters(t *testing.T) []Adapter {
	t.Helper()
	var out []Adapter
	for _, name := range Names() {
		adapter, err := For(name)
		if err != nil {
			t.Fatalf("For(%s) = %v", name, err)
		}
		if adapter.Name() != name {
			t.Fatalf("adapter for %s calls itself %s", name, adapter.Name())
		}
		out = append(out, adapter)
	}
	return out
}

func TestForRefusesAnUnknownRunner(t *testing.T) {
	if _, err := For("gemini"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("For(gemini) = %v, want invalid", err)
	}
}

func TestAdaptersBuildAnExactCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	installFakeAgents(t)
	env := testEnvironment(t, root)
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}

	for _, adapter := range adapters(t) {
		t.Run(string(adapter.Name()), func(t *testing.T) {
			options := Options{
				Runner: adapter.Name(), Name: "api", Model: "a-model", Team: "backend",
				WorkingDir: "server", Environment: []string{"HTTP_PROXY"},
			}
			launch, err := adapter.Build(options, env)
			if err != nil {
				t.Fatalf("Build = %v", err)
			}
			if filepath.Base(launch.Path) != string(adapter.Name()) {
				t.Errorf("Path = %q", launch.Path)
			}
			if launch.Dir != resolvedRoot {
				t.Errorf("Dir = %q, want %q", launch.Dir, resolvedRoot)
			}
			if launch.Mode != Headless {
				t.Errorf("Mode = %q, want headless", launch.Mode)
			}
			if !slices.Contains(launch.Args, "a-model") {
				t.Errorf("args %q do not select the model", launch.Args)
			}

			// The prompt never becomes an argument. It reaches the agent on
			// stdin, so it cannot be read out of a process listing.
			for _, arg := range launch.Args {
				if strings.Contains(arg, "prompt") || len(arg) > 128 {
					t.Errorf("argument %q looks like it carries a prompt", arg)
				}
			}
			// Nor does any of it become a shell string.
			for _, arg := range launch.Args {
				if strings.ContainsAny(arg, "|;&$`") {
					t.Errorf("argument %q contains shell syntax", arg)
				}
			}

			want := map[string]string{
				"HOME": "/home/tester", "LANG": "en_GB.UTF-8",
				"XDG_CONFIG_HOME":  "/home/tester/.config",
				"HTTP_PROXY":       "http://proxy.internal:3128",
				"INTERLOCK_RUN_ID": "run-0123456789abcdef01234567",
			}
			got := map[string]string{}
			for _, pair := range launch.Env {
				name, value, _ := strings.Cut(pair, "=")
				if _, repeated := got[name]; repeated {
					t.Errorf("environment names %s twice", name)
				}
				got[name] = value
			}
			for name, value := range want {
				if got[name] != value {
					t.Errorf("env %s = %q, want %q", name, got[name], value)
				}
			}
			// The launching environment held two credentials that were not
			// asked for. An agent is exactly the process that would read them.
			for _, name := range []string{"AWS_SECRET_KEY", "GITHUB_TOKEN"} {
				if _, leaked := got[name]; leaked {
					t.Errorf("%s reached the agent", name)
				}
			}
		})
	}
}

func TestAdaptersRefuseWhatTheyCannotDo(t *testing.T) {
	root := t.TempDir()
	installFakeAgents(t)
	env := testEnvironment(t, root)

	for _, adapter := range adapters(t) {
		t.Run(string(adapter.Name()), func(t *testing.T) {
			caps := adapter.Capabilities()
			if !caps.Headless || caps.Interactive {
				t.Fatalf("capabilities = %#v; this release is headless only", caps)
			}
			interactive := Options{Runner: adapter.Name(), Interactive: true}
			if _, err := adapter.Build(interactive, env); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("interactive Build = %v, want unsupported", err)
			}
			mismatched := Options{Runner: otherRunner(adapter.Name())}
			if _, err := adapter.Build(mismatched, env); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Build of another runner's options = %v, want invalid", err)
			}
			escaping := Options{Runner: adapter.Name(), WorkingDir: "../.."}
			if _, err := adapter.Build(escaping, env); !errors.Is(err, ErrInvalid) {
				t.Fatalf("escaping Build = %v, want invalid", err)
			}
		})
	}
}

func bundleInstalled() bool {
	for _, path := range CodexSearchPaths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func otherRunner(name Name) Name {
	if name == Codex {
		return Claude
	}
	return Codex
}

func TestAdaptersReportAMissingAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	for _, adapter := range adapters(t) {
		t.Run(string(adapter.Name()), func(t *testing.T) {
			// The bundled application paths are real paths on a real machine,
			// and a developer with Codex installed has one of them. The fallback
			// working is the point of it, so this case only makes sense on a
			// machine without the bundle.
			if bundleInstalled() {
				t.Skip("an agent bundle is installed at one of the fallback paths")
			}
			env := Environment{Root: root, Lookup: func(string) (string, bool) { return "", false }}
			_, err := adapter.Build(Options{Runner: adapter.Name()}, env)
			if !errors.Is(err, ErrNotInstalled) {
				t.Fatalf("Build without the agent installed = %v, want not installed", err)
			}
			if !strings.Contains(err.Error(), "INTERLOCK_") {
				t.Errorf("error %q does not say how to point Interlock at the binary", err)
			}
		})
	}
}

// Someone who names a specific binary wants to know when it is missing, not to
// silently get a different one from PATH.
func TestExplicitBinaryOverrideIsHonouredAndChecked(t *testing.T) {
	root := t.TempDir()
	installFakeAgents(t)
	elsewhere := filepath.Join(t.TempDir(), "codex-build")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, _ := For(Codex)

	env := testEnvironment(t, root)
	base := env.Lookup
	env.Lookup = func(name string) (string, bool) {
		if name == "INTERLOCK_CODEX_BIN" {
			return elsewhere, true
		}
		return base(name)
	}
	launch, err := adapter.Build(Options{Runner: Codex}, env)
	if err != nil || launch.Path != elsewhere {
		t.Fatalf("Build with an override = %q err=%v", launch.Path, err)
	}

	env.Lookup = func(name string) (string, bool) {
		if name == "INTERLOCK_CODEX_BIN" {
			return filepath.Join(root, "nothing-here"), true
		}
		return base(name)
	}
	if _, err := adapter.Build(Options{Runner: Codex}, env); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Build with a broken override = %v, want not installed", err)
	}
}

func TestDecodeClassifiesOutput(t *testing.T) {
	cases := []struct {
		name   string
		runner Name
		stream Stream
		line   string
		want   EventKind
	}{
		{"codex activity", Codex, Stdout, `{"type":"item.completed","item":{"type":"command"}}`, Activity},
		{"codex result", Codex, Stdout, `{"type":"turn.completed"}`, Result},
		{"codex failure", Codex, Stdout, `{"type":"turn.failed","message":"model refused"}`, Failure},
		{"claude activity", Claude, Stdout, `{"type":"assistant"}`, Activity},
		{"claude result", Claude, Stdout, `{"type":"result","subtype":"success"}`, Result},
		{"claude failed result", Claude, Stdout, `{"type":"result","is_error":true}`, Failure},
		{"plain text", Codex, Stdout, "starting up", Output},
		{"broken json", Claude, Stdout, `{"type":`, Output},
		{"json without a type", Codex, Stdout, `{"other":1}`, Output},
		{"stderr is never structure", Claude, Stderr, `{"type":"result"}`, Output},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := For(tc.runner)
			if err != nil {
				t.Fatal(err)
			}
			event := adapter.Decode(tc.stream, []byte(tc.line))
			if event.Kind != tc.want {
				t.Fatalf("Decode = %q, want %q", event.Kind, tc.want)
			}
			if event.Summary == "" {
				t.Error("every decoded line needs something to display")
			}
		})
	}
}

// A malformed or enormous line must not be able to stop a run, and must not
// reach a display carrying escape sequences.
func TestDecodeIsAlwaysSafeAndNeverFails(t *testing.T) {
	adapter, _ := For(Codex)
	huge := `{"type":"item.completed","text":"` + strings.Repeat("a", maxDecodedLine) + `"}`
	if event := adapter.Decode(Stdout, []byte(huge)); event.Kind != Output {
		t.Fatalf("an oversized line decoded as %q", event.Kind)
	}
	if event := adapter.Decode(Stdout, nil); event.Kind != Output {
		t.Fatalf("an empty line decoded as %q", event.Kind)
	}
	event := adapter.Decode(Stdout, []byte("\x1b[31mred\x1b[0m\x07 text"))
	if event.Summary != "red text" {
		t.Fatalf("Summary = %q; escape sequences must not survive", event.Summary)
	}
	long := adapter.Decode(Stdout, []byte(strings.Repeat("b", MaxEventSummary*2)))
	if len(long.Summary) > MaxEventSummary {
		t.Fatalf("summary is %d bytes, want at most %d", len(long.Summary), MaxEventSummary)
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[1;31mbold red\x1b[0m", "bold red"},
		{"\x1b]0;a window title\x07after", "after"},
		{"\x1b]0;title\x1b\\after", "after"},
		{"trailing \x1b", "trailing "},
		{"\x1bc unknown", " unknown"},
	}
	for _, tc := range cases {
		if got := StripANSI(tc.in); got != tc.want {
			t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
