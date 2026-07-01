package toml

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	gotoml "github.com/pelletier/go-toml/v2"
)

// goStrconvQuote wraps strconv.Quote so we can localize the double-quoting
// choice in one place. Kept as a var-free wrapper to make future tweaks
// (e.g. \uXXXX escaping policy) trivial.
func goStrconvQuote(s string) string { return strconv.Quote(s) }

// scanStructure walks the raw TOML bytes line-by-line to build the ordered
// section+kv structure that Doc uses for surgical round-trip. Values are
// sourced from the already-decoded tree so we get correct Go types.
//
// This scanner is intentionally narrow: it recognizes blank lines, comment
// lines starting with '#', "[header]" and "[[header]]" table headers, and
// single-line "key = value" assignments. Continuation lines for multi-line
// strings, arrays, or inline tables that span physical lines are absorbed
// into the preceding kv via a balance heuristic.
func scanStructure(data []byte, tree map[string]any) (*Doc, error) {
	d := &Doc{eol: detectEOL(data)}
	// Root section is always present; not "created" (it's implicit from the
	// original input and its raw content lives in kv.commentRaw of its
	// first kv or in trailingRaw).
	root := &section{}
	d.sections = append(d.sections, root)
	current := root

	var pending bytes.Buffer // accumulates blank/comment lines waiting to attach

	lines := splitLinesKeepEnding(data)
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := trimLeadingSpace(stripLineEnding(line))
		switch {
		case len(trimmed) == 0:
			pending.Write(line)
			i++
			continue
		case trimmed[0] == '#':
			pending.Write(line)
			i++
			continue
		case trimmed[0] == '[':
			header, isArrayTable, ok := parseHeader(trimmed)
			if !ok {
				return nil, fmt.Errorf("unrecognized header line at %d: %q", i+1, stripLineEnding(line))
			}
			if isArrayTable {
				// Array-of-tables: we don't model kvs inside these for
				// Set/Delete. Preserve raw as an opaque section whose
				// body (all lines up to the next header) rides inside
				// trailingRaw. We use a "@array:" header prefix so
				// findKV cannot collide with a real dotted path.
				sec := &section{
					header:     arrayOfTablesHeaderPrefix + header,
					commentRaw: cloneBytes(pending.Bytes()),
					headerLine: cloneBytes(line),
				}
				pending.Reset()
				d.sections = append(d.sections, sec)
				current = sec
				i++
				// Consume body lines opaquely until the next header line
				// (which will start a new section).
				var body bytes.Buffer
				for i < len(lines) {
					peek := trimLeadingSpace(stripLineEnding(lines[i]))
					if len(peek) > 0 && peek[0] == '[' {
						break
					}
					body.Write(lines[i])
					i++
				}
				sec.trailingRaw = cloneBytes(body.Bytes())
				continue
			}
			sec := &section{
				header:     header,
				commentRaw: cloneBytes(pending.Bytes()),
				headerLine: cloneBytes(line),
			}
			pending.Reset()
			d.sections = append(d.sections, sec)
			current = sec
			i++
			continue
		default:
			// Assume it's a key = value line. Extract the key part; the
			// value part may extend across subsequent lines if it opens
			// a multi-line construct.
			key, valueStart, ok := extractKey(trimmed)
			if !ok {
				return nil, fmt.Errorf("unrecognized line at %d: %q", i+1, stripLineEnding(line))
			}
			// Capture the raw of this KV, extending forward until brackets/
			// braces/quotes balance out.
			var rawBuf bytes.Buffer
			rawBuf.Write(line)
			j := i
			valueBytes := []byte(valueStart)
			for !isBalanced(valueBytes) && j+1 < len(lines) {
				j++
				rawBuf.Write(lines[j])
				valueBytes = append(valueBytes, lines[j]...)
			}
			// Look up value in the tree by full dotted path.
			fullPath := joinPath(current.header, key)
			val, found := lookupPath(tree, fullPath)
			if !found {
				// The unmarshal saw this key. If we can't find it, our
				// path splitting doesn't agree with go-toml's. That
				// usually means the key contains quoted dots; refuse.
				return nil, fmt.Errorf("value for key %q not found in decoded tree", fullPath)
			}
			entry := &kv{
				key:        key,
				value:      val,
				commentRaw: cloneBytes(pending.Bytes()),
				lineRaw:    cloneBytes(rawBuf.Bytes()),
			}
			pending.Reset()
			current.kvs = append(current.kvs, entry)
			i = j + 1
			continue
		}
	}

	// Any remaining pending bytes belong to the final section as trailing.
	if pending.Len() > 0 {
		current.trailingRaw = cloneBytes(pending.Bytes())
	}
	return d, nil
}

// detectEOL inspects the first line ending of data and returns "\r\n" if it
// is CRLF, otherwise "\n". Empty / newline-free input defaults to "\n".
func detectEOL(data []byte) string {
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if i > 0 && data[i-1] == '\r' {
				return "\r\n"
			}
			return "\n"
		}
	}
	return "\n"
}

// splitLinesKeepEnding returns each line of data including its original line
// ending ("\n", "\r\n"). The last line may lack a terminator.
func splitLinesKeepEnding(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			out = append(out, data[start:i+1])
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

// stripLineEnding removes any trailing \r?\n from the line.
func stripLineEnding(line []byte) []byte {
	if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
		return line[:len(line)-2]
	}
	if len(line) >= 1 && line[len(line)-1] == '\n' {
		return line[:len(line)-1]
	}
	return line
}

// trimLeadingSpace trims ASCII spaces and tabs from the front.
func trimLeadingSpace(line []byte) []byte {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[i:]
}

// parseHeader parses a "[a.b.c]" or "[[a.b.c]]" header line (already trimmed
// of leading whitespace). Trailing whitespace/comment after the closing
// bracket is tolerated.
func parseHeader(trimmed []byte) (header string, isArrayTable bool, ok bool) {
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return "", false, false
	}
	rest := trimmed[1:]
	if len(rest) > 0 && rest[0] == '[' {
		isArrayTable = true
		rest = rest[1:]
	}
	// Find the matching closing bracket(s). We accept only bare-dotted
	// headers (no quoted key parts) — that matches Codex config shape.
	end := bytes.IndexByte(rest, ']')
	if end < 0 {
		return "", false, false
	}
	name := strings.TrimSpace(string(rest[:end]))
	if name == "" {
		return "", false, false
	}
	if isArrayTable {
		if end+1 >= len(rest) || rest[end+1] != ']' {
			return "", false, false
		}
	}
	// Validate: dotted identifier characters only.
	for _, r := range name {
		if r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return "", false, false
	}
	// Reject empty segments (leading, trailing, or consecutive dots): "[a.b.]"
	// and "[.a]" are malformed TOML headers even though every rune is a legal
	// bare-key character.
	for _, seg := range strings.Split(name, ".") {
		if seg == "" {
			return "", false, false
		}
	}
	return name, isArrayTable, true
}

// extractKey scans "key = value..." and returns the dotted key (unquoted) and
// the value expression string (trimmed of leading whitespace).
func extractKey(trimmed []byte) (key string, valueStart string, ok bool) {
	// Find first '=' outside of a quoted section. We reject quoted keys.
	for i, c := range trimmed {
		switch c {
		case '"', '\'':
			return "", "", false
		case '=':
			k := strings.TrimSpace(string(trimmed[:i]))
			v := strings.TrimSpace(string(trimmed[i+1:]))
			if k == "" {
				return "", "", false
			}
			// Validate bare-dotted key.
			for _, r := range k {
				if r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					continue
				}
				return "", "", false
			}
			return k, v, true
		}
	}
	return "", "", false
}

// isBalanced returns true if the given value expression has balanced quotes,
// brackets, and braces. This is a heuristic used to detect single-line
// values vs. multi-line continuations. It intentionally errs on the side of
// declaring balance when in doubt so simple values don't over-scan.
func isBalanced(value []byte) bool {
	var inStr byte
	tripleStr := false
	depthBracket := 0
	depthBrace := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		if inStr != 0 {
			if tripleStr {
				if i+2 < len(value) && value[i] == inStr && value[i+1] == inStr && value[i+2] == inStr {
					inStr = 0
					tripleStr = false
					i += 2
				}
				continue
			}
			if c == '\\' && inStr == '"' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '#':
			// Rest is a comment
			return depthBracket == 0 && depthBrace == 0
		case '"', '\'':
			if i+2 < len(value) && value[i+1] == c && value[i+2] == c {
				inStr = c
				tripleStr = true
				i += 2
				continue
			}
			inStr = c
		case '[':
			depthBracket++
		case ']':
			depthBracket--
		case '{':
			depthBrace++
		case '}':
			depthBrace--
		}
	}
	return inStr == 0 && depthBracket == 0 && depthBrace == 0
}

// lookupPath walks tree by dotted path and returns the value if found.
func lookupPath(tree map[string]any, path string) (any, bool) {
	dots := strings.Split(path, ".")
	var cur any = tree
	for _, part := range dots {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[part]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// renderKVLine emits a single "key = value[ trailingComment]<eol>" line, using
// go-toml's marshaller to format the value for maximum correctness. When
// trailingComment is non-empty it is appended verbatim (its leading whitespace
// is expected to be part of the byte slice), preserving inline comments from
// the original bytes on a mutated line. eol is written last; callers pass
// the document's dominant line ending so CRLF files stay CRLF end-to-end.
func renderKVLine(key string, value any, eol string, trailingComment []byte) ([]byte, error) {
	b, err := marshalScalar(value)
	if err != nil {
		return nil, err
	}
	if eol == "" {
		eol = "\n"
	}
	var out bytes.Buffer
	out.WriteString(key)
	out.WriteString(" = ")
	out.Write(b)
	if len(trailingComment) > 0 {
		out.Write(trailingComment)
	}
	out.WriteString(eol)
	return out.Bytes(), nil
}

// marshalScalar formats a value in TOML value-position syntax.
//
// Strings are always emitted as double-quoted basic strings (TOML basic-string
// form) for consistency and to match the idiomatic Codex config.toml shape;
// go-toml's default of literal-string quoting for delimiter-free strings would
// be a surprise across a Set-updated key sitting next to a hand-authored
// double-quoted sibling.
//
// For all other scalar and container types we trampoline through gotoml.Marshal
// on a single-key wrapper so upstream owns the formatting rules.
func marshalScalar(value any) ([]byte, error) {
	if s, ok := value.(string); ok {
		return []byte(quoteTOMLString(s)), nil
	}
	if m, ok := value.(map[string]any); ok {
		return marshalInlineMap(m)
	}
	wrapper := map[string]any{"__v": value}
	raw, err := gotoml.Marshal(wrapper)
	if err != nil {
		return nil, fmt.Errorf("claudecm/codex/toml: marshal value: %w", err)
	}
	line := stripTrailingNewline(raw)
	const prefix = "__v ="
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, fmt.Errorf("claudecm/codex/toml: unexpected marshal shape: %q", string(line))
	}
	rest := bytes.TrimLeft(line[len(prefix):], " ")
	return rest, nil
}

// quoteTOMLString serializes s as a double-quoted TOML basic string.
// TOML basic strings accept the same escapes as Go's strconv.Quote for the
// characters we care about (\b, \t, \n, \f, \r, \", \\), which is why we can
// delegate to it here.
func quoteTOMLString(s string) string {
	// strconv.Quote uses double quotes and escapes control chars similarly
	// to TOML basic strings.
	return goStrconvQuote(s)
}

// marshalInlineMap serializes a map as a TOML inline table "{k = v, ...}".
// Keys are emitted in sorted order for determinism.
func marshalInlineMap(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sort for determinism (no key order info survives a map[string]any).
	sortStrings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(k)
		buf.WriteString(" = ")
		vb, err := marshalScalar(m[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func sortStrings(s []string) {
	// tiny insertion sort to avoid pulling sort into this hot path twice
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func stripTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// isSupportedValue reports whether value is one of the types we round-trip
// with type fidelity. See Doc.Set.
func isSupportedValue(value any) bool {
	switch value.(type) {
	case nil,
		string,
		bool,
		int, int32, int64,
		uint, uint32, uint64,
		float32, float64,
		map[string]any,
		[]any:
		return true
	}
	return false
}

// normalizeValue coerces numeric widths so that Get after Set returns the same
// concrete type across round-trips (int -> int64, float32 -> float64).
func normalizeValue(value any) any {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case uint:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		return int64(v)
	case float32:
		return float64(v)
	}
	return value
}
