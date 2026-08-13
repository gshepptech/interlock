package runner

type claude struct{}

func (claude) Name() Name { return Claude }

func (claude) Capabilities() Capabilities {
	return Capabilities{Headless: true, StructuredLog: true, ModelSelection: true}
}

// Build constructs Claude Code's headless mode.
//
// There is no working-directory flag, so the directory reaches the agent as the
// process's own working directory. The prompt is not an argument: with --print
// and nothing positional, Claude reads it from stdin, which keeps it out of the
// machine's process listing.
func (c claude) Build(o Options, env Environment) (Launch, error) {
	path, dir, err := build(o, env, c.Capabilities(), string(Claude), nil)
	if err != nil {
		return Launch{}, err
	}
	args := []string{"--print", "--verbose", "--output-format", "stream-json"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return Launch{Path: path, Args: args, Dir: dir, Env: resolveEnv(o, env), Mode: o.Mode()}, nil
}

// Decode reads one line of Claude's stream-json output.
func (claude) Decode(stream Stream, line []byte) Event {
	if stream == Stderr {
		return Event{Kind: Output, Summary: SafeText(string(line), MaxEventSummary)}
	}
	return decodeLine(line, func(payload map[string]any) (EventKind, string) {
		kind := stringField(payload, "type")
		// A result carries its own verdict, so the type alone is not enough:
		// "result" with is_error set is a failure, not a completion.
		if kind == "result" {
			if boolField(payload, "is_error") {
				return Failure, claudeDetail(payload, kind)
			}
			return Result, claudeDetail(payload, kind)
		}
		switch kind {
		case "":
			return Output, ""
		case "error":
			return Failure, claudeDetail(payload, kind)
		default:
			return Activity, claudeDetail(payload, kind)
		}
	})
}

func claudeDetail(payload map[string]any, kind string) string {
	for _, field := range []string{"result", "subtype", "message"} {
		if value := stringField(payload, field); value != "" {
			return value
		}
	}
	return kind
}
