// Package install wires the hooks into each agent's own settings.
//
// These are files the user owns and may have edited by hand, so three properties
// matter more than convenience:
//
//   - Merge, never replace. Everything already in the file survives.
//   - Idempotent. Running init twice does not install the hooks twice.
//   - Atomic. A crash mid-write cannot leave an agent with unparseable settings,
//     which would stop it starting at all.
//
// A backup is written the first time a file is modified, because the failure this
// cannot rule out is a merge that is valid JSON and still wrong.
package install

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const managedMarker = "# interlock-managed:v1"

// claudeWriteTools is every Claude Code tool that can put bytes on disk.
//
// Bash belongs here as much as Write does. An agent that shells out to sed, or
// to a python one-liner, changes the tree exactly as much as one that calls the
// edit tool, and a guard that watches only the polite route is not a guard.
const claudeWriteTools = "Write|Edit|MultiEdit|NotebookEdit|Bash"

// Target is one agent's settings file and the hooks that belong in it.
type Target struct {
	Agent string
	File  string
	// Superseded are files this tool used to install into. Nothing is ever
	// written to them, but they are still read, so an install made by an older
	// version can be found and removed rather than left firing invisibly.
	Superseded []string
	// Events maps a hook event to the matcher it needs. An empty matcher means
	// the agent does not support them, or that every tool should fire.
	Events map[string]string
}

// Plan describes what wiring each agent needs.
//
// Claude Code matches hooks by tool name, so PreToolUse has to name Bash as well
// as the edit tools: an agent that shells out to sed would otherwise skip the
// check entirely. Codex fires its hooks for every tool, so it needs no matcher.
//
// Codex exposes no session-end event, which is why it gets none here and why its
// leases are freed by the time to live instead.
func Plan(root string) []Target {
	return []Target{
		{
			Agent: "claude",
			// settings.local.json, not settings.json. Every hook we write names
			// an absolute path to this machine's binary, and settings.json is
			// the shareable file people commit. Committing a hook that points
			// at /home/somebody/go/bin/interlock means everyone who pulls the
			// repo runs a missing command on every tool call. settings.local.json
			// is the file Claude Code provides for exactly this.
			File:       filepath.Join(root, ".claude", "settings.local.json"),
			Superseded: []string{filepath.Join(root, ".claude", "settings.json")},
			Events: map[string]string{
				"SessionStart": "",
				"PreToolUse":   claudeWriteTools,
				// Must match PreToolUse exactly. PreToolUse snapshots the tree
				// before a shell command whose targets it cannot read, and
				// PostToolUse is what compares it afterwards. Leaving Bash out
				// of this one made the whole after-the-fact check silently dead:
				// the snapshot was armed and nothing ever looked at it.
				"PostToolUse": claudeWriteTools,
				"SessionEnd":  "",
			},
		},
		{
			Agent: "codex",
			File:  filepath.Join(root, ".codex", "hooks.json"),
			Events: map[string]string{
				"SessionStart": "",
				"PreToolUse":   "",
				"PostToolUse":  "",
			},
		},
	}
}

// Change reports what happened to one file.
type Change struct {
	// Superseded holds what was cleaned out of files this tool no longer writes
	// to, so a caller can report an old install being removed.
	Superseded []Change
	File       string
	Added      []string
	Updated    []string
	Removed    []string
	Backup     string
}

// Changed reports whether anything was written.
func (c Change) Changed() bool {
	return len(c.Added) > 0 || len(c.Updated) > 0 || len(c.Removed) > 0
}

// EventState is the installation state for one hook event.
type EventState string

const (
	Healthy    EventState = "healthy"
	Missing    EventState = "missing"
	Stale      EventState = "stale"
	Incomplete EventState = "incomplete"
	Ambiguous  EventState = "ambiguous"
	Malformed  EventState = "malformed"
)

// EventStatus is a stable, structured diagnosis for one expected event.
type EventStatus struct {
	Event  string
	State  EventState
	Detail string
}

// Status describes every expected hook for one target.
type Status struct {
	Target Target
	Events []EventStatus
}

// Command is one exact installed hook command safe to execute for smoke
// verification after Inspect reports it healthy.
type Command struct {
	Event   string
	Agent   string
	Command string
}

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

// Apply merges the target's hooks into its settings file. It reports what it
// added; an empty result means everything was already wired.
//
// dryRun computes the same answer without writing, so a user can see the change
// before it happens to a file they own.
func Apply(t Target, exe string, dryRun bool) (Change, error) {
	change := Change{File: t.File}

	doc, original, err := readJSON(t.File)
	if err != nil {
		return change, err
	}
	existed := original.existed

	hooks, err := hooksObject(doc)
	if err != nil {
		return change, err
	}
	inspections := inspectHooks(t, hooks, exe)
	for _, inspected := range inspections {
		event := inspected.status.Event
		matcher := t.Events[event]
		desired := entry(matcher, hookCommand(exe, event, t.Agent))
		switch inspected.status.State {
		case Healthy:
			continue
		case Missing:
			list, _ := hooks[event].([]any)
			hooks[event] = append(list, desired)
			change.Added = append(change.Added, event)
		case Stale:
			list := hooks[event].([]any)
			list[inspected.index] = desired
			hooks[event] = list
			change.Updated = append(change.Updated, event)
		default:
			return change, fmt.Errorf("%s hook %s is %s: %s",
				t.Agent, event, inspected.status.State, inspected.status.Detail)
		}
	}

	sortStrings(change.Added)
	sortStrings(change.Updated)
	if !change.Changed() || dryRun {
		return change, nil
	}

	doc["hooks"] = hooks
	if existed {
		backup, err := backupOnce(t.File)
		if err != nil {
			return change, err
		}
		change.Backup = backup
	}
	if err := writeJSON(t.File, doc, original); err != nil {
		return change, err
	}
	return change, nil
}

// Inspect reports whether every expected hook is installed exactly as this
// binary would write it. It never mutates the target file.
func Inspect(t Target, exe string) (Status, error) {
	doc, existed, err := read(t.File)
	if err != nil {
		return Status{Target: t}, err
	}
	if !existed {
		status := Status{Target: t}
		for _, event := range sortedEvents(t.Events) {
			status.Events = append(status.Events, EventStatus{
				Event: event, State: Missing, Detail: "hook event is not installed",
			})
		}
		return status, nil
	}
	hooks, err := hooksObject(doc)
	if err != nil {
		status := Status{Target: t}
		for _, event := range sortedEvents(t.Events) {
			status.Events = append(status.Events, EventStatus{
				Event: event, State: Malformed, Detail: err.Error(),
			})
		}
		return status, nil
	}
	status := Status{Target: t}
	for _, inspected := range inspectHooks(t, hooks, exe) {
		status.Events = append(status.Events, inspected.status)
	}
	return status, nil
}

// InstalledCommand returns the exact command for one healthy event. It refuses
// stale or ambiguous state so callers never execute a user-controlled lookalike.
func InstalledCommand(t Target, exe, event string) (Command, error) {
	doc, existed, err := read(t.File)
	if err != nil {
		return Command{}, err
	}
	if !existed {
		return Command{}, fmt.Errorf("%s hooks are not installed", t.Agent)
	}
	hooks, err := hooksObject(doc)
	if err != nil {
		return Command{}, err
	}
	inspected := inspectEvent(t, hooks, exe, event)
	if inspected.status.State != Healthy {
		return Command{}, fmt.Errorf("%s hook %s is %s: %s", t.Agent, event,
			inspected.status.State, inspected.status.Detail)
	}
	list := hooks[event].([]any)
	group := list[inspected.index].(map[string]any)
	handlers := group["hooks"].([]any)
	handler := handlers[0].(map[string]any)
	command := handler["command"].(string)
	return Command{Event: event, Agent: t.Agent, Command: command}, nil
}

type eventInspection struct {
	status EventStatus
	index  int
}

func inspectHooks(t Target, hooks map[string]any, exe string) []eventInspection {
	out := make([]eventInspection, 0, len(t.Events))
	for _, event := range sortedEvents(t.Events) {
		out = append(out, inspectEvent(t, hooks, exe, event))
	}
	return out
}

func inspectEvent(t Target, hooks map[string]any, exe, event string) eventInspection {
	value, exists := hooks[event]
	if !exists {
		return eventInspection{status: EventStatus{
			Event: event, State: Missing, Detail: "hook event is not installed",
		}, index: -1}
	}
	list, ok := value.([]any)
	if !ok {
		return eventInspection{status: EventStatus{
			Event: event, State: Malformed, Detail: "hook event is not an array",
		}, index: -1}
	}

	var candidates []int
	incomplete := false
	malformed := false
	for i, item := range list {
		owned, exact, marked := ownedGroup(item, event, t.Agent)
		switch {
		case owned && exact:
			candidates = append(candidates, i)
		case owned:
			incomplete = true
		case marked:
			malformed = true
		}
	}
	if len(candidates) > 1 || (len(candidates) > 0 && incomplete) {
		return eventInspection{status: EventStatus{
			Event: event, State: Ambiguous,
			Detail: "multiple or mixed Interlock hook groups require manual review",
		}, index: -1}
	}
	if incomplete {
		return eventInspection{status: EventStatus{
			Event: event, State: Incomplete,
			Detail: "an Interlock command shares a group with fields or handlers Interlock does not own",
		}, index: -1}
	}
	if len(candidates) == 0 {
		state := Missing
		detail := "hook event is not installed"
		if malformed {
			state = Malformed
			detail = "an Interlock-marked command has an invalid event, agent, or command shape"
		}
		return eventInspection{status: EventStatus{Event: event, State: state, Detail: detail}, index: -1}
	}

	index := candidates[0]
	desired := entry(t.Events[event], hookCommand(exe, event, t.Agent))
	if sameJSON(list[index], desired) {
		return eventInspection{status: EventStatus{
			Event: event, State: Healthy, Detail: "installed command and matcher are current",
		}, index: index}
	}
	return eventInspection{status: EventStatus{
		Event: event, State: Stale, Detail: "installed command or matcher differs from this binary",
	}, index: index}
}

// ownedGroup recognizes only the exact single-command shape Interlock writes.
// marked is true when our current marker appears in a shape we cannot safely
// update, which lets diagnostics distinguish malformed ownership from absence.
func ownedGroup(item any, event, agent string) (owned, exact, marked bool) {
	group, ok := item.(map[string]any)
	if !ok {
		return false, false, containsManagedMarker(item)
	}
	inner, ok := group["hooks"].([]any)
	if !ok {
		return false, false, containsManagedMarker(item)
	}
	for _, handlerAny := range inner {
		handler, ok := handlerAny.(map[string]any)
		if !ok {
			continue
		}
		command, _ := handler["command"].(string)
		parsedExe, parsedEvent, parsedAgent, current, recognized := parseHookCommand(command)
		_ = parsedExe
		if current {
			marked = true
		}
		if recognized && parsedEvent == event && parsedAgent == agent {
			owned = true
		}
	}
	if !owned {
		return false, false, marked
	}
	if len(inner) != 1 {
		return true, false, marked
	}
	for key := range group {
		if key != "hooks" && key != "matcher" {
			return true, false, marked
		}
	}
	handler, ok := inner[0].(map[string]any)
	if !ok || len(handler) != 2 || handler["type"] != "command" {
		return true, false, marked
	}
	if _, ok := group["matcher"]; ok && len(group) != 2 {
		return true, false, marked
	}
	return true, true, marked
}

func parseHookCommand(command string) (exe, event, agent string, current, ok bool) {
	body := strings.TrimSpace(command)
	if strings.HasSuffix(body, managedMarker) {
		current = true
		body = strings.TrimSpace(strings.TrimSuffix(body, managedMarker))
	}
	cut := strings.LastIndex(body, " hook ")
	if cut <= 0 {
		return "", "", "", current, false
	}
	exe, ok = shellUnquote(strings.TrimSpace(body[:cut]))
	if !ok || filepath.Base(exe) != "interlock" {
		return "", "", "", current, false
	}
	parts := strings.Fields(body[cut+len(" hook "):])
	if len(parts) != 2 {
		return "", "", "", current, false
	}
	return exe, parts[0], parts[1], current, true
}

func hookCommand(exe, event, agent string) string {
	return fmt.Sprintf("%s hook %s %s %s", shellQuote(exe), event, agent, managedMarker)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellUnquote(s string) (string, bool) {
	if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2 {
		body := s[1 : len(s)-1]
		body = strings.ReplaceAll(body, `'"'"'`, "'")
		if shellQuote(body) != s {
			return "", false
		}
		return body, true
	}
	if strings.ContainsAny(s, " \t\r\n\"'") || s == "" {
		return "", false
	}
	return s, true
}

func containsManagedMarker(v any) bool {
	raw, err := json.Marshal(v)
	return err == nil && strings.Contains(string(raw), managedMarker)
}

func sameJSON(a, b any) bool {
	aRaw, aErr := json.Marshal(a)
	bRaw, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && string(aRaw) == string(bRaw)
}

func sortedEvents(events map[string]string) []string {
	out := make([]string, 0, len(events))
	for event := range events {
		out = append(out, event)
	}
	slices.Sort(out)
	return out
}

// Remove takes this tool's hooks back out and leaves everything else alone.
// Anything that installs into a user's file owes them an uninstall.
// Remove takes this tool's hooks out of the current file and out of any file it
// used to install into. Skipping the superseded ones would leave an old install
// firing on every tool call with nothing in the current file to explain it.
func Remove(t Target, dryRun bool) (Change, error) {
	change, err := removeFrom(t, t.File, dryRun)
	if err != nil {
		return change, err
	}
	for _, old := range t.Superseded {
		stale, err := removeFrom(t, old, dryRun)
		if err != nil {
			return change, err
		}
		if stale.Changed() {
			change.Superseded = append(change.Superseded, stale)
		}
	}
	return change, nil
}

func removeFrom(t Target, file string, dryRun bool) (Change, error) {
	change := Change{File: file}

	doc, original, err := readJSON(file)
	if err != nil || !original.existed {
		return change, err
	}

	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return change, nil
	}

	for event := range t.Events {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		ownedIndex := -1
		for i, item := range list {
			owned, exact, marked := ownedGroup(item, event, t.Agent)
			if (owned && !exact) || (marked && !owned) {
				return change, fmt.Errorf("%s hook %s has a mixed or malformed Interlock group; refusing uninstall",
					t.Agent, event)
			}
			if owned && exact {
				if ownedIndex >= 0 {
					return change, fmt.Errorf("%s hook %s has multiple Interlock groups; refusing uninstall",
						t.Agent, event)
				}
				ownedIndex = i
			}
		}
		if ownedIndex < 0 {
			continue
		}
		kept := slices.Delete(slices.Clone(list), ownedIndex, ownedIndex+1)
		change.Removed = append(change.Removed, event)
		if len(kept) == 0 {
			// An empty array left behind is litter in someone else's file.
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}

	sortStrings(change.Removed)
	if !change.Changed() || dryRun {
		return change, nil
	}

	if len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return change, writeJSON(file, doc, original)
}

// Installed reports which of the target's events already carry our hook, sorted.
//
// This is what makes a half-installed setup visible. A settings file that cannot
// be parsed is an error rather than an empty list, because "unreadable" and
// "not installed" call for different fixes.
// Superseded reports events still wired up in a file this tool no longer writes
// to. Anything here is an old install that is still firing.
func Superseded(t Target) (map[string][]string, error) {
	found := map[string][]string{}
	for _, old := range t.Superseded {
		wired, err := installedIn(t, old)
		if err != nil {
			return nil, err
		}
		if len(wired) > 0 {
			found[old] = wired
		}
	}
	return found, nil
}

func Installed(t Target) ([]string, error) { return installedIn(t, t.File) }

func installedIn(t Target, file string) ([]string, error) {
	doc, existed, err := read(file)
	if err != nil || !existed {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, nil
	}

	var wired []string
	for event := range t.Events {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			owned, exact, _ := ownedGroup(item, event, t.Agent)
			if owned && exact {
				wired = append(wired, event)
				break
			}
		}
	}
	slices.Sort(wired)
	return wired, nil
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

// entry builds one hook entry in the shape both agents accept. The nesting is
// theirs, not ours: a list of matcher groups, each holding a list of commands.
func entry(matcher, command string) map[string]any {
	e := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
	if matcher != "" {
		e["matcher"] = matcher
	}
	return e
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

// read loads a settings file. A missing file is not an error: it means we are the
// first to write one.
func read(file string) (doc map[string]any, existed bool, err error) {
	doc, snapshot, err := readJSON(file)
	return doc, snapshot.existed, err
}

type fileSnapshot struct {
	raw     []byte
	mode    os.FileMode
	existed bool
}

func readSnapshot(file string) (fileSnapshot, error) {
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{mode: 0o644}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %s: %w", file, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fileSnapshot{}, fmt.Errorf("refusing to modify symlink %s", file)
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("refusing to modify non-regular file %s", file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read %s: %w", file, err)
	}
	return fileSnapshot{raw: raw, mode: info.Mode().Perm(), existed: true}, nil
}

func readJSON(file string) (map[string]any, fileSnapshot, error) {
	snapshot, err := readSnapshot(file)
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	if !snapshot.existed || len(strings.TrimSpace(string(snapshot.raw))) == 0 {
		return map[string]any{}, snapshot, nil
	}
	var doc map[string]any
	raw := snapshot.raw
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Refusing is the only safe move. Overwriting settings we cannot parse
		// would throw away work the user did by hand.
		return nil, snapshot, fmt.Errorf(
			"%s is not valid JSON, so it will not be modified: %w", file, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, snapshot, nil
}

func hooksObject(doc map[string]any) (map[string]any, error) {
	value, exists := doc["hooks"]
	if !exists {
		return map[string]any{}, nil
	}
	hooks, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("hooks setting is not an object")
	}
	return hooks, nil
}

func writeJSON(file string, doc map[string]any, expected fileSnapshot) error {
	enc, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", file, err)
	}
	enc = append(enc, '\n')
	return writeExpected(file, enc, expected)
}

func writeExpected(file string, body []byte, expected fileSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(file), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".interlock-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	mode := expected.mode
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("set temp file mode: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	current, err := readSnapshot(file)
	if err != nil {
		return err
	}
	if current.existed != expected.existed || !slices.Equal(current.raw, expected.raw) {
		return fmt.Errorf("%s changed while Interlock was preparing an update; no changes were written", file)
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install %s: %w", file, err)
	}
	return nil
}

// backupOnce copies the file aside the first time we touch it, and never
// overwrites an existing backup. A second run must not clobber the copy of the
// file as it was before this tool ever ran.
func backupOnce(file string) (string, error) {
	backup := file + ".interlock-backup"
	if info, err := os.Lstat(backup); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing non-regular backup path %s", backup)
		}
		return backup, nil
	}
	snapshot, err := readSnapshot(file)
	if err != nil {
		return "", fmt.Errorf("read %s for backup: %w", file, err)
	}
	f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, snapshot.mode)
	if errors.Is(err, os.ErrExist) {
		return backup, nil
	}
	if err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}
	if _, err := f.Write(snapshot.raw); err != nil {
		f.Close()
		return "", fmt.Errorf("write backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return backup, nil
}

// Gitignore adds a line to the repo's .gitignore if it is not already covered.
// Leases and session state are local runtime state, and a tool that quietly adds
// untracked files to someone's repo without saying so is a nuisance.
func Gitignore(root, line string) (bool, error) {
	file := filepath.Join(root, ".gitignore")

	original, err := readSnapshot(file)
	if err != nil {
		return false, err
	}
	raw := original.raw
	for existing := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(existing) == line {
			return false, nil
		}
	}

	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	if err := writeExpected(file, []byte(body), original); err != nil {
		return false, fmt.Errorf("write .gitignore: %w", err)
	}
	return true, nil
}

func sortStrings(s []string) { slices.Sort(s) }

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

// ForeignHooks lists hook commands in a target's file that this tool did not
// write, newest first in file order.
//
// It exists for one caller: launching an agent with hook trust disabled. That
// flag turns off the agent's own vetting of hook commands, which is the only
// thing standing between a cloned repository and arbitrary execution. Interlock
// can put that protection back by refusing to disable vetting for a file it
// does not fully recognise, which makes the launcher safer than typing the flag
// by hand.
func ForeignHooks(t Target) ([]string, error) {
	doc, existed, err := read(t.File)
	if err != nil || !existed {
		return nil, err
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil, nil
	}

	var foreign []string
	for _, event := range slices.Sorted(maps.Keys(hooks)) {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			if owned, _, _ := ownedGroup(item, event, t.Agent); owned {
				continue
			}
			group, ok := item.(map[string]any)
			if !ok {
				foreign = append(foreign, event+": unreadable hook entry")
				continue
			}
			inner, _ := group["hooks"].([]any)
			for _, h := range inner {
				handler, ok := h.(map[string]any)
				if !ok {
					continue
				}
				command, _ := handler["command"].(string)
				foreign = append(foreign, event+": "+command)
			}
		}
	}
	return foreign, nil
}
