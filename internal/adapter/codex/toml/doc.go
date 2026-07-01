// Package toml is a thin, comment-and-order-preserving wrapper over
// github.com/pelletier/go-toml/v2, purpose-built for the Codex adapter's
// config.toml owned-key rewrites (PRD NFR-S7, architecture §10).
//
// # Scope
//
// This is NOT a general-purpose TOML library. It exposes exactly the surface
// the Codex adapter needs across stories E4-S3 (Import) and E4-S4 (Plan/Apply):
//
//   - Load a config.toml into an in-memory document that remembers original
//     comments, blank lines, and key order line-by-line.
//   - Get/Set/Delete values by dotted path.
//   - Marshal back to bytes, byte-identical on pure round-trip, with
//     surgical replacement of only the mutated key lines otherwise.
//
// # Supported input shape
//
// Real-world Codex config.toml files consist of top-level scalar assignments
// and [dotted.section] tables containing scalar keys. That is what this
// wrapper is optimized for. Specifically:
//
//   - Top-level "key = value" lines with single-line scalar values
//     (string, integer, float, bool).
//   - "[a.b.c]" table headers followed by scalar key-value lines.
//   - Blank lines and "# comment" lines above key-value lines or table
//     headers, which are attached to the following entry.
//   - Inline tables and arrays as scalar-position values (round-trip as
//     opaque blocks; Set replaces them wholesale).
//
// # Explicit limits (post-v1 follow-ups)
//
//   - Multi-line basic/literal strings (triple-quoted) and multi-line arrays
//     that span physical lines: the parser accepts them as valid TOML but the
//     wrapper attributes each continuation line to the same key entry via a
//     bracket/quote balance scan. If the balance heuristic fails, the whole
//     file is preserved by pass-through and mutations may trigger a
//     "comments/order may shift" warning surfaced via Doc.Warnings.
//   - "Quoted keys" with literal dots (e.g. "a.b" = 1 meaning a single key
//     whose name is the string "a.b") are not supported: any dotted path is
//     always split on '.'. Documented explicitly so callers cannot rely on
//     the ambiguity.
//   - Array-of-table headers ([[a]]) are recognized as opaque headers and
//     round-trip unchanged; Set/Delete against paths inside an array-of-table
//     is not supported and returns an error / false respectively.
//
// # No fallback
//
// Malformed input is refused with ErrParseFailed. There is no "best-effort"
// rewrite path (coding-standards rule 2, ADR-0001, ~/.claude/CLAUDE.md).
package toml

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	gotoml "github.com/pelletier/go-toml/v2"
)

// ErrParseFailed is returned by Load when the input bytes are not valid TOML.
// Callers must treat this as a hard refusal (NFR-S1).
var ErrParseFailed = errors.New("claudecm/codex/toml: parse failed")

// ErrArrayOfTablesMutation is returned by Doc.Set when the target path lives
// inside an [[array-of-tables]] block. Mutating array-of-tables content is not
// supported by this wrapper (see package doc). Delete returns false rather
// than this error, matching the idempotent-delete contract.
var ErrArrayOfTablesMutation = errors.New("claudecm/codex/toml: set into array-of-tables not supported")

// ErrShapeConflict is returned by Doc.Set when the target path would clash
// with the existing TOML shape - either writing a scalar where a table
// already exists, or writing a nested key under a scalar ancestor.
var ErrShapeConflict = errors.New("claudecm/codex/toml: shape conflict")

// Doc is a mutable TOML document that preserves original comments, key order,
// and section grouping across Load -> Set/Delete -> Marshal round-trips.
//
// The zero value is a valid empty document (equivalent to Load of "").
//
// Doc is NOT safe for concurrent Marshal or Set/Delete from multiple
// goroutines. Callers must serialize access (in production the writepath's
// flock provides this externally; in-process callers must add their own
// mutex if they share a Doc across goroutines).
type Doc struct {
	sections []*section
	// warnings accumulate during Marshal when the wrapper had to fall back
	// from surgical raw-preserving emission (e.g. a section was newly
	// created and its layout is synthetic). Callers can inspect them via
	// Warnings() after Marshal.
	warnings []string
	// eol is the dominant line ending detected on Load: "\n" or "\r\n".
	// Empty means unset (treat as "\n"). Used for all synthesized lines
	// (rewritten kv values, new headers, blank separators) so a CRLF file
	// stays CRLF end-to-end after Set/Delete.
	eol string
}

// section is a contiguous block of the document with a common table header
// (or the "root" pre-header block when header == "").
type section struct {
	header      string // dotted path, "" for root
	commentRaw  []byte // leading blank/comment lines before the header line (or before first kv for root)
	headerLine  []byte // "[header]\n" line as originally written; empty for root
	kvs         []*kv
	trailingRaw []byte // trailing blank/comment lines held with this section (before next header)
	created     bool   // true iff this section was inserted by Set (no original bytes)
}

// kv is a single key-value entry inside a section.
type kv struct {
	key        string // dotted key relative to the section header (usually a leaf)
	value      any    // decoded value
	commentRaw []byte // leading blank/comment lines above the key line
	lineRaw    []byte // the "key = value\n" line as originally written
	deleted    bool
	modified   bool // value changed via Set; regenerate lineRaw on Marshal
	created    bool // inserted via Set; commentRaw is empty and lineRaw is generated
}

// Load parses the given bytes as TOML. Empty or whitespace-only input yields
// an empty Doc (documented policy: absent config == {}).
//
// Malformed input returns ErrParseFailed (NFR-S1). No fallback.
func Load(data []byte) (*Doc, error) {
	if isWhitespaceOnly(data) {
		return &Doc{}, nil
	}

	// First: validate + decode into a value tree. This is the source of
	// truth for value types (int64 vs float64, string, bool, nested
	// tables). It also fails fast on malformed input.
	var tree map[string]any
	if err := gotoml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	// Second: walk the raw bytes line-by-line to capture structure,
	// comments, and key order.
	d, err := scanStructure(data, tree)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	return d, nil
}

// Marshal serializes the current Doc state to TOML bytes.
//
// If no Set/Delete calls were made since Load, Marshal returns the original
// bytes verbatim (byte-identical round-trip, AC1 of story E4-S2).
//
// On mutation, only the mutated key lines are regenerated; surrounding
// comments and non-owned keys are preserved verbatim.
//
// When a section is newly created (e.g. Set into a previously absent table),
// its header line is synthetic and Warnings() will report
// "comments/order may shift" per NFR-S7.
func (d *Doc) Marshal() ([]byte, error) {
	d.warnings = nil
	var buf bytes.Buffer
	eol := d.newline()

	for _, s := range d.sections {
		// Emit section commentRaw + headerLine (or a synthetic header for created sections).
		if s.created && s.header != "" {
			// Synthetic layout: warn if this section carries non-empty data or
			// is squeezed between preserved sections.
			d.warnings = append(d.warnings,
				fmt.Sprintf("comments/order may shift: section [%s] was newly created", s.header))
			if buf.Len() > 0 && !endsWithBlankLine(buf.Bytes()) {
				buf.WriteString(eol)
			}
			buf.WriteString("[")
			buf.WriteString(s.header)
			buf.WriteString("]")
			buf.WriteString(eol)
		} else {
			buf.Write(s.commentRaw)
			buf.Write(s.headerLine)
		}

		for _, k := range s.kvs {
			if k.deleted {
				continue
			}
			if k.created {
				// Newly inserted key: no original bytes. Emit a fresh
				// "key = value<eol>" line with no leading comments and no
				// preserved trailing comment.
				line, err := renderKVLine(k.key, k.value, eol, nil)
				if err != nil {
					return nil, err
				}
				buf.Write(line)
				continue
			}
			if k.modified {
				// Preserve leading comments; regenerate the value line only,
				// carrying over any trailing inline comment from the original
				// bytes so "key = new # note" stays "key = new # note".
				buf.Write(k.commentRaw)
				trailingComment := extractTrailingComment(k.lineRaw)
				if lineSpansMultiplePhysicalLines(k.lineRaw) {
					d.warnings = append(d.warnings,
						fmt.Sprintf("multi-line value on %s re-rendered inline", joinPath(s.header, k.key)))
				}
				line, err := renderKVLine(k.key, k.value, eol, trailingComment)
				if err != nil {
					return nil, err
				}
				buf.Write(line)
				continue
			}
			// Unchanged: emit original bytes verbatim.
			buf.Write(k.commentRaw)
			buf.Write(k.lineRaw)
		}

		// Trailing raw belongs with this section (blank/comment lines after
		// the last kv, before the next header). Preserve unless the section
		// itself is created (synthetic sections have no trailing raw).
		//
		// Note: when every kv of an existing section is deleted, we keep the
		// header + trailingRaw on purpose (see Delete godoc): non-owned empty
		// tables round-trip unchanged.
		if !s.created {
			buf.Write(s.trailingRaw)
		}
	}

	return buf.Bytes(), nil
}

// newline returns the document's dominant line ending, defaulting to "\n" for
// the zero value / empty documents.
func (d *Doc) newline() string {
	if d.eol == "" {
		return "\n"
	}
	return d.eol
}

// Warnings returns any warnings produced by the last Marshal call.
// See NFR-S7 "comments/order may shift" surfacing.
func (d *Doc) Warnings() []string {
	out := make([]string, len(d.warnings))
	copy(out, d.warnings)
	return out
}

// Get returns the value at the dotted path (e.g. "model_providers.openai.base_url").
// Returns (nil, false) if the path does not exist.
func (d *Doc) Get(path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	s, k, ok := d.findKV(path)
	if !ok {
		return nil, false
	}
	if s.kvs[k].deleted {
		return nil, false
	}
	return s.kvs[k].value, true
}

// Set assigns a value at the dotted path. Creates intermediate tables as needed.
//
// The path is split on '.' always. Quoted-key literal dots are NOT supported
// (see package doc). If the path already exists, its value is replaced but the
// surrounding comments and sibling ordering are preserved.
//
// Supported value types: string, int, int32, int64, uint, uint32, uint64,
// float32, float64, bool, map[string]any, []any, nil (deletes). Other types
// are rejected with an error to keep type fidelity across round-trips.
func (d *Doc) Set(path string, value any) error {
	if path == "" {
		return errors.New("claudecm/codex/toml: empty path")
	}
	if !isSupportedValue(value) {
		return fmt.Errorf("claudecm/codex/toml: unsupported value type %T for path %q", value, path)
	}
	if value == nil {
		d.Delete(path)
		return nil
	}
	// Refuse mutation into [[array-of-tables]] blocks (unsupported shape).
	if d.isInArrayOfTables(path) {
		return fmt.Errorf("%w: %s", ErrArrayOfTablesMutation, path)
	}
	// Refuse writes that would produce invalid TOML - scalar-under-table or
	// table-under-scalar shape mismatches.
	if err := d.checkShapeConflict(path); err != nil {
		return err
	}
	// Normalize numeric widths for round-trip type fidelity.
	value = normalizeValue(value)

	// If path already exists, update in place.
	if s, i, ok := d.findKV(path); ok {
		s.kvs[i].value = value
		s.kvs[i].modified = true
		s.kvs[i].deleted = false
		return nil
	}
	// Split path into (headerPath, leafKey). Prefer the deepest existing
	// section header that matches a prefix. Otherwise, create a new section
	// with the parent as header, leaf as key.
	dots := strings.Split(path, ".")
	var headerPath, leaf string
	if len(dots) == 1 {
		headerPath, leaf = "", dots[0]
	} else {
		headerPath = strings.Join(dots[:len(dots)-1], ".")
		leaf = dots[len(dots)-1]
	}
	sec := d.findOrCreateSection(headerPath)
	sec.kvs = append(sec.kvs, &kv{
		key:     leaf,
		value:   value,
		created: true,
	})
	return nil
}

// Delete removes the value at the dotted path.
//
// Returns true if a value was removed. Missing keys return false and are a
// no-op (idempotent).
//
// If the containing section becomes empty AND was created solely by this Doc
// (no original bytes), the empty section header is also removed. Existing
// empty tables are preserved to honor NFR-S7 non-owned-key semantics.
func (d *Doc) Delete(path string) bool {
	if path == "" {
		return false
	}
	// Deleting into an array-of-tables is not supported (see Set docs). Return
	// false to keep the idempotent-delete contract; the AOT body remains opaque.
	if d.isInArrayOfTables(path) {
		return false
	}
	s, i, ok := d.findKV(path)
	if !ok {
		return false
	}
	if s.kvs[i].deleted {
		return false
	}
	s.kvs[i].deleted = true

	if s.created {
		liveCount := 0
		for _, k := range s.kvs {
			if !k.deleted {
				liveCount++
			}
		}
		if liveCount == 0 {
			// Drop the synthetic section entirely.
			for idx, sec := range d.sections {
				if sec == s {
					d.sections = append(d.sections[:idx], d.sections[idx+1:]...)
					break
				}
			}
		}
	}
	return true
}

// Keys returns the flat dotted key paths of every live scalar/inline value in
// the document, in a deterministic sorted order.
func (d *Doc) Keys() []string {
	var out []string
	for _, s := range d.sections {
		for _, k := range s.kvs {
			if k.deleted {
				continue
			}
			out = append(out, joinPath(s.header, k.key))
		}
	}
	sort.Strings(out)
	return out
}

// findKV returns the containing section and index of the kv matching the
// dotted path, or ok=false if no match.
func (d *Doc) findKV(path string) (*section, int, bool) {
	for _, s := range d.sections {
		for i, k := range s.kvs {
			if joinPath(s.header, k.key) == path {
				return s, i, true
			}
		}
	}
	return nil, 0, false
}

// findOrCreateSection returns the section whose header equals headerPath,
// creating and appending one if none exists.
func (d *Doc) findOrCreateSection(headerPath string) *section {
	for _, s := range d.sections {
		if s.header == headerPath {
			return s
		}
	}
	// Ensure a root section exists too (invariant: sections[0].header == "").
	if len(d.sections) == 0 {
		d.sections = append(d.sections, &section{created: true})
	}
	if headerPath == "" {
		return d.sections[0]
	}
	ns := &section{header: headerPath, created: true}
	d.sections = append(d.sections, ns)
	return ns
}

func joinPath(header, key string) string {
	if header == "" {
		return key
	}
	if key == "" {
		return header
	}
	return header + "." + key
}

func isWhitespaceOnly(data []byte) bool {
	for _, b := range data {
		if b > 127 || !unicode.IsSpace(rune(b)) {
			return false
		}
	}
	return true
}

// arrayOfTablesHeaderPrefix is the sentinel prefix scanStructure uses to
// distinguish "@array:<name>" opaque sections from real table headers.
const arrayOfTablesHeaderPrefix = "@array:"

// isInArrayOfTables reports whether path targets a key inside any
// [[array-of-tables]] block we captured opaquely on Load.
func (d *Doc) isInArrayOfTables(path string) bool {
	for _, s := range d.sections {
		if !strings.HasPrefix(s.header, arrayOfTablesHeaderPrefix) {
			continue
		}
		name := s.header[len(arrayOfTablesHeaderPrefix):]
		if path == name || strings.HasPrefix(path, name+".") {
			return true
		}
	}
	return false
}

// checkShapeConflict returns ErrShapeConflict if writing a scalar at path
// would collide with the existing document shape:
//
//   - The path (or any prefix of the path) is already a section header
//     ([a] exists, caller wrote to "a" or "a.x"): scalar-under-table.
//   - An ancestor prefix of the path is already a scalar kv ("a = 5"
//     exists, caller wrote to "a.b"): table-under-scalar.
//
// Symmetric writes that just replace an existing scalar at path are handled
// by findKV in Set and never reach this check.
func (d *Doc) checkShapeConflict(path string) error {
	for _, s := range d.sections {
		h := s.header
		if h == "" || strings.HasPrefix(h, arrayOfTablesHeaderPrefix) {
			continue
		}
		if h == path {
			return fmt.Errorf("%w: %q is already a table", ErrShapeConflict, path)
		}
		if strings.HasPrefix(h, path+".") {
			return fmt.Errorf("%w: cannot write scalar at %q; table %q already exists", ErrShapeConflict, path, h)
		}
	}
	dots := strings.Split(path, ".")
	for i := 1; i < len(dots); i++ {
		prefix := strings.Join(dots[:i], ".")
		if s, idx, ok := d.findKV(prefix); ok && !s.kvs[idx].deleted {
			return fmt.Errorf("%w: cannot write %q; ancestor %q is a scalar", ErrShapeConflict, path, prefix)
		}
	}
	return nil
}

// extractTrailingComment returns the trailing inline comment (leading
// whitespace + "# ...") from a raw kv line, or nil if none is present. The
// returned bytes do NOT include the line ending; the caller appends its own.
//
// The scanner respects TOML string, bracket, and brace state so a "#" that
// appears inside a quoted string or an unterminated bracket/brace does not
// prematurely close the value.
func extractTrailingComment(lineRaw []byte) []byte {
	line := stripLineEnding(lineRaw)
	var inStr byte
	tripleStr := false
	depthBracket := 0
	depthBrace := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr != 0 {
			if tripleStr {
				if i+2 < len(line) && line[i] == inStr && line[i+1] == inStr && line[i+2] == inStr {
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
			if depthBracket == 0 && depthBrace == 0 {
				start := i
				for start > 0 && (line[start-1] == ' ' || line[start-1] == '\t') {
					start--
				}
				out := make([]byte, len(line)-start)
				copy(out, line[start:])
				return out
			}
		case '"', '\'':
			if i+2 < len(line) && line[i+1] == c && line[i+2] == c {
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
	return nil
}

// lineSpansMultiplePhysicalLines reports whether lineRaw carries more than
// one physical line (i.e., the original value used a multi-line array,
// inline table, or triple-quoted string continuation). Used to surface a
// warning when Set forces such a value back onto a single line.
func lineSpansMultiplePhysicalLines(lineRaw []byte) bool {
	if len(lineRaw) == 0 {
		return false
	}
	body := stripLineEnding(lineRaw)
	return bytes.IndexByte(body, '\n') >= 0
}

func endsWithBlankLine(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	// Look for "\n\n" or "\n\r\n" at the tail.
	if bytes.HasSuffix(b, []byte("\n\n")) || bytes.HasSuffix(b, []byte("\n\r\n")) {
		return true
	}
	return false
}
