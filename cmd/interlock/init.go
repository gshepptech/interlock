package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/diagnostic"
	"github.com/gshepptech/interlock/internal/install"
	"github.com/gshepptech/interlock/internal/instructions"
)

const starterConfig = `{
  "teams": {
    "backend": {
      "owns": ["src/api/**", "src/services/**", "migrations/**"],
      "shared": ["src/types/**", "openapi.yaml"],
      "agents": ["claude"]
    },
    "frontend": {
      "owns": ["src/components/**", "src/pages/**", "styles/**"],
      "agents": ["codex"]
    }
  }
}
`

// runInit wires the hooks into each agent's settings, merging rather than
// replacing, and reports exactly which files it changed. Nothing is silent:
// these are files the user owns.
func runInit(args []string) int {
	var dryRun, uninstall, withTerritory bool
	for _, a := range args {
		switch a {
		case "--dry-run", "-dry-run", "-n":
			dryRun = true
		case "--uninstall", "-uninstall":
			uninstall = true
		case "--with-territory", "-with-territory":
			withTerritory = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return exitUsage
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine working directory: %v\n", err)
		return 1
	}

	if uninstall {
		return uninstallHooks(cwd, dryRun)
	}

	// No configuration is written. The tool works on observation alone, and a
	// starter file would suggest it has to be filled in before anything happens.
	if withTerritory {
		switch _, err := os.Stat(config.FileName); {
		case err == nil:
			fmt.Printf("%s exists, leaving it alone\n", config.FileName)
		case dryRun:
			fmt.Printf("would write %s\n", config.FileName)
		default:
			if err := os.WriteFile(config.FileName, []byte(starterConfig), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write %s: %v\n", config.FileName, err)
				return 1
			}
			fmt.Printf("wrote %s, edit it so the territories match this repo\n",
				config.FileName)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine my own path: %v\n", err)
		return 1
	}

	codexFile, err := codexConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot locate Codex config: %v\n", err)
		return 1
	}
	repairs, repairErr := (diagnostic.Service{}).Repair(diagnostic.Options{
		Root: cwd, Executable: exe, CodexFile: codexFile,
	}, dryRun)
	for _, repair := range repairs {
		switch {
		case repair.Changed:
			fmt.Printf("%s: %s\n", repair.ID, repair.Detail)
		default:
			fmt.Printf("%s: already current\n", repair.ID)
		}
	}
	if repairErr != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", repairErr)
	}

	fmt.Printf(`
That is it. Nothing to configure: open Claude in one terminal and Codex in
another and they will see each other. Whoever writes a file first has it for a
minute or two; the other is told who is in there and works on something else.

Run interlock doctor to confirm it is enforcing, and interlock dashboard to
watch them work.

If you ever need a hard rule the automatic behaviour cannot work out, such as
one team owning a directory outright, run: interlock init --with-territory
`)
	if repairErr != nil {
		return 1
	}
	return 0
}

func uninstallHooks(root string, dryRun bool) int {
	failed := false
	// Read the trust entries before the hooks file is removed, because they are
	// derived from it and cannot be identified afterwards.
	var codexTrust []install.CodexTrustEntry
	for _, target := range install.Plan(root) {
		if target.Agent != "codex" {
			continue
		}
		entries, err := install.CodexTrustEntries(target)
		if err == nil {
			codexTrust = entries
		}
	}

	for _, target := range install.Plan(root) {
		change, err := install.Remove(target, dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", target.Agent, err)
			failed = true
			continue
		}
		switch {
		case !change.Changed():
			fmt.Printf("%s: nothing of ours to remove\n", target.Agent)
		case dryRun:
			fmt.Printf("%s: would remove %s from %s\n",
				target.Agent, strings.Join(change.Removed, ", "), rel(root, change.File))
		default:
			fmt.Printf("%s: removed %s from %s\n",
				target.Agent, strings.Join(change.Removed, ", "), rel(root, change.File))
		}
		for _, old := range change.Superseded {
			fmt.Printf("%s: also removed %s from %s, left by an older version\n",
				target.Agent, strings.Join(old.Removed, ", "), rel(root, old.File))
		}
	}
	// Withdraw the Codex hook trust this repository was granted. Left behind, it
	// accumulates in the user's global config for every repository Interlock was
	// ever installed into, including ones that no longer exist.
	if len(codexTrust) > 0 {
		if codexFile, err := codexConfigPath(); err != nil {
			fmt.Fprintf(os.Stderr, "codex: cannot locate config to withdraw trust: %v\n", err)
			failed = true
		} else if change, err := install.UntrustCodexConfig(codexFile, codexTrust, dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "codex: %v\n", err)
			failed = true
		} else if len(change.HooksRemoved) > 0 {
			verb := "withdrew"
			if dryRun {
				verb = "would withdraw"
			}
			fmt.Printf("codex: %s hook trust for %d hook(s) from %s\n",
				verb, len(change.HooksRemoved), change.File)
		}
	}

	if change, err := instructions.Remove(root, dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "instructions: %v\n", err)
		failed = true
	} else if change.Removed {
		verb := "removed"
		if dryRun {
			verb = "would remove"
		}
		fmt.Printf("instructions: %s the coordination section from %s\n",
			verb, instructions.FileName)
	}

	fmt.Printf("\n%s and .interlock/ are left in place. Delete them if you are done.\n",
		config.FileName)
	if failed {
		return 1
	}
	return 0
}
