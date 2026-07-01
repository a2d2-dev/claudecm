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

// Doc is a mutable TOML document that preserves original comments, key order,
// and section grouping across Load -> Set/Delete -> Marshal round-trips.
//
// The zero value is a valid empty document (equivalent to Load of "").
type Doc struct {
	sections []*section
	// warnings accumulate during Marshal when the wrapper had to fall back
	// from surgical raw-preserving emission (e.g. a section was newly
	// created and its layout is synthetic). Callers can inspect them via
	// Warnings() after Marshal.
	warnings []string
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

	for _, s := range d.sections {
		// Emit section commentRaw + headerLine (or a synthetic header for created sections).
		if s.created && s.header != "" {
			// Synthetic layout: warn if this section carries non-empty data or
			// is squeezed between preserved sections.
			d.warnings = append(d.warnings,
				fmt.Sprintf("comments/order may shift: section [%s] was newly created", s.header))
			if buf.Len() > 0 && !endsWithBlankLine(buf.Bytes()) {
				buf.WriteByte('\n')
			}
			buf.WriteString("[")
			buf.WriteString(s.header)
			buf.WriteString("]\n")
		} else {
			buf.Write(s.commentRaw)
			buf.Write(s.headerLine)
		}

		anyLive := false
		for _, k := range s.kvs {
			if k.deleted {
				continue
			}
			anyLive = true
			if k.created {
				// Newly inserted key: no original bytes. Emit a fresh
				// "key = value\n" line with no leading comments.
				line, err := renderKVLine(k.key, k.value)
				if err != nil {
					return nil, err
				}
				buf.Write(line)
				continue
			}
			if k.modified {
				// Preserve leading comments; regenerate the value line only.
				buf.Write(k.commentRaw)
				line, err := renderKVLine(k.key, k.value)
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
		if !s.created {
			buf.Write(s.trailingRaw)
		}

		// Suppress trailing runs of a fully-deleted section. If !anyLive AND
		// the section is not the root and is not created and the header +
		// trailing are all we have, we still preserved the header line above
		// (intentional: existing empty tables are preserved per contract).
		_ = anyLive
	}

	return buf.Bytes(), nil
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
