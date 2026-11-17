package install

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CodexTrustEntry is the persisted trust state Codex needs before it will run a
// project command hook. Codex keys this by hook source, event and positional
// index, and stores the hash of a normalized hook identity.
type CodexTrustEntry struct {
	Key  string
	Hash string
}

// CodexTrustChange reports what was added to ~/.codex/config.toml.
type CodexTrustChange struct {
	File           string
	ProjectTrusted bool
	HooksTrusted   []string
	HooksUpdated   []string
	HooksRemoved   []string
}

// Changed reports whether the Codex config file needs a write.
func (c CodexTrustChange) Changed() bool {
	return c.ProjectTrusted || len(c.HooksTrusted) > 0 ||
		len(c.HooksUpdated) > 0 || len(c.HooksRemoved) > 0
}

// CodexTrustEntries reads the Codex hook file and returns the exact trust
// entries Codex expects for this tool's command hooks. Reading the file rather
// than rebuilding it from the current executable path matters because init is
// idempotent even after the binary moves.
func CodexTrustEntries(t Target) ([]CodexTrustEntry, error) {
	if t.Agent != "codex" {
		return nil, nil
	}
	doc, existed, err := read(t.File)
	if err != nil || !existed {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, nil
	}

	var out []CodexTrustEntry
	for event := range t.Events {
		groups, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for groupIndex, groupAny := range groups {
			owned, exact, _ := ownedGroup(groupAny, event, t.Agent)
			if !owned || !exact {
				continue
			}
			group, ok := groupAny.(map[string]any)
			if !ok {
				continue
			}
			matcher, hasMatcher := group["matcher"].(string)
			inner, ok := group["hooks"].([]any)
			if !ok {
				continue
			}
			for handlerIndex, hookAny := range inner {
				hook, ok := hookAny.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := hook["type"].(string)
				if typ != "command" {
					continue
				}
				entry := CodexTrustEntry{
					Key: fmt.Sprintf("%s:%s:%d:%d", t.File, codexEventLabel(event),
						groupIndex, handlerIndex),
					Hash: codexHookHash(event, matcher, hasMatcher, hook),
				}
				out = append(out, entry)
			}
		}
	}
	slices.SortFunc(out, func(a, b CodexTrustEntry) int {
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

// TrustCodexConfig makes Codex's own config acknowledge both this project and
// the exact command hooks we just installed. Without the hook-state entries
// Codex Desktop can load the hook file but skip every command pending review.
func TrustCodexConfig(file, root string, entries []CodexTrustEntry, dryRun bool) (CodexTrustChange, error) {
	change := CodexTrustChange{File: file}
	original, err := readSnapshot(file)
	if err != nil {
		return change, err
	}
	doc, err := decodeTOML(original.raw)
	if err != nil {
		return change, fmt.Errorf("%s will not be modified: %w", file, err)
	}

	var settings []tomlSetting
	projectPath := []string{"projects", root, "trust_level"}
	if value, ok := lookupTOML(doc, projectPath); !ok {
		change.ProjectTrusted = true
		settings = append(settings, tomlSetting{table: projectPath[:2], key: projectPath[2], value: "trusted"})
	} else if got, ok := value.(string); !ok {
		return change, fmt.Errorf("%s trust value at %s is not a string", file, strings.Join(projectPath, "."))
	} else if got != "trusted" {
		change.ProjectTrusted = true
		settings = append(settings, tomlSetting{table: projectPath[:2], key: projectPath[2], value: "trusted"})
	}
	for _, entry := range entries {
		path := []string{"hooks", "state", entry.Key, "trusted_hash"}
		value, ok := lookupTOML(doc, path)
		switch {
		case !ok:
			change.HooksTrusted = append(change.HooksTrusted, entry.Key)
			settings = append(settings, tomlSetting{table: path[:3], key: path[3], value: entry.Hash})
		case value == entry.Hash:
			continue
		case value != nil:
			if _, ok := value.(string); !ok {
				return change, fmt.Errorf("%s trust value at %s is not a string", file, strings.Join(path, "."))
			}
			change.HooksUpdated = append(change.HooksUpdated, entry.Key)
			settings = append(settings, tomlSetting{table: path[:3], key: path[3], value: entry.Hash})
		}
	}

	if !change.Changed() || dryRun {
		return change, nil
	}
	if original.existed {
		if _, err := backupOnce(file); err != nil {
			return change, err
		}
	}
	updated, err := updateTOML(original.raw, settings)
	if err != nil {
		return change, err
	}
	if err := writeExpected(file, updated, original); err != nil {
		return change, err
	}
	return change, nil
}

// CodexTrustStatus reports whether config.toml contains the trust state needed
// for project-local Codex hooks to actually run.
func CodexTrustStatus(file, root string, entries []CodexTrustEntry) (bool, string) {
	project := InspectCodexProjectTrust(file, root)
	if project.State != TrustHealthy {
		return false, project.Detail
	}
	hooks := InspectCodexHookTrust(file, entries)
	return hooks.State == TrustHealthy, hooks.Detail
}

// InspectCodexProjectTrust reports a typed project trust state so callers can
// distinguish a safe repair from malformed or unsafe configuration.
func InspectCodexProjectTrust(file, root string) TrustStatus {
	status := inspectTOMLValue(file, []string{"projects", root, "trust_level"}, "trusted")
	switch status.State {
	case TrustHealthy:
		status.Detail = "this repo is trusted, Codex will load its hooks"
	case TrustMissing:
		status.Detail = "this repo is NOT trusted, so Codex skips its project hooks"
	}
	return status
}

// CodexProjectTrustStatus checks only whether Codex will load project-local
// configuration for root.
func CodexProjectTrustStatus(file, root string) (bool, string) {
	status := InspectCodexProjectTrust(file, root)
	return status.State == TrustHealthy, status.Detail
}

// InspectCodexHookTrust reports aggregate trust for every installed command.
func InspectCodexHookTrust(file string, entries []CodexTrustEntry) TrustStatus {
	if len(entries) == 0 {
		return TrustStatus{State: TrustMalformed, Detail: "no Interlock Codex hooks found to trust"}
	}
	for _, entry := range entries {
		status := inspectTOMLValue(file,
			[]string{"hooks", "state", entry.Key, "trusted_hash"}, entry.Hash)
		if status.State != TrustHealthy {
			status.Detail = fmt.Sprintf("Codex hook trust for %s: %s", entry.Key, status.Detail)
			return status
		}
	}
	return TrustStatus{State: TrustHealthy,
		Detail: fmt.Sprintf("%d Interlock hook commands trusted", len(entries))}
}

// CodexHookTrustStatus checks only the command-hook trust entries. It is kept
// separate so doctor can name repo trust and hook trust independently.
func CodexHookTrustStatus(file string, entries []CodexTrustEntry) (bool, string) {
	status := InspectCodexHookTrust(file, entries)
	return status.State == TrustHealthy, status.Detail
}

func codexEventLabel(event string) string {
	var b strings.Builder
	for i, r := range event {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

func codexHookHash(event, matcher string, hasMatcher bool, hook map[string]any) string {
	command, _ := hook["command"].(string)
	timeout := numberAsInt(hook["timeout"], 600)
	async, _ := hook["async"].(bool)

	handler := map[string]any{
		"async":   async,
		"command": command,
		"timeout": timeout,
		"type":    "command",
	}
	if status, ok := hook["statusMessage"].(string); ok {
		handler["statusMessage"] = status
	}
	if limit, ok := optionalInt(hook["additionalContextLimit"]); ok && limit != 2500 {
		handler["additionalContextLimit"] = limit
	}
	identity := map[string]any{
		"event_name": codexEventLabel(event),
		"hooks":      []any{handler},
	}
	if hasMatcher {
		identity["matcher"] = matcher
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return "sha256:" + fmt.Sprintf("%x", sum[:])
}

func numberAsInt(v any, fallback int) int {
	if n, ok := optionalInt(v); ok {
		return n
	}
	return fallback
}

func optionalInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		i, err := strconv.Atoi(string(n))
		return i, err == nil
	default:
		return 0, false
	}
}

// UntrustCodexConfig removes the hook trust this tool added for a repository.
//
// Without this, every repository Interlock was ever installed into, including
// throwaway ones, would keep an entry in the user's global Codex config forever.
//
// Project trust is deliberately left alone. Interlock may have set it, but the
// user may equally have trusted that repository for their own reasons, and
// silently withdrawing it would change how Codex behaves in a repository that
// has nothing to do with us any more.
func UntrustCodexConfig(file string, entries []CodexTrustEntry, dryRun bool) (CodexTrustChange, error) {
	change := CodexTrustChange{File: file}
	original, err := readSnapshot(file)
	if err != nil || !original.existed {
		return change, err
	}
	doc, err := decodeTOML(original.raw)
	if err != nil {
		return change, fmt.Errorf("%s will not be modified: %w", file, err)
	}

	var tables [][]string
	for _, entry := range entries {
		table := []string{"hooks", "state", entry.Key}
		if _, ok := lookupTOML(doc, append(slices.Clone(table), "trusted_hash")); !ok {
			continue
		}
		tables = append(tables, table)
		change.HooksRemoved = append(change.HooksRemoved, entry.Key)
	}
	if len(tables) == 0 || dryRun {
		return change, nil
	}

	if _, err := backupOnce(file); err != nil {
		return change, err
	}
	updated, _, err := removeTOMLTables(original.raw, tables)
	if err != nil {
		return change, err
	}
	return change, writeExpected(file, updated, original)
}
