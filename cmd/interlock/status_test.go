package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusShowsTerritoryAndHeldFiles(t *testing.T) {
	dir := repo(t, defaultConfig)
	hook(t, dir, "PreToolUse", "claude",
		write("s1", filepath.Join(dir, "src/types/api.ts")))

	res := cli(t, dir, "status")
	for _, want := range []string{
		"backend", "src/api/**", "src/types/api.ts", "recent", "Expires in",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("status missing %q:\n%s", want, res.stdout)
		}
	}
}
