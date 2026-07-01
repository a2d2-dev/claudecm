package adapter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/a2d2-dev/claudecm/internal/config"
	"github.com/a2d2-dev/claudecm/internal/storage"
	"github.com/a2d2-dev/claudecm/internal/writepath"
)

// mockAdapter proves at compile time that the Adapter interface is
// implementable with a plain struct and that every method signature
// matches. It carries no logic — E3-S2..E4-S7 ship the real ones. Any
// call to a mock method returns errNotImplemented so accidental use
// from other tests is loud rather than silent (NFR-S1 spirit: no
// fallback synthesis).
type mockAdapter struct{ id ToolID }

var errNotImplemented = errors.New("mockAdapter: not implemented")

func (m *mockAdapter) ID() ToolID { return m.id }

func (m *mockAdapter) Detect(ctx context.Context, r *storage.Resolver) (Presence, error) {
	return Presence{}, errNotImplemented
}

func (m *mockAdapter) Files(r *storage.Resolver) OwnedFiles { return nil }

func (m *mockAdapter) Import(ctx context.Context, r *storage.Resolver) (CoreFromTool, OverlayFromTool, error) {
	return CoreFromTool{}, OverlayFromTool{}, errNotImplemented
}

func (m *mockAdapter) Plan(ctx context.Context, r *storage.Resolver, profile config.Profile) ([]WritePlan, error) {
	return nil, errNotImplemented
}

func (m *mockAdapter) Apply(ctx context.Context, r *storage.Resolver, plan WritePlan) (ApplyReport, error) {
	return ApplyReport{}, errNotImplemented
}

func (m *mockAdapter) Project(ctx context.Context, r *storage.Resolver, profile config.Profile) (EffectiveView, error) {
	return EffectiveView{}, errNotImplemented
}

// Compile-time assertions.
var _ Adapter = (*mockAdapter)(nil)

// Ensure re-exports remain honest aliases so downstream packages that
// import writepath and the adapter package see the same underlying
// types. If any of these ever cease to be aliases (e.g. someone
// converts them to distinct types) this block fails at compile time.
var (
	_ WritePlan   = writepath.WritePlan{}
	_ ApplyReport = writepath.WriteReport{}
	_ ToolID      = config.ToolID("")
)

func TestToolIDsExported(t *testing.T) {
	// Story AC (E3-S1): ToolID is a typed string constrained to
	// "claude_code" and "codex" in v1 (architecture §2.1).
	if got, want := string(ToolClaudeCode), "claude_code"; got != want {
		t.Errorf("ToolClaudeCode = %q, want %q", got, want)
	}
	if got, want := string(ToolCodex), "codex"; got != want {
		t.Errorf("ToolCodex = %q, want %q", got, want)
	}
	// Alias vs config: must be the SAME underlying value, not a
	// re-typed twin. Cross-package equality catches accidental
	// re-declaration.
	if ToolClaudeCode != config.ToolClaudeCode {
		t.Errorf("ToolClaudeCode alias mismatch vs config.ToolClaudeCode")
	}
	if ToolCodex != config.ToolCodex {
		t.Errorf("ToolCodex alias mismatch vs config.ToolCodex")
	}
}

func TestRegistry_HappyRegisterGetList(t *testing.T) {
	reg := NewRegistry()

	idA := ToolID("mock-a")
	idB := ToolID("mock-b")

	reg.Register(idA, func() Adapter { return &mockAdapter{id: idA} })
	reg.Register(idB, func() Adapter { return &mockAdapter{id: idB} })

	// Get resolves each id.
	gotA, okA := reg.Get(idA)
	if !okA {
		t.Fatalf("Get(%q) missing", idA)
	}
	if gotA.ID() != idA {
		t.Errorf("Get(%q).ID() = %q, want %q", idA, gotA.ID(), idA)
	}
	gotB, okB := reg.Get(idB)
	if !okB {
		t.Fatalf("Get(%q) missing", idB)
	}
	if gotB.ID() != idB {
		t.Errorf("Get(%q).ID() = %q, want %q", idB, gotB.ID(), idB)
	}

	// Each Get returns a fresh instance (constructor is invoked per
	// call). Not a hard requirement of the interface, but the way
	// tests written against the constructor pattern would break if
	// this ever silently changed.
	gotA2, _ := reg.Get(idA)
	if gotA == gotA2 {
		t.Errorf("Get returned the same pointer twice; expected constructor to run per Get")
	}

	// List is sorted.
	got := reg.List()
	want := []ToolID{idA, idB}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v (sorted)", got, want)
	}

	// Register a third id that sorts before the others; List order
	// must reflect the new lexicographic ordering.
	idC := ToolID("mock-0")
	reg.Register(idC, func() Adapter { return &mockAdapter{id: idC} })
	got = reg.List()
	want = []ToolID{idC, idA, idB}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() after third Register = %v, want %v (sorted)", got, want)
	}
}

func TestRegistry_DoublePanics(t *testing.T) {
	reg := NewRegistry()
	id := ToolID("mock-dup")
	reg.Register(id, func() Adapter { return &mockAdapter{id: id} })

	defer func() {
		msg := recover()
		if msg == nil {
			t.Fatalf("second Register did not panic")
		}
		s, ok := msg.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", msg)
		}
		if want := "duplicate registration"; !contains(s, want) {
			t.Errorf("panic message = %q, want it to contain %q", s, want)
		}
		if !contains(s, string(id)) {
			t.Errorf("panic message = %q, want it to name the offending ToolID %q", s, id)
		}
	}()
	reg.Register(id, func() Adapter { return &mockAdapter{id: id} })
}

func TestRegistry_EmptyIDPanics(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatalf("Register with empty ToolID did not panic")
		}
	}()
	reg.Register(ToolID(""), func() Adapter { return &mockAdapter{} })
}

func TestRegistry_NilCtorPanics(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		msg := recover()
		if msg == nil {
			t.Fatalf("Register with nil constructor did not panic")
		}
		if s, ok := msg.(string); !ok || !contains(s, "mock-nil") {
			t.Errorf("panic message = %v, want it to name the offending ToolID", msg)
		}
	}()
	reg.Register(ToolID("mock-nil"), nil)
}

func TestRegistry_GetMissingReturnsFalse(t *testing.T) {
	reg := NewRegistry()
	got, ok := reg.Get(ToolID("nope"))
	if ok {
		t.Errorf("Get(unregistered) ok = true, want false")
	}
	if got != nil {
		t.Errorf("Get(unregistered) adapter = %v, want nil", got)
	}
	// A fresh registry lists nothing.
	if list := reg.List(); len(list) != 0 {
		t.Errorf("empty registry List() = %v, want empty slice", list)
	}
}

func TestRegistry_ConcurrentGetList(t *testing.T) {
	// Registers happen first (sequentially, as init() would); then
	// N goroutines slam Get/List concurrently. Must be race-free
	// under `go test -race`.
	reg := NewRegistry()
	ids := []ToolID{
		ToolID("mock-alpha"),
		ToolID("mock-beta"),
		ToolID("mock-gamma"),
		ToolID("mock-delta"),
	}
	for _, id := range ids {
		id := id
		reg.Register(id, func() Adapter { return &mockAdapter{id: id} })
	}

	const workers = 32
	const iters = 500

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := ids[(w+i)%len(ids)]
				a, ok := reg.Get(id)
				if !ok {
					t.Errorf("Get(%q) missing under concurrency", id)
					return
				}
				if a.ID() != id {
					t.Errorf("Get(%q).ID() = %q under concurrency", id, a.ID())
					return
				}
				if list := reg.List(); len(list) != len(ids) {
					t.Errorf("List() len = %d, want %d under concurrency", len(list), len(ids))
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestDefaultRegistryShortcuts(t *testing.T) {
	// DefaultRegistry is written to only from init() in adapter
	// sub-packages; in this test we exercise the package-level
	// shortcuts against an id no adapter would ever claim. The
	// documented double-register guard means we cannot cleanly
	// unregister afterwards, so use a random-flavored id and just
	// assert Get + List reflect the write.
	id := ToolID("mock-default-registry-shortcut")

	if _, ok := Get(id); ok {
		t.Fatalf("precondition: DefaultRegistry already contains %q", id)
	}

	Register(id, func() Adapter { return &mockAdapter{id: id} })

	got, ok := Get(id)
	if !ok || got == nil || got.ID() != id {
		t.Errorf("Get after Register: (%v, %v), want (adapter with id=%q, true)", got, ok, id)
	}

	// List includes our id, sorted.
	found := false
	list := List()
	for i, listed := range list {
		if listed == id {
			found = true
			// Confirm sort invariant: neighbors are in order.
			if i > 0 && list[i-1] >= id {
				t.Errorf("List not sorted around inserted id: %v", list)
			}
			if i+1 < len(list) && list[i+1] <= id {
				t.Errorf("List not sorted around inserted id: %v", list)
			}
		}
	}
	if !found {
		t.Errorf("List() missing registered id %q; got %v", id, list)
	}
}

func TestLayerConstantsMatchArchitectureNames(t *testing.T) {
	// Story AC (E3-S1 F6): the Layer string values must match
	// architecture.md §6 verbatim so `explain` output uses the exact
	// operator-facing names in the architecture doc.
	cases := []struct {
		got  Layer
		want string
	}{
		{LayerDefault, "BuiltInDefault"},
		{LayerCore, "ProfileCore"},
		{LayerOverlay, "ProfileOverlay"},
		{LayerOnDisk, "OnDiskToolConfig"},
		{LayerEnvOverride, "EnvOverride"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Layer = %q, want %q", string(c.got), c.want)
		}
	}
}

func TestSortFields(t *testing.T) {
	// SortFields must be a stable, in-place lexicographic sort by
	// Key so `current`/`explain` output is deterministic regardless
	// of the order the adapter emitted fields in.
	fields := []EffectiveField{
		{Key: "env.ANTHROPIC_MODEL", Value: "claude-3-5-sonnet"},
		{Key: "env.ANTHROPIC_API_KEY", Value: "sk-xxx", Secret: true},
		{Key: "env.ANTHROPIC_BASE_URL", Value: "https://api.anthropic.com"},
	}
	SortFields(fields)
	want := []string{
		"env.ANTHROPIC_API_KEY",
		"env.ANTHROPIC_BASE_URL",
		"env.ANTHROPIC_MODEL",
	}
	got := make([]string, len(fields))
	for i, f := range fields {
		got[i] = f.Key
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SortFields order = %v, want %v", got, want)
	}
	// Secret + non-string Value carriers must survive the sort.
	if !fields[0].Secret {
		t.Errorf("SortFields dropped Secret bit on fields[0] = %+v", fields[0])
	}
	// Idempotent: sorting a sorted slice is a no-op.
	SortFields(fields)
	got2 := make([]string, len(fields))
	for i, f := range fields {
		got2[i] = f.Key
	}
	if !reflect.DeepEqual(got2, want) {
		t.Errorf("SortFields not idempotent: %v -> %v", want, got2)
	}
	// Nil/empty is a no-op, not a panic.
	SortFields(nil)
	SortFields([]EffectiveField{})
}

func TestEffectiveFieldHeterogeneousValueTypes(t *testing.T) {
	// F2 contract: Value is any. Adapters must be able to store
	// bool / int64 / []string alongside string values, and Secret
	// operates independently of Go type.
	view := EffectiveView{
		Tool: ToolClaudeCode,
		Fields: []EffectiveField{
			{Key: "env.CLAUDE_CODE_USE_BEDROCK", Value: true, WinningLayer: LayerEnvOverride},
			{Key: "env.ANTHROPIC_API_KEY", Value: "sk-live-abc", Secret: true, WinningLayer: LayerOnDisk,
				Shadowed: []ShadowedLayer{{Layer: LayerCore, Source: "profile.core", Value: "sk-core-xxx", Secret: true}}},
			{Key: "env.SOMETHING_NUMERIC", Value: int64(42), WinningLayer: LayerCore},
		},
	}
	// Sanity: the map->slice migration means Fields is indexable by
	// position and can be sorted; the shape must be a slice.
	if got, want := len(view.Fields), 3; got != want {
		t.Fatalf("Fields len = %d, want %d", got, want)
	}
	SortFields(view.Fields)
	if view.Fields[0].Key != "env.ANTHROPIC_API_KEY" {
		t.Errorf("Fields[0].Key = %q after sort, want env.ANTHROPIC_API_KEY", view.Fields[0].Key)
	}
	// Value type-switching contract: renderers must be able to
	// pattern-match on the underlying type.
	switch v := view.Fields[2].Value.(type) {
	case int64:
		if v != 42 {
			t.Errorf("int64 Value = %d, want 42", v)
		}
	default:
		t.Errorf("Value type = %T, want int64", v)
	}
	// Shadowed carries Secret alongside its Value.
	if len(view.Fields[0].Shadowed) != 1 || !view.Fields[0].Shadowed[0].Secret {
		t.Errorf("Shadowed[0].Secret dropped: %+v", view.Fields[0].Shadowed)
	}
}

// contains is a tiny helper so we don't pull in strings just for panic
// message assertions.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
