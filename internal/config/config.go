// Package config loads the per-repo territory declaration.
//
// The format is JSON rather than YAML to keep the binary dependency-free. A hook
// runs on every tool call, so the shipped artifact should be a single static
// binary with nothing to resolve at startup.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentcoord/internal/territory"
)

// FileName is looked for in the working directory and every parent up to the
// filesystem root, so an agent invoked from a subdirectory still finds it.
const FileName = ".agentcoord.json"

// EnvTeam overrides which team the current session belongs to. Teams span
// vendors, so a second Claude session can join the frontend team without
// reinstalling anything.
const EnvTeam = "AGENTCOORD_TEAM"

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

// Config is the loaded, validated declaration plus where it was found.
type Config struct {
	Root string
	Map  *territory.Map
	file File
}

// ErrNotFound means no declaration exists at or above the starting directory.
// Callers must treat this as "not configured, do nothing" rather than an error
// worth failing a tool call over.
var ErrNotFound = errors.New("no " + FileName + " found")

// Load walks up from start looking for the declaration.
func Load(start string) (*Config, error) {
	path, root, err := find(start)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg := &Config{Root: root, file: f, Map: &territory.Map{Root: root}}
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

// Summary describes the territory map for injection into an agent's context at
// session start. It is prose because the consumer is a language model.
func (c *Config) Summary(team string) string {
	var b strings.Builder
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

func find(start string) (configPath, root string, err error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", start, err)
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", ErrNotFound
		}
		dir = parent
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
