package toml

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLoad_EmptyOrWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"newlines", "\n\n\n"},
		{"crlf", "\r\n\r\n"},
		{"tabs+newline", "\t\n \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Load([]byte(tc.in))
			if err != nil {
				t.Fatalf("Load(%q) unexpected error: %v", tc.in, err)
			}
			if len(d.Keys()) != 0 {
				t.Fatalf("Load(%q) Keys() = %v, want empty", tc.in, d.Keys())
			}
			b, err := d.Marshal()
			if err != nil {
				t.Fatalf("Marshal error: %v", err)
			}
			if len(b) != 0 {
				t.Fatalf("Marshal() = %q, want empty", string(b))
			}
		})
	}
}

func TestLoad_ValidTOML_TopLevelScalars(t *testing.T) {
	in := "model = \"sonnet\"\nmodel_provider = \"anthropic\"\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	v, ok := d.Get("model")
	if !ok || v != "sonnet" {
		t.Fatalf("Get(model) = (%v,%v), want (sonnet,true)", v, ok)
	}
	v, ok = d.Get("model_provider")
	if !ok || v != "anthropic" {
		t.Fatalf("Get(model_provider) = (%v,%v), want (anthropic,true)", v, ok)
	}
}

func TestLoad_Malformed(t *testing.T) {
	cases := []string{
		"model = ",
		"model = \"unterminated",
		"[unclosed section",
		"key = @@@",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := Load([]byte(in))
			if err == nil {
				t.Fatalf("Load(%q) expected error, got nil", in)
			}
			if !errors.Is(err, ErrParseFailed) {
				t.Fatalf("Load(%q) error = %v, want ErrParseFailed wrap", in, err)
			}
		})
	}
}

func TestSet_NewKey_EmptyDoc(t *testing.T) {
	d, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("model", "sonnet"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := "model = \"sonnet\"\n"
	if string(got) != want {
		t.Fatalf("Marshal = %q, want %q", string(got), want)
	}
}

func TestSet_ExistingKey_ReplacedInPlace(t *testing.T) {
	in := "# top comment\nmodel = \"old\"\nmodel_provider = \"anthropic\"\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("model", "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := "# top comment\nmodel = \"new\"\nmodel_provider = \"anthropic\"\n"
	if string(got) != want {
		t.Fatalf("Marshal =\n%q\nwant\n%q", string(got), want)
	}
}

func TestSet_NestedPath_CreatesSection(t *testing.T) {
	d, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("model_providers.openai.base_url", "https://x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(got), "[model_providers.openai]") {
		t.Fatalf("expected header, got: %q", string(got))
	}
	if !strings.Contains(string(got), "base_url = \"https://x\"") {
		t.Fatalf("expected kv, got: %q", string(got))
	}
	// Warning surfaced.
	ws := d.Warnings()
	if len(ws) == 0 {
		t.Fatalf("expected warning about newly created section, got none")
	}
}

func TestSet_TypesPreserved(t *testing.T) {
	d, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		key string
		val any
	}{
		{"i", int64(42)},
		{"i2", int(7)},
		{"f", float64(3.14)},
		{"b", true},
		{"s", "hi"},
	}
	for _, c := range cases {
		if err := d.Set(c.key, c.val); err != nil {
			t.Fatalf("Set(%s): %v", c.key, err)
		}
	}
	// Round-trip check via Get.
	if v, _ := d.Get("i"); v != int64(42) {
		t.Fatalf("i got %T %v", v, v)
	}
	if v, _ := d.Get("i2"); v != int64(7) {
		t.Fatalf("i2 got %T %v (expected int -> int64 normalization)", v, v)
	}
	if v, _ := d.Get("f"); v != float64(3.14) {
		t.Fatalf("f got %T %v", v, v)
	}
	if v, _ := d.Get("b"); v != true {
		t.Fatalf("b got %v", v)
	}
	if v, _ := d.Get("s"); v != "hi" {
		t.Fatalf("s got %v", v)
	}
	// Marshal -> reload preserves types (through TOML on-disk syntax).
	raw, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	d2, err := Load(raw)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if v, _ := d2.Get("i"); v != int64(42) {
		t.Fatalf("reload i got %T %v", v, v)
	}
	if v, _ := d2.Get("f"); v != float64(3.14) {
		t.Fatalf("reload f got %T %v", v, v)
	}
	if v, _ := d2.Get("b"); v != true {
		t.Fatalf("reload b got %v", v)
	}
}

func TestGet_MissingReturnsFalse(t *testing.T) {
	d, err := Load([]byte("model = \"x\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v, ok := d.Get("missing"); ok {
		t.Fatalf("Get(missing) = (%v,true), want (nil,false)", v)
	}
	if v, ok := d.Get(""); ok {
		t.Fatalf("Get(empty) = (%v,true), want (nil,false)", v)
	}
}

func TestDelete_ExistingKey_RemovesFromMarshal(t *testing.T) {
	in := "a = 1\nb = 2\nc = 3\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Delete("b") {
		t.Fatalf("Delete(b) returned false")
	}
	if _, ok := d.Get("b"); ok {
		t.Fatalf("Get(b) still true after Delete")
	}
	got, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(got), "b =") {
		t.Fatalf("Marshal still contains b: %q", string(got))
	}
	if !strings.Contains(string(got), "a = 1") || !strings.Contains(string(got), "c = 3") {
		t.Fatalf("expected a and c preserved, got: %q", string(got))
	}
}

func TestDelete_MissingKeyReturnsFalse(t *testing.T) {
	d, err := Load([]byte("a = 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Delete("nope") {
		t.Fatalf("Delete(nope) returned true, want false")
	}
	if d.Delete("") {
		t.Fatalf("Delete(empty) returned true, want false")
	}
}

func TestDelete_SyntheticSection_DroppedWhenEmpty(t *testing.T) {
	d, _ := Load(nil)
	if err := d.Set("model_providers.openai.base_url", "https://x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !d.Delete("model_providers.openai.base_url") {
		t.Fatalf("Delete returned false")
	}
	got, _ := d.Marshal()
	if strings.Contains(string(got), "[model_providers.openai]") {
		t.Fatalf("expected synthetic section dropped, got: %q", string(got))
	}
}

func TestDelete_ExistingSection_Preserved(t *testing.T) {
	// Existing empty tables are preserved: the [foo] header stays even after
	// deleting its only key.
	in := "[foo]\na = 1\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !d.Delete("foo.a") {
		t.Fatalf("Delete returned false")
	}
	got, _ := d.Marshal()
	if !strings.Contains(string(got), "[foo]") {
		t.Fatalf("expected [foo] preserved, got: %q", string(got))
	}
}

func TestKeys_SortedFlatDottedPaths(t *testing.T) {
	in := `model = "x"
model_provider = "y"

[model_providers.openai]
base_url = "https://api.openai.com"
name = "openai"

[model_providers.anthropic]
base_url = "https://api.anthropic.com"
`
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := d.Keys()
	want := []string{
		"model",
		"model_provider",
		"model_providers.anthropic.base_url",
		"model_providers.openai.base_url",
		"model_providers.openai.name",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
}

func TestRoundTrip_ByteIdentical_NoMutation(t *testing.T) {
	// Story E4-S2 AC1: byte-identical pure round-trip.
	in := `# top comment
# second line
model = "sonnet"
model_provider = "anthropic"

# section preface
[model_providers.openai]
# above key
base_url = "https://api.openai.com"
name = "openai"

[model_providers.anthropic]
base_url = "https://api.anthropic.com"
# trailing comment
`
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != in {
		t.Fatalf("byte-identical round-trip failed:\nGOT:\n%s\nWANT:\n%s", string(out), in)
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("expected no warnings, got: %v", d.Warnings())
	}
}

func TestRoundTrip_CommentsPreserved_OnSurgicalUpdate(t *testing.T) {
	// Story E4-S2 AC2: comments/order around non-owned keys preserved when
	// an owned key is updated.
	in := `# global comment
model = "old"

[model_providers.openai]
# above base_url
base_url = "https://api.openai.com"
name = "openai"
`
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("model", "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `# global comment
model = "new"

[model_providers.openai]
# above base_url
base_url = "https://api.openai.com"
name = "openai"
`
	if string(got) != want {
		t.Fatalf("Marshal =\n%s\nwant\n%s", string(got), want)
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("expected no warnings on surgical update, got: %v", d.Warnings())
	}
}

func TestRoundTrip_KeyOrderPreserved(t *testing.T) {
	in := "z = 1\ny = 2\nx = 3\nm = 4\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("order preservation failed:\nGOT:\n%s\nWANT:\n%s", string(out), in)
	}
}

func TestSet_ExistingSection_AppendsKey(t *testing.T) {
	in := `[model_providers.openai]
name = "openai"
`
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("model_providers.openai.base_url", "https://x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ := d.Marshal()
	// Existing header preserved, new key appended within same section (no
	// duplicated header, no synthetic warning).
	if strings.Count(string(got), "[model_providers.openai]") != 1 {
		t.Fatalf("expected exactly one header, got:\n%s", string(got))
	}
	if !strings.Contains(string(got), "base_url = \"https://x\"") {
		t.Fatalf("expected base_url appended, got:\n%s", string(got))
	}
	if len(d.Warnings()) != 0 {
		t.Fatalf("expected no warnings when appending to existing section, got: %v", d.Warnings())
	}
}

func TestSet_RejectsEmptyPath(t *testing.T) {
	d, _ := Load(nil)
	if err := d.Set("", "x"); err == nil {
		t.Fatalf("expected error for empty path")
	}
}

func TestSet_RejectsUnsupportedType(t *testing.T) {
	d, _ := Load(nil)
	type X struct{ A int }
	if err := d.Set("k", X{A: 1}); err == nil {
		t.Fatalf("expected error for unsupported type")
	}
}

func TestSet_NilDeletes(t *testing.T) {
	d, _ := Load([]byte("k = 1\n"))
	if err := d.Set("k", nil); err != nil {
		t.Fatalf("Set(nil): %v", err)
	}
	if _, ok := d.Get("k"); ok {
		t.Fatalf("Get(k) still true after Set(nil)")
	}
}

func TestWarnings_ClearedOnEachMarshal(t *testing.T) {
	d, _ := Load(nil)
	_ = d.Set("model_providers.openai.base_url", "https://x")
	_, _ = d.Marshal()
	if len(d.Warnings()) == 0 {
		t.Fatalf("expected warning after synthetic section")
	}
	// Second Marshal call still emits the warning (it's derived, not consumed).
	_, _ = d.Marshal()
	if len(d.Warnings()) == 0 {
		t.Fatalf("expected warning to persist across Marshal calls (recomputed)")
	}
}

func TestCRLF_RoundTrip(t *testing.T) {
	in := "model = \"x\"\r\na = 1\r\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("CRLF round-trip failed: got %q want %q", string(out), in)
	}
}

func TestSet_MapValue_EmitsInlineTable(t *testing.T) {
	d, _ := Load(nil)
	if err := d.Set("point", map[string]any{"x": int64(1), "y": int64(2)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, _ := d.Marshal()
	// Sorted key order in inline table.
	want := "point = {x = 1, y = 2}\n"
	if string(out) != want {
		t.Fatalf("Marshal = %q, want %q", string(out), want)
	}
}

func TestSet_ArrayValue_UsesGotomlFormatting(t *testing.T) {
	d, _ := Load(nil)
	if err := d.Set("nums", []any{int64(1), int64(2), int64(3)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, _ := d.Marshal()
	if !strings.Contains(string(out), "nums = ") {
		t.Fatalf("expected nums = ..., got %q", string(out))
	}
}

func TestNormalizeValue_Widths(t *testing.T) {
	d, _ := Load(nil)
	_ = d.Set("a", int32(5))
	_ = d.Set("b", uint(6))
	_ = d.Set("c", uint32(7))
	_ = d.Set("d", uint64(8))
	_ = d.Set("e", float32(1.5))
	if v, _ := d.Get("a"); v != int64(5) {
		t.Fatalf("int32 not normalized: %T %v", v, v)
	}
	if v, _ := d.Get("b"); v != int64(6) {
		t.Fatalf("uint not normalized: %T %v", v, v)
	}
	if v, _ := d.Get("c"); v != int64(7) {
		t.Fatalf("uint32 not normalized: %T %v", v, v)
	}
	if v, _ := d.Get("d"); v != int64(8) {
		t.Fatalf("uint64 not normalized: %T %v", v, v)
	}
	if v, _ := d.Get("e"); v != float64(float32(1.5)) {
		t.Fatalf("float32 not normalized: %T %v", v, v)
	}
}

func TestScanStructure_ArrayOfTables_RoundTrip(t *testing.T) {
	in := "[[servers]]\nname = \"a\"\n\n[[servers]]\nname = \"b\"\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("array-of-tables round-trip:\ngot %q\nwant %q", string(out), in)
	}
}

func TestScanStructure_TrailingCommentPreserved(t *testing.T) {
	in := "a = 1\n\n# trailing comment\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("trailing comment round-trip:\ngot %q\nwant %q", string(out), in)
	}
}

func TestSyntheticSection_BlankLineSeparator(t *testing.T) {
	// When a synthetic section is appended after preserved content and the
	// buffer doesn't end with a blank line, Marshal inserts one so the
	// header sits on its own logical block.
	in := "a = 1\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := d.Set("new_section.key", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, _ := d.Marshal()
	got := string(out)
	if !strings.Contains(got, "a = 1\n\n[new_section]") {
		t.Fatalf("expected blank line before synthetic section header, got: %q", got)
	}
}

func TestKeys_EmptyDoc(t *testing.T) {
	d, _ := Load(nil)
	if len(d.Keys()) != 0 {
		t.Fatalf("Keys() = %v, want empty", d.Keys())
	}
	if v, ok := d.Get("anything"); ok {
		t.Fatalf("Get(anything) = (%v,true)", v)
	}
}

func TestJoinPath(t *testing.T) {
	if got := joinPath("", "k"); got != "k" {
		t.Fatalf("joinPath(\"\",k) = %q", got)
	}
	if got := joinPath("a", ""); got != "a" {
		t.Fatalf("joinPath(a,\"\") = %q", got)
	}
	if got := joinPath("a", "b"); got != "a.b" {
		t.Fatalf("joinPath(a,b) = %q", got)
	}
}

func TestIsBalanced_MultiLineArray(t *testing.T) {
	in := "nums = [\n  1,\n  2,\n  3,\n]\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("multi-line array round-trip:\ngot %q\nwant %q", string(out), in)
	}
	// Value visible.
	v, ok := d.Get("nums")
	if !ok {
		t.Fatalf("Get(nums) missing")
	}
	arr, _ := v.([]any)
	if len(arr) != 3 {
		t.Fatalf("expected 3 elements, got %v", arr)
	}
}

func TestIsBalanced_TripleQuotedString(t *testing.T) {
	in := "s = \"\"\"\nhello\n\"\"\"\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("triple-quoted round-trip:\ngot %q\nwant %q", string(out), in)
	}
}

func TestSet_RepeatedDelete(t *testing.T) {
	d, _ := Load([]byte("k = 1\n"))
	if !d.Delete("k") {
		t.Fatalf("first Delete returned false")
	}
	if d.Delete("k") {
		t.Fatalf("second Delete returned true, want false (idempotent)")
	}
}

func TestLoad_InlineTableValue_RoundTrip(t *testing.T) {
	// Inline tables round-trip verbatim when unchanged.
	in := "point = { x = 1, y = 2 }\n"
	d, err := Load([]byte(in))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, _ := d.Marshal()
	if string(out) != in {
		t.Fatalf("inline table round-trip: got %q want %q", string(out), in)
	}
	// Get returns a map.
	v, ok := d.Get("point")
	if !ok {
		t.Fatalf("Get(point) missing")
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		t.Fatalf("Get(point) = %T, want map", v)
	}
	if m["x"] != int64(1) || m["y"] != int64(2) {
		t.Fatalf("inline table values: %v", m)
	}
}
