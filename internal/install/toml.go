package install

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// TrustState distinguishes safe repairs from files that require user review.
type TrustState string

const (
	TrustHealthy   TrustState = "healthy"
	TrustMissing   TrustState = "missing"
	TrustStale     TrustState = "stale"
	TrustMalformed TrustState = "malformed"
)

// TrustStatus is the typed state of one Codex trust value.
type TrustStatus struct {
	State  TrustState
	Detail string
}

// Repairable reports whether Interlock can safely add or update this value.
func (s TrustStatus) Repairable() bool {
	return s.State == TrustMissing || s.State == TrustStale
}

type tomlSetting struct {
	table []string
	key   string
	value string
}

type tomlLayout struct {
	values map[string]unstable.Range
	tables map[string]unstable.Range
}

type tomlEdit struct {
	start int
	end   int
	body  []byte
}

func inspectTOMLValue(file string, path []string, want string) TrustStatus {
	snapshot, err := readSnapshot(file)
	if err != nil {
		return TrustStatus{State: TrustMalformed, Detail: err.Error()}
	}
	if !snapshot.existed {
		return TrustStatus{State: TrustMissing, Detail: "Codex config file does not exist"}
	}
	doc, err := decodeTOML(snapshot.raw)
	if err != nil {
		return TrustStatus{State: TrustMalformed, Detail: err.Error()}
	}
	value, ok := lookupTOML(doc, path)
	if !ok {
		return TrustStatus{State: TrustMissing, Detail: "trust value is missing"}
	}
	got, ok := value.(string)
	if !ok {
		return TrustStatus{
			State:  TrustMalformed,
			Detail: fmt.Sprintf("trust value at %s is not a string", strings.Join(path, ".")),
		}
	}
	if got != want {
		return TrustStatus{
			State:  TrustStale,
			Detail: fmt.Sprintf("trust value is %q, want %q", got, want),
		}
	}
	return TrustStatus{State: TrustHealthy, Detail: "trust value is current"}
}

func decodeTOML(raw []byte) (map[string]any, error) {
	var doc map[string]any
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func lookupTOML(doc map[string]any, path []string) (any, bool) {
	var current any = doc
	for _, part := range path {
		table, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = table[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func updateTOML(raw []byte, settings []tomlSetting) ([]byte, error) {
	if _, err := decodeTOML(raw); err != nil {
		return nil, err
	}
	layout, err := parseTOMLLayout(raw)
	if err != nil {
		return nil, err
	}

	var edits []tomlEdit
	var appendBody strings.Builder
	for _, setting := range settings {
		fullPath := append(slices.Clone(setting.table), setting.key)
		if valueRange, ok := layout.values[tomlPath(fullPath)]; ok {
			edits = append(edits, tomlEdit{
				start: int(valueRange.Offset),
				end:   int(valueRange.Offset + valueRange.Length),
				body:  []byte(strconv.Quote(setting.value)),
			})
			continue
		}
		if tableRange, ok := layout.tables[tomlPath(setting.table)]; ok {
			offset := lineEnd(raw, int(tableRange.Offset+tableRange.Length))
			edits = append(edits, tomlEdit{
				start: offset,
				end:   offset,
				body:  []byte(quoteTOMLKey(setting.key) + " = " + strconv.Quote(setting.value) + "\n"),
			})
			continue
		}

		if appendBody.Len() == 0 && len(raw) > 0 && raw[len(raw)-1] != '\n' {
			appendBody.WriteByte('\n')
		}
		appendBody.WriteByte('\n')
		appendBody.WriteByte('[')
		for i, part := range setting.table {
			if i > 0 {
				appendBody.WriteByte('.')
			}
			appendBody.WriteString(quoteTOMLKey(part))
		}
		appendBody.WriteString("]\n")
		appendBody.WriteString(quoteTOMLKey(setting.key))
		appendBody.WriteString(" = ")
		appendBody.WriteString(strconv.Quote(setting.value))
		appendBody.WriteByte('\n')
	}
	if appendBody.Len() > 0 {
		edits = append(edits, tomlEdit{
			start: len(raw), end: len(raw), body: []byte(appendBody.String()),
		})
	}

	slices.SortFunc(edits, func(a, b tomlEdit) int {
		return b.start - a.start
	})
	updated := slices.Clone(raw)
	for _, edit := range edits {
		updated = append(updated[:edit.start],
			append(slices.Clone(edit.body), updated[edit.end:]...)...)
	}

	doc, err := decodeTOML(updated)
	if err != nil {
		return nil, fmt.Errorf("generated Codex config is invalid: %w", err)
	}
	for _, setting := range settings {
		path := append(slices.Clone(setting.table), setting.key)
		value, ok := lookupTOML(doc, path)
		if !ok || value != setting.value {
			return nil, fmt.Errorf("generated Codex config did not set %s",
				strings.Join(path, "."))
		}
	}
	return updated, nil
}

func parseTOMLLayout(raw []byte) (tomlLayout, error) {
	layout := tomlLayout{
		values: map[string]unstable.Range{},
		tables: map[string]unstable.Range{},
	}
	var parser unstable.Parser
	parser.Reset(raw)
	var currentTable []string
	for parser.NextExpression() {
		expression := parser.Expression()
		switch expression.Kind {
		case unstable.Table:
			currentTable = nodeKey(expression)
			layout.tables[tomlPath(currentTable)] = expression.Raw
		case unstable.ArrayTable:
			currentTable = nodeKey(expression)
		case unstable.KeyValue:
			path := append(slices.Clone(currentTable), nodeKey(expression)...)
			layout.values[tomlPath(path)] = expression.Value().Raw
		}
	}
	if err := parser.Error(); err != nil {
		return tomlLayout{}, fmt.Errorf("parse TOML layout: %w", err)
	}
	return layout, nil
}

func nodeKey(node *unstable.Node) []string {
	var parts []string
	iterator := node.Key()
	for iterator.Next() {
		parts = append(parts, string(iterator.Node().Data))
	}
	return parts
}

func tomlPath(parts []string) string { return strings.Join(parts, "\x00") }

func lineEnd(raw []byte, offset int) int {
	for offset < len(raw) {
		switch raw[offset] {
		case '\n':
			return offset + 1
		case '\r':
			if offset+1 < len(raw) && raw[offset+1] == '\n' {
				return offset + 2
			}
			return offset + 1
		default:
			offset++
		}
	}
	return offset
}

func quoteTOMLKey(key string) string { return strconv.Quote(key) }

// lineStart returns the offset of the first byte on the line containing offset.
func lineStart(raw []byte, offset int) int {
	for offset > 0 && raw[offset-1] != '\n' {
		offset--
	}
	return offset
}

// removeTOMLTables deletes whole tables, header and body, by path.
//
// A table's body runs to the next table header, so the boundary is taken from
// the document rather than guessed from blank lines: a hand-edited file may put
// comments or spacing anywhere, and none of it should change which bytes belong
// to which table.
func removeTOMLTables(raw []byte, targets [][]string) ([]byte, int, error) {
	if _, err := decodeTOML(raw); err != nil {
		return nil, 0, err
	}
	want := map[string]bool{}
	for _, target := range targets {
		want[tomlPath(target)] = true
	}

	type header struct {
		path  string
		start int
	}
	var headers []header
	var parser unstable.Parser
	parser.Reset(raw)
	for parser.NextExpression() {
		expression := parser.Expression()
		switch expression.Kind {
		case unstable.Table, unstable.ArrayTable:
			// A table expression carries no source range of its own, so the
			// header is located from its first key part instead. Reading the
			// zero-valued range put every cut at offset zero and removed
			// nothing at all.
			it := expression.Key()
			if !it.Next() {
				continue
			}
			headers = append(headers, header{
				path:  tomlPath(nodeKey(expression)),
				start: lineStart(raw, int(it.Node().Raw.Offset)),
			})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, 0, fmt.Errorf("parse TOML layout: %w", err)
	}

	var cuts []tomlEdit
	for i, h := range headers {
		if !want[h.path] {
			continue
		}
		end := len(raw)
		if i+1 < len(headers) {
			end = headers[i+1].start
		}
		cuts = append(cuts, tomlEdit{start: h.start, end: end})
	}
	if len(cuts) == 0 {
		return raw, 0, nil
	}

	// Back to front, so earlier offsets stay valid as later cuts are applied.
	slices.SortFunc(cuts, func(a, b tomlEdit) int { return b.start - a.start })
	updated := slices.Clone(raw)
	for _, cut := range cuts {
		updated = append(updated[:cut.start], updated[cut.end:]...)
	}

	if _, err := decodeTOML(updated); err != nil {
		return nil, 0, fmt.Errorf("removing tables produced invalid TOML: %w", err)
	}
	return updated, len(cuts), nil
}
