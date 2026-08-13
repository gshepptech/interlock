package runner

// codexSearchPaths are where the binary has actually lived. Codex moved from
// its own bundle into the ChatGPT app, so a single hard-coded path would have
// broken silently the week it moved.
var codexSearchPaths = []string{
	"/Applications/ChatGPT.app/Contents/Resources/codex",
	"/Applications/Codex.app/Contents/Resources/codex",
}

// codexHookTrustFlag makes Codex run its hooks without the persisted trust it
// no longer offers any way to grant.
//
// It reads like the sort of flag this package refuses to add, and it is worth
// being precise about why it is not. It does not bypass Codex's sandbox or its
// permission prompts; both remain exactly as the user configured them. It
// bypasses the trust check on the hook commands, which are Interlock's own, and
// without it Interlock's guard does not run at all. A managed agent whose
// writes are unguarded is the thing this whole tool exists to prevent.
const codexHookTrustFlag = "--dangerously-bypass-hook-trust"

type codex struct{}

func (codex) Name() Name { return Codex }

func (codex) Capabilities() Capabilities {
	return Capabilities{Headless: true, StructuredLog: true, ModelSelection: true}
}

// Build constructs `codex exec`, Codex's headless mode.
//
// The prompt is the trailing `-`, which tells Codex to read it from stdin. That
// is the whole reason for the argument: a prompt passed as an argument is
// visible in every process listing on the machine for as long as the agent
// runs, and prompts routinely describe work that is not public.
func (c codex) Build(o Options, env Environment) (Launch, error) {
	path, dir, err := build(o, env, c.Capabilities(), string(Codex), codexSearchPaths)
	if err != nil {
		return Launch{}, err
	}
	args := []string{codexHookTrustFlag, "exec", "--json", "--color", "never", "-C", dir}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, "-")
	return Launch{Path: path, Args: args, Dir: dir, Env: resolveEnv(o, env), Mode: o.Mode()}, nil
}

// Decode reads one line of Codex's JSON event stream.
//
// The type names are Codex's, and they will change. That is survivable because
// an unrecognised structured line still counts as the agent doing something,
// and an unparseable one still counts as output. The worst an upstream rename
// can do here is make a run look less specific, never make it look dead.
func (codex) Decode(stream Stream, line []byte) Event {
	if stream == Stderr {
		return Event{Kind: Output, Summary: SafeText(string(line), MaxEventSummary)}
	}
	return decodeLine(line, func(payload map[string]any) (EventKind, string) {
		kind := stringField(payload, "type")
		switch kind {
		case "":
			return Output, ""
		case "error", "turn.failed", "thread.error":
			return Failure, codexDetail(payload, kind)
		case "turn.completed", "thread.completed":
			return Result, codexDetail(payload, kind)
		default:
			return Activity, codexDetail(payload, kind)
		}
	})
}

// codexDetail prefers the human-readable field an event carries, and falls back
// to the event's own name. It never returns raw nested JSON: everything here
// ends up in a status summary or on a page.
func codexDetail(payload map[string]any, kind string) string {
	for _, field := range []string{"message", "text", "reason"} {
		if value := stringField(payload, field); value != "" {
			return value
		}
	}
	if item, ok := payload["item"].(map[string]any); ok {
		if value := stringField(item, "type"); value != "" {
			return kind + ": " + value
		}
	}
	return kind
}
