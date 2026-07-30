// Package config loads the per-repo territory declaration.
//
// The format is JSON rather than YAML to keep the binary dependency-free. A hook
// runs on every tool call, so the shipped artifact should be a single static
// binary with nothing to resolve at startup.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"interlock/internal/territory"
)

// FileName is looked for in the working directory and every parent up to the
// filesystem root, so an agent invoked from a subdirectory still finds it.
const FileName = ".interlock.json"

// EnvTeam overrides which team the current session belongs to. Teams span
// vendors, so a second Claude session can join the frontend team without
// reinstalling anything.
const EnvTeam = "INTERLOCK_TEAM"

// File is the on-disk shape.
type File struct {
	Teams map[string]TeamSpec `json:"teams"`
}

// TeamSpec declares what a team owns and which agents default into it.
type TeamSpec struct {
	Owns   []string `json:"owns"`
	Shared []string `json:"shared,omitempty"`
	// Agents that join this team when nothing else says otherwise. Optional:
	// a team can exist with no default agent and be joined only by env var.
	Agents []string `json:"agents,omitempty"`
}

// Config is the territory declaration, if there is one, plus the repo it applies
// to.
//
// A repo with no declaration is the normal case and produces a usable Config
// with no teams. Everything still works: agents see each other, and writing a
// file claims it briefly so a second agent is told rather than colliding.
// Declaring territory is how you add a rule the automatic behaviour cannot
// infer, not how you switch the tool on.
type Config struct {
	Root string
	Map  *territory.Map
	// Declared reports whether a declaration file was found. Without one the
	// tool runs on observation alone.
	Declared bool
	file     File
}

// Load finds the repo for start and any declaration in it.
//
// A missing declaration is not an error. It returns a Config with no teams and
// Declared false, which is the shape the tool is designed around: the common
// case is a repo nobody has configured.
func Load(start string) (*Config, error) {
	path, root, found := find(start)
	if !found {
		repoRoot, err := discoverRoot(start)
		if err != nil {
			return nil, err
		}
		return &Config{Root: repoRoot, Map: &territory.Map{Root: repoRoot}}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg := &Config{Root: root, file: f, Declared: true, Map: &territory.Map{Root: root}}
	// Map iteration order is random, and Check reports the first conflicting
	// claim it meets. Sorting keeps that message stable between runs so a
	// config bug does not look like an intermittent one.
	for _, name := range sortedKeys(f.Teams) {
		spec := f.Teams[name]
		cfg.Map.Teams = append(cfg.Map.Teams, territory.Team{
			Name:   name,
			Owns:   spec.Owns,
			Shared: spec.Shared,
		})
	}
	return cfg, nil
}

// TeamFor resolves which team a session belongs to. The environment wins over
// the declaration, because it is the only way to place two sessions of the same
// vendor on different teams.
func (c *Config) TeamFor(agent string) string {
	if team := os.Getenv(EnvTeam); team != "" {
		return team
	}
	for _, name := range sortedKeys(c.file.Teams) {
		if slices.Contains(c.file.Teams[name].Agents, agent) {
			return name
		}
	}
	return ""
}

// AgentsFor returns the agents that default into a team, for display.
func (c *Config) AgentsFor(team string) []string {
	return c.file.Teams[team].Agents
}

// Summary describes the territory map for injection into an agent's context at
// session start. It is prose because the consumer is a language model.
func (c *Config) Summary(team string) string {
	var b strings.Builder

	if !c.Declared {
		// Nothing was declared, so describe the behaviour rather than a map.
		// The agent needs to know why a write might be refused, and that the
		// refusal is about timing rather than permission.
		b.WriteString("More than one AI coding agent may be working in this " +
			"repository at the same time. Writing a file claims it for a couple " +
			"of minutes; if another agent is already in a file you try to write, " +
			"the write is refused and you are told who has it. That is a " +
			"scheduling conflict, not a permission one: work on something else " +
			"and come back to it, and do not try to route around it.")
		return b.String()
	}

	if team != "" {
		fmt.Fprintf(&b, "You are on the %q team. ", team)
	}
	b.WriteString("This repo has agent territories:\n")
	for _, t := range c.Map.Teams {
		fmt.Fprintf(&b, "  - %s owns: %s\n", t.Name, strings.Join(t.Owns, ", "))
		if len(t.Shared) > 0 {
			fmt.Fprintf(&b, "    shared (any team, one at a time): %s\n",
				strings.Join(t.Shared, ", "))
		}
	}
	b.WriteString("Writes outside your territory are blocked. Other agents may " +
		"be working in this repo at the same time.")
	return b.String()
}

func find(start string) (configPath, root string, found bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", "", false
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// discoverRoot finds the repository when no declaration marks it.
//
// The git root is the right boundary: it is what a person means by "this
// project", and it is the same boundary the after-the-fact checks already use.
// Outside a repo the working directory is the only honest answer, which keeps
// coordination scoped to wherever the agents were actually started.
func discoverRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", start, err)
	}
	for probe := dir; ; {
		if info, err := os.Stat(filepath.Join(probe, ".git")); err == nil && info != nil {
			return probe, nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return dir, nil
		}
		probe = parent
	}
}

func sortedKeys(m map[string]TeamSpec) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
