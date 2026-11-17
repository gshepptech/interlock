package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gshepptech/interlock/internal/config"
	"github.com/gshepptech/interlock/internal/lease"
	"github.com/gshepptech/interlock/internal/session"
)

// presence describes the other agents working this repo right now.
//
// This is the half of the tool that needs no rules and no configuration at all.
// An agent that knows a peer is live in src/components and last touched Card.tsx
// a minute ago will usually stay out of the way on its own, without ever being
// refused anything. Blocking is the backstop; this is the mechanism.
//
// Nothing is said when the repo is quiet, because a line of boilerplate on every
// session start trains an agent to skip the whole block.
func presence(cfg *config.Config, ownSession string) string {
	sessions, leases := otherSessions(cfg, ownSession), heldBy(cfg, ownSession)
	if len(sessions) == 0 && len(leases) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nOther agents are working in this repository right now.")

	now := time.Now()
	for _, sess := range sessions {
		fmt.Fprintf(&b, "\n  - %s", describeSession(sess, now))
		if areas := workingAreas(sess); areas != "" {
			fmt.Fprintf(&b, ", working in %s", areas)
		}
		if recent := sess.Recent; len(recent) > 0 {
			fmt.Fprintf(&b, ". Last touched %s %s ago",
				recent[0].Path, now.Sub(recent[0].At).Round(time.Second))
		}
	}
	for _, l := range leases {
		fmt.Fprintf(&b, "\n  - %s is claimed, so writing it will be refused until "+
			"they move on", l.Path)
	}
	b.WriteString("\nStay out of what they are in the middle of. If you need a " +
		"file they hold, do something else first and come back to it.")
	return b.String()
}

func describeSession(s *session.Session, now time.Time) string {
	who := s.Agent
	if who == "" {
		who = "an agent"
	}
	if s.Team != "" {
		who += " on " + s.Team
	}
	return fmt.Sprintf("%s, active %s ago", who,
		now.Sub(s.LastSeen).Round(time.Second))
}

// workingAreas names the directories a session has been writing in, which is a
// better hint than a file list: it tells a peer which part of the repo to avoid
// rather than which exact bytes are locked.
func workingAreas(s *session.Session) string {
	seen := map[string]bool{}
	var dirs []string
	for _, t := range s.Recent {
		dir := filepath.Dir(t.Path)
		if dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
		if len(dirs) == 3 {
			break
		}
	}
	return strings.Join(dirs, ", ")
}

func otherSessions(cfg *config.Config, ownSession string) []*session.Session {
	store, err := session.Open(cfg.Root)
	if err != nil {
		return nil
	}
	active, err := store.Active()
	if err != nil {
		return nil
	}
	var out []*session.Session
	for _, s := range active {
		if s.ID != ownSession {
			out = append(out, s)
		}
	}
	return out
}

func heldBy(cfg *config.Config, ownSession string) []*lease.Lease {
	store, err := lease.Open(cfg.Root)
	if err != nil {
		return nil
	}
	active, err := store.Active()
	if err != nil {
		return nil
	}
	var out []*lease.Lease
	for _, l := range active {
		// Implicit claims churn as agents move between files, and listing all of
		// them would bury the session summary above. The directories a peer is
		// working in are the useful signal; only a deliberately shared file is
		// worth naming as a hard obstacle.
		if l.SessionID != ownSession && !l.Implicit {
			out = append(out, l)
		}
	}
	return out
}
