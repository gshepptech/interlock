// Package diagnostic evaluates and safely repairs an Interlock installation.
package diagnostic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/gitstate"
	"github.com/gshepptech/interlock/internal/handoffrequest"
	"github.com/gshepptech/interlock/internal/install"
	"github.com/gshepptech/interlock/internal/journal"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/remedy"
	"github.com/gshepptech/interlock/internal/session"
	"github.com/gshepptech/interlock/internal/state"
	"github.com/gshepptech/interlock/internal/verification"
)

// Severity describes whether a check passed, is advisory, or prevents part of
// Interlock from enforcing.
type Severity string

const (
	OK   Severity = "ok"
	Warn Severity = "warn"
	Fail Severity = "fail"
)

// Check is one stable diagnostic result suitable for terminal or HTTP display.
type Check struct {
	ID         string
	Subject    string
	Severity   Severity
	Detail     string
	Repairable bool
}

// Options identifies the repository and local installation being checked.
type Options struct {
	Root       string
	Executable string
	CodexFile  string
	Smoke      bool
	// Config pins an already resolved repository boundary for a long-lived
	// caller such as the dashboard. Nil resolves Root for this one operation.
	Config *config.Config
}

// Report is a deterministic list of installation checks.
type Report struct {
	Checks []Check
}

// HasFailures reports whether any part of enforcement is unavailable.
func (r Report) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Severity == Fail {
			return true
		}
	}
	return false
}

// Repair records one attempted safe repair.
type Repair struct {
	ID      string
	Changed bool
	Detail  string
}

// CommandRunner executes one exact inspected hook command.
type CommandRunner func(ctx context.Context, dir, command string, stdin []byte) (
	stdout, stderr []byte, err error)

// Service holds injectable process behavior for deterministic tests.
type Service struct {
	RunCommand CommandRunner
	EndSession func(root, sessionID string) error
}

// Check evaluates the complete local installation without mutating it.
func (s Service) Check(opts Options) (Report, error) {
	if opts.Root == "" {
		return Report{}, errors.New("diagnostic root is required")
	}
	if opts.Executable == "" {
		return Report{}, errors.New("diagnostic executable is required")
	}

	var report Report
	add := func(check Check) { report.Checks = append(report.Checks, check) }

	cfg, err := optionsConfig(opts)
	if err != nil {
		add(Check{ID: "config", Subject: "config", Severity: Fail, Detail: err.Error()})
		return report, nil
	}
	driftErr := pinnedRootDrift(opts, cfg)
	if driftErr != nil {
		add(Check{ID: "config_drift", Subject: "repository boundary", Severity: Fail,
			Detail: driftErr.Error()})
	}
	if cfg.Declared {
		add(Check{
			ID: "territory", Subject: "territory", Severity: OK,
			Detail: fmt.Sprintf("%d teams declared in %s", len(cfg.Map.Teams), config.FileName),
		})
		claimed := 0
		for _, team := range cfg.Map.Teams {
			if len(cfg.AgentsFor(team.Name)) > 0 {
				claimed++
			}
		}
		severity := OK
		if claimed == 0 {
			severity = Fail
		}
		add(Check{
			ID: "team_assignment", Subject: "team assignment", Severity: severity,
			Detail: fmt.Sprintf("%d of %d teams have a default agent", claimed, len(cfg.Map.Teams)),
		})
	} else {
		add(Check{
			ID: "territory", Subject: "territory", Severity: OK,
			Detail: "automatic, agents claim files as they write them",
		})
	}
	add(Check{ID: "repo", Subject: "repo", Severity: OK, Detail: cfg.Root})

	targets := install.Plan(cfg.Root)
	for _, target := range targets {
		status, inspectErr := install.Inspect(target, opts.Executable)
		add(hookCheck(target, status, inspectErr))
	}

	codexTarget := targetFor(targets, "codex")
	if opts.CodexFile == "" {
		add(Check{
			ID: "codex_repo_trust", Subject: "codex repo trust", Severity: Fail,
			Detail: "Codex config path is unavailable", Repairable: false,
		})
		add(Check{
			ID: "codex_hook_trust", Subject: "codex hook trust", Severity: Fail,
			Detail: "Codex config path is unavailable", Repairable: false,
		})
	} else {
		trust := install.InspectCodexProjectTrust(opts.CodexFile, cfg.Root)
		add(Check{
			ID: "codex_repo_trust", Subject: "codex repo trust",
			Severity: severity(trust.State == install.TrustHealthy), Detail: trust.Detail,
			Repairable: trust.Repairable(),
		})
		entries, entriesErr := install.CodexTrustEntries(codexTarget)
		if entriesErr != nil {
			add(Check{
				ID: "codex_hook_trust", Subject: "codex hook trust", Severity: Fail,
				Detail: entriesErr.Error(), Repairable: false,
			})
		} else {
			trust = install.InspectCodexHookTrust(opts.CodexFile, entries)
			add(Check{
				ID: "codex_hook_trust", Subject: "codex hook trust",
				Severity: severity(trust.State == install.TrustHealthy), Detail: trust.Detail,
				Repairable: trust.Repairable(),
			})
		}
	}

	if gitstate.Available(cfg.Root) {
		add(Check{
			ID: "verification", Subject: "after-the-fact checks", Severity: OK,
			Detail: "git available, opaque writes are verified",
		})
	} else {
		add(Check{
			ID: "verification", Subject: "after-the-fact checks", Severity: Fail,
			Detail: "not a git repo, so writes from opaque shell commands cannot be checked",
		})
	}

	if err := lease.Check(cfg.Root); err != nil {
		add(Check{ID: "leases", Subject: "leases", Severity: Fail, Detail: err.Error()})
	} else {
		add(Check{ID: "leases", Subject: "leases", Severity: OK, Detail: "available"})
	}

	issues, inspectErr := inspectState(cfg.Root)
	if inspectErr != nil {
		add(Check{ID: "state_schema", Subject: "state schema", Severity: Fail,
			Detail: inspectErr.Error()})
	} else if len(issues) > 0 {
		detail := issues[0].Error()
		if len(issues) > 1 {
			detail += fmt.Sprintf(" (and %d more)", len(issues)-1)
		}
		add(Check{ID: "state_schema", Subject: "state schema", Severity: Fail,
			Detail: detail})
	} else {
		add(Check{ID: "state_schema", Subject: "state schema", Severity: OK,
			Detail: fmt.Sprintf("version %d; all local records readable", state.CurrentVersion)})
	}

	if opts.Smoke {
		for _, target := range targets {
			if driftErr != nil {
				add(Check{ID: target.Agent + "_smoke", Subject: target.Agent + " smoke",
					Severity: Fail, Detail: "not run: " + driftErr.Error()})
			} else {
				add(s.smokeCheck(opts, cfg.Root, target))
			}
		}
	}
	return report, nil
}

// Repair applies only deterministic installation fixes. It continues after an
// independent target fails and returns partial structured results with a joined
// error.
func (s Service) Repair(opts Options, dryRun bool) ([]Repair, error) {
	if opts.Root == "" || opts.Executable == "" {
		return nil, errors.New("diagnostic root and executable are required")
	}
	cfg, err := optionsConfig(opts)
	if err != nil {
		return nil, err
	}
	if err := pinnedRootDrift(opts, cfg); err != nil {
		return nil, err
	}

	var repairs []Repair
	var errs []error
	targets := install.Plan(cfg.Root)
	for _, target := range targets {
		change, applyErr := install.Apply(target, opts.Executable, dryRun)
		if applyErr != nil {
			errs = append(errs, fmt.Errorf("%s hooks: %w", target.Agent, applyErr))
			repairs = append(repairs, Repair{
				ID: target.Agent + "_hooks", Detail: applyErr.Error(),
			})
			continue
		}
		repairs = append(repairs, Repair{
			ID: target.Agent + "_hooks", Changed: change.Changed(),
			Detail: describeChange(change, dryRun),
		})
	}

	codexTarget := targetFor(targets, "codex")
	entries, entriesErr := install.CodexTrustEntries(codexTarget)
	switch {
	case entriesErr != nil:
		errs = append(errs, fmt.Errorf("codex trust entries: %w", entriesErr))
	case opts.CodexFile == "":
		errs = append(errs, errors.New("Codex config path is unavailable"))
	default:
		change, trustErr := install.TrustCodexConfig(
			opts.CodexFile, cfg.Root, entries, dryRun)
		if trustErr != nil {
			errs = append(errs, fmt.Errorf("codex trust: %w", trustErr))
			repairs = append(repairs, Repair{ID: "codex_trust", Detail: trustErr.Error()})
		} else {
			repairs = append(repairs, Repair{
				ID: "codex_trust", Changed: change.Changed(),
				Detail: describeTrust(change, dryRun),
			})
		}
	}

	if dryRun {
		ignored, ignoreErr := gitignoreStatus(cfg.Root, ".interlock/")
		if ignoreErr != nil {
			errs = append(errs, ignoreErr)
		}
		repairs = append(repairs, Repair{
			ID: "gitignore", Changed: !ignored,
			Detail: choose(ignored, "already ignored", "would add .interlock/"),
		})
	} else {
		added, ignoreErr := install.Gitignore(cfg.Root, ".interlock/")
		if ignoreErr != nil {
			errs = append(errs, fmt.Errorf("gitignore: %w", ignoreErr))
			repairs = append(repairs, Repair{ID: "gitignore", Detail: ignoreErr.Error()})
		} else {
			repairs = append(repairs, Repair{
				ID: "gitignore", Changed: added,
				Detail: choose(added, "added .interlock/", "already ignored"),
			})
		}
	}
	return repairs, errors.Join(errs...)
}

func optionsConfig(opts Options) (*config.Config, error) {
	if opts.Config == nil {
		return config.Load(opts.Root)
	}
	if filepath.Clean(opts.Config.Root) != filepath.Clean(opts.Root) {
		return nil, errors.New("diagnostic config root does not match requested root")
	}
	return opts.Config, nil
}

func pinnedRootDrift(opts Options, pinned *config.Config) error {
	if opts.Config == nil {
		return nil
	}
	current, err := config.Load(opts.Root)
	if err != nil {
		return fmt.Errorf("repository configuration changed; restart the dashboard: %w", err)
	}
	if filepath.Clean(current.Root) != filepath.Clean(pinned.Root) {
		return fmt.Errorf("repository boundary changed from %s to %s; restart the dashboard",
			pinned.Root, current.Root)
	}
	return nil
}

func hookCheck(target install.Target, status install.Status, err error) Check {
	check := Check{ID: target.Agent + "_hooks", Subject: target.Agent + " hooks"}
	if err != nil {
		check.Severity = Fail
		check.Detail = err.Error()
		return check
	}
	var unhealthy []string
	repairable := true
	missing := 0
	for _, event := range status.Events {
		if event.State == install.Healthy {
			continue
		}
		unhealthy = append(unhealthy, fmt.Sprintf("%s=%s", event.Event, event.State))
		if event.State == install.Missing {
			missing++
		}
		if event.State != install.Missing && event.State != install.Stale {
			repairable = false
		}
	}
	if len(unhealthy) == 0 {
		check.Severity = OK
		check.Detail = fmt.Sprintf("%d events installed in %s", len(status.Events), target.File)
		return check
	}
	check.Severity = Fail
	check.Detail = strings.Join(unhealthy, ", ")
	if missing == len(status.Events) {
		check.Detail = "not wired up; " + check.Detail
	}
	check.Repairable = repairable
	return check
}

func (s Service) smokeCheck(opts Options, root string, target install.Target) Check {
	check := Check{
		ID: target.Agent + "_smoke", Subject: target.Agent + " smoke",
		Severity: Fail,
	}
	installed, err := install.InstalledCommand(
		target, opts.Executable, "SessionStart")
	if err != nil {
		check.Detail = "not run: " + err.Error()
		return check
	}
	id, err := smokeID()
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	payload, err := json.Marshal(map[string]any{
		"session_id": id, "cwd": root, "hook_event_name": "SessionStart",
		"source": "interlock-doctor-smoke",
	})
	if err != nil {
		check.Detail = err.Error()
		return check
	}

	store, err := session.Open(root)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run := s.RunCommand
	if run == nil {
		run = runShell
	}
	_, stderr, runErr := run(ctx, root, installed.Command, payload)
	if runErr != nil {
		if ctx.Err() != nil {
			check.Detail = "timed out executing installed hook command"
		} else {
			check.Detail = fmt.Sprintf("installed hook command failed: %v: %s",
				runErr, strings.TrimSpace(string(stderr)))
		}
		return s.finishSmoke(store, root, id, check)
	}
	seen, err := store.Get(id)
	if err != nil {
		check.Detail = "hook command exited without recording the synthetic session"
		return s.finishSmoke(store, root, id, check)
	}
	if seen.Agent != target.Agent {
		check.Detail = fmt.Sprintf("synthetic session recorded agent %q, want %q",
			seen.Agent, target.Agent)
		return s.finishSmoke(store, root, id, check)
	}
	check.Detail = "installed command recorded a synthetic session"
	check.Severity = OK
	check = s.finishSmoke(store, root, id, check)
	if check.Severity == Fail {
		return check
	}
	check.Detail = "installed command recorded and cleaned up a synthetic session"
	return check
}

func (s Service) finishSmoke(store *session.Store, root, id string, check Check) Check {
	end := s.EndSession
	if end == nil {
		end = func(_ string, sessionID string) error { return store.End(sessionID) }
	}
	if err := end(root, id); err != nil {
		check.Severity = Fail
		check.Detail += fmt.Sprintf("; cleanup failed: %v", err)
		return check
	}
	if _, err := store.Get(id); !errors.Is(err, os.ErrNotExist) {
		check.Severity = Fail
		if err != nil {
			check.Detail += fmt.Sprintf("; verify cleanup: %v", err)
		} else {
			check.Detail += "; cleanup left the synthetic session behind"
		}
		return check
	}
	return check
}

const maxSmokeOutput = 8 * 1024

func runShell(ctx context.Context, dir, command string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr limitedBuffer
	stdout.remaining = maxSmokeOutput
	stderr.remaining = maxSmokeOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return stdout.Bytes(), stderr.Bytes(), err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killErr := killProcessGroup(cmd)
		<-done
		err = errors.Join(ctx.Err(), killErr)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedBuffer struct {
	buf       bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.remaining > 0 {
		n := min(len(p), b.remaining)
		_, _ = b.buf.Write(p[:n])
		b.remaining -= n
	}
	return original, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func smokeID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("generate smoke session id: %w", err)
	}
	return "interlock-smoke-" + hex.EncodeToString(raw[:]), nil
}

func severity(ok bool) Severity {
	if ok {
		return OK
	}
	return Fail
}

func targetFor(targets []install.Target, agent string) install.Target {
	for _, target := range targets {
		if target.Agent == agent {
			return target
		}
	}
	return install.Target{}
}

func describeChange(change install.Change, dryRun bool) string {
	var parts []string
	if len(change.Added) > 0 {
		parts = append(parts, "add "+strings.Join(change.Added, ", "))
	}
	if len(change.Updated) > 0 {
		parts = append(parts, "update "+strings.Join(change.Updated, ", "))
	}
	if len(parts) == 0 {
		return "already current"
	}
	prefix := ""
	if dryRun {
		prefix = "would "
	}
	detail := prefix + strings.Join(parts, "; ")
	if change.Backup != "" && !dryRun {
		detail += "; backup " + change.Backup
	}
	return detail
}

func describeTrust(change install.CodexTrustChange, dryRun bool) string {
	if !change.Changed() {
		return "already current"
	}
	count := len(change.HooksTrusted) + len(change.HooksUpdated)
	action := "trusted"
	if dryRun {
		action = "would trust"
	}
	return fmt.Sprintf("%s repo=%t hook_commands=%d",
		action, change.ProjectTrusted, count)
}

func choose(condition bool, yes, no string) string {
	if condition {
		return yes
	}
	return no
}

func gitignoreStatus(root, line string) (bool, error) {
	// Dry-run must not call the mutating installer helper.
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read .gitignore: %w", err)
	}
	for existing := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(existing) == line {
			return true, nil
		}
	}
	return false, nil
}

func inspectState(root string) ([]state.Issue, error) {
	var issues []state.Issue
	sessions, err := session.Open(root)
	if err != nil {
		return nil, err
	}
	leases, err := lease.Open(root)
	if err != nil {
		return nil, err
	}
	remedies, err := remedy.Open(root)
	if err != nil {
		return nil, err
	}
	journalStore, err := journal.Open(root)
	if err != nil {
		return nil, err
	}
	inspectors := []func() ([]state.Issue, error){
		sessions.Inspect,
		leases.Inspect,
		remedies.Inspect,
		journalStore.Inspect,
		handoffrequest.Open(root).Inspect,
		verification.Open(root).Inspect,
	}
	for _, inspect := range inspectors {
		found, err := inspect()
		if err != nil {
			return nil, err
		}
		issues = append(issues, found...)
	}
	return issues, nil
}
