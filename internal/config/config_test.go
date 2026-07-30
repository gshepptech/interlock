package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

const sample = `{
  "teams": {
    "backend":  {"owns": ["src/api/**"], "agents": ["claude"]},
    "frontend": {"owns": ["src/components/**"], "shared": ["src/types/**"], "agents": ["codex"]}
  }
}`

func TestLoadReadsTeamsAndRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, sample)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Root != dir {
		t.Errorf("Root = %q, want %q", cfg.Root, dir)
	}
	if len(cfg.Map.Teams) != 2 {
		t.Fatalf("got %d teams, want 2", len(cfg.Map.Teams))
	}
	// Sorted, so a config error reports the same way every run instead of
	// looking intermittent.
	if cfg.Map.Teams[0].Name != "backend" || cfg.Map.Teams[1].Name != "frontend" {
		t.Errorf("teams = %v, want backend then frontend",
			[]string{cfg.Map.Teams[0].Name, cfg.Map.Teams[1].Name})
	}
}

// An agent invoked from a subdirectory still has to find the declaration, or
// enforcement silently stops working the moment someone cds into src.
func TestLoadWalksUpFromASubdirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, sample)
	deep := filepath.Join(root, "src", "api", "handlers")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg, err := Load(deep)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Root != root {
		t.Errorf("Root = %q, want the directory holding the config, %q", cfg.Root, root)
	}
}

// Callers must be able to tell "not configured" from "broken", because the first
// means do nothing and the second means say something.
func TestMissingConfigIsADistinctError(t *testing.T) {
	_, err := Load(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestBrokenConfigIsNotReportedAsMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"teams": {oh dear`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load succeeded on invalid JSON")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a broken config reported as missing, which would silently disable enforcement")
	}
}

func TestTeamForUsesTheDeclaredDefault(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, sample)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.TeamFor("claude"); got != "backend" {
		t.Errorf("TeamFor(claude) = %q, want backend", got)
	}
	if got := cfg.TeamFor("codex"); got != "frontend" {
		t.Errorf("TeamFor(codex) = %q, want frontend", got)
	}
	// An agent nobody claimed has no team, and must not be guessed into one.
	if got := cfg.TeamFor("cursor"); got != "" {
		t.Errorf("TeamFor(cursor) = %q, want empty", got)
	}
}

// The env var is the only way to put two sessions of the same vendor on
// different teams, which is the whole point of teams spanning vendors.
func TestEnvOverridesTheDeclaredTeam(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, sample)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Setenv(EnvTeam, "frontend")
	if got := cfg.TeamFor("claude"); got != "frontend" {
		t.Errorf("TeamFor(claude) = %q, want the env override frontend", got)
	}
}

// The summary is injected into a model's context, so it has to actually name the
// things the model needs to act on.
func TestSummaryNamesTheTeamAndTheTerritories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, sample)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Summary("backend")
	for _, want := range []string{
		"backend", "src/api/**", "frontend", "src/components/**", "src/types/**",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryWithoutATeamStillDescribesTheMap(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, sample)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Summary("")
	if strings.Contains(got, "You are on") {
		t.Error("claimed a team when the session has none")
	}
	if !strings.Contains(got, "src/api/**") {
		t.Error("dropped the territory map when the session has no team")
	}
}

// An empty declaration is valid and means nothing is enforced. It must not error,
// or dropping in a stub config would break the repo.
func TestEmptyTeamsIsValid(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"teams": {}}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Map.Teams) != 0 {
		t.Errorf("got %d teams, want 0", len(cfg.Map.Teams))
	}
	if got := cfg.TeamFor("claude"); got != "" {
		t.Errorf("TeamFor = %q, want empty", got)
	}
}
