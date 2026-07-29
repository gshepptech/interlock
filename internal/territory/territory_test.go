package territory

import "testing"

// The example from the design: a Claude team on the backend, a Codex team on the
// frontend, and the contract files both of them legitimately need.
func testMap() *Map {
	return &Map{
		Root: "/repo",
		Teams: []Team{
			{
				Name: "backend",
				Owns: []string{"src/api/**", "src/services/**", "migrations/**"},
			},
			{
				Name:   "frontend",
				Owns:   []string{"src/components/**", "src/pages/**", "styles/**"},
				Shared: []string{"src/types/**", "openapi.yaml"},
			},
		},
	}
}

func TestOwnTerritoryIsAllowedAndOthersAreDenied(t *testing.T) {
	m := testMap()

	for _, tc := range []struct {
		team string
		path string
		want Decision
	}{
		{"backend", "/repo/src/api/users.go", Allowed},
		{"backend", "/repo/migrations/001_init.sql", Allowed},
		{"backend", "/repo/src/components/Button.tsx", Denied},
		{"frontend", "/repo/src/components/Button.tsx", Allowed},
		{"frontend", "/repo/src/api/users.go", Denied},
		{"frontend", "/repo/styles/main.css", Allowed},
	} {
		got, _, err := m.Check(tc.team, tc.path)
		if err != nil {
			t.Fatalf("Check(%q, %q): %v", tc.team, tc.path, err)
		}
		if got != tc.want {
			t.Errorf("Check(%q, %q) = %v, want %v", tc.team, tc.path, got, tc.want)
		}
	}
}

// Shared paths are where the lease layer earns its keep, so they must not be
// reported as plain Allowed or the caller will skip the lease check.
func TestSharedPathsAreNeitherAllowedNorDenied(t *testing.T) {
	m := testMap()

	for _, team := range []string{"backend", "frontend"} {
		for _, path := range []string{"/repo/src/types/api.ts", "/repo/openapi.yaml"} {
			got, _, err := m.Check(team, path)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != Shared {
				t.Errorf("Check(%q, %q) = %v, want Shared", team, path, got)
			}
		}
	}
}

// A path nobody claimed must not be denied. Installing this tool on a repo with
// a partial territory map has to leave the repo working.
func TestUnclaimedPathIsUnownedNotDenied(t *testing.T) {
	m := testMap()

	got, _, err := m.Check("backend", "/repo/README.md")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != Unowned {
		t.Errorf("got %v, want Unowned", got)
	}
}

// An agent that declared no team is not yet participating. Denying it would
// break every session that has not been configured.
func TestAgentWithNoTeamIsUnowned(t *testing.T) {
	m := testMap()

	got, _, err := m.Check("", "/repo/src/api/users.go")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != Unowned {
		t.Errorf("got %v, want Unowned", got)
	}
}

// Two teams claiming one path is a config bug. Silently letting the first match
// win would hide it until someone lost work.
func TestOverlappingClaimsAreReportedAsAnError(t *testing.T) {
	m := &Map{
		Root: "/repo",
		Teams: []Team{
			{Name: "a", Owns: []string{"src/**"}},
			{Name: "b", Owns: []string{"src/api/**"}},
		},
	}

	got, _, err := m.Check("a", "/repo/src/api/users.go")
	if err == nil {
		t.Fatal("Check succeeded, want an error naming both claimants")
	}
	if got != Denied {
		t.Errorf("got %v, want Denied on a conflicting config", got)
	}
}

// Writing "src/api" and finding it does not cover the files inside is the kind
// of surprise that gets a tool uninstalled.
func TestBareDirectoryPatternCoversItsContents(t *testing.T) {
	m := &Map{
		Root:  "/repo",
		Teams: []Team{{Name: "backend", Owns: []string{"src/api"}}},
	}

	for _, path := range []string{
		"/repo/src/api",
		"/repo/src/api/users.go",
		"/repo/src/api/nested/deep.go",
	} {
		got, _, err := m.Check("backend", path)
		if err != nil {
			t.Fatalf("Check(%q): %v", path, err)
		}
		if got != Allowed {
			t.Errorf("Check(%q) = %v, want Allowed", path, got)
		}
	}

	// It must not leak into a sibling with a shared prefix.
	got, _, err := m.Check("backend", "/repo/src/apiv2/users.go")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != Unowned {
		t.Errorf("src/apiv2 = %v, want Unowned: src/api must not match it", got)
	}
}

func TestDoubleStarCrossesDirectoriesAndSingleStarDoesNot(t *testing.T) {
	m := &Map{
		Root: "/repo",
		Teams: []Team{
			{Name: "deep", Owns: []string{"a/**/z.go"}},
			{Name: "flat", Owns: []string{"b/*.go"}},
		},
	}

	for _, tc := range []struct {
		team, path string
		want       Decision
	}{
		{"deep", "/repo/a/z.go", Allowed},
		{"deep", "/repo/a/b/c/z.go", Allowed},
		{"flat", "/repo/b/main.go", Allowed},
		// A single * must not cross a separator.
		{"flat", "/repo/b/sub/main.go", Unowned},
	} {
		got, _, err := m.Check(tc.team, tc.path)
		if err != nil {
			t.Fatalf("Check(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("Check(%q, %q) = %v, want %v", tc.team, tc.path, got, tc.want)
		}
	}
}

// Territory is per-repo. Enforcing outside the root would mean this tool taking
// a view on the user's whole filesystem.
func TestPathOutsideRootIsRefusedNotJudged(t *testing.T) {
	m := testMap()

	got, _, err := m.Check("backend", "/etc/passwd")
	if err == nil {
		t.Fatal("Check succeeded on a path outside the root, want an error")
	}
	if got != Unowned {
		t.Errorf("got %v, want Unowned for a path we will not judge", got)
	}
}

func TestOwnerNamesTheClaimingTeam(t *testing.T) {
	m := testMap()

	if got := m.Owner("/repo/src/api/users.go"); got != "backend" {
		t.Errorf("Owner = %q, want backend", got)
	}
	if got := m.Owner("/repo/README.md"); got != "" {
		t.Errorf("Owner = %q, want empty for an unclaimed path", got)
	}
}
