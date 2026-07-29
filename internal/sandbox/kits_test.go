package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAdmit(t *testing.T) {
	tests := []struct {
		name    string
		info    KitInfo
		wantErr bool
	}{
		{"a plain mixin is admitted", KitInfo{Kind: "mixin"}, false},
		{"a sandbox kit is refused", KitInfo{Kind: "sandbox"}, true},
		{"an empty kind is refused", KitInfo{}, true},
		{"a mixin with resources is refused", KitInfo{Kind: "mixin", HasResources: true}, true},
		{"a mixin with runOptions is refused", KitInfo{Kind: "mixin", HasRunOptions: true}, true},
		{"a mixin with a template is refused", KitInfo{Kind: "mixin", HasTemplate: true}, true},
		{"a mixin with volumes is refused", KitInfo{Kind: "mixin", HasVolumes: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := admit(tc.info)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

func TestAdmitKits_DropsARejectedKitAndKeepsAnUnloadableOne(t *testing.T) {
	inspect := func(_ context.Context, ref string) (KitInfo, error) {
		switch ref {
		case "/good":
			return KitInfo{Kind: "mixin"}, nil
		case "/is-a-sandbox-kit":
			return KitInfo{Kind: "sandbox"}, nil
		default:
			return KitInfo{}, errors.New("cannot load")
		}
	}
	got := admitKits(context.Background(), inspect, map[string]string{
		"good":       "/good",
		"wrongkind":  "/is-a-sandbox-kit",
		"unloadable": "/typo",
	}, quietLog())

	want := map[string]string{"good": "/good", "unloadable": "/typo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// TestAdmitKits_TimeoutIsPerKit proves each kit's inspect call receives its
// OWN context (its own timer), not one context shared by the whole batch.
//
// An earlier version of this test blocked one kit on <-ctx.Done() and had a
// second kit check its own ctx.Err() once signaled by the first. A reviewer
// found that version passed under the OLD aggregate-bound code too (both
// designs resolve to the same deadline there), so a revert would not be
// caught. Rebuilding it to prove the deadline had actually fired uncovered a
// deeper problem: with two per-kit timers of the same duration started
// microseconds apart, which one fires first is a genuine race -- in local
// testing that construction passed only ~40% of the time under -race,
// regardless of how generous the timeout was (the raciness is about relative
// goroutine scheduling order, not duration).
//
// Comparing each kit's own ctx.Deadline() sidesteps the race entirely: under
// the old bug every kit shares the identical context.WithTimeout call (made
// once for the whole batch), so every kit observes the byte-identical
// deadline value. Under the per-kit bound, each kit's context.WithTimeout
// call runs in its own goroutine and computes its deadline from its own
// time.Now(), so the two deadlines are virtually certain to differ even
// though both goroutines start within microseconds of each other. No
// deadline needs to actually fire.
func TestAdmitKits_TimeoutIsPerKit(t *testing.T) {
	prev := kitInspectTimeout
	kitInspectTimeout = 50 * time.Millisecond // fast test; the value itself doesn't matter here
	t.Cleanup(func() { kitInspectTimeout = prev })

	var mu sync.Mutex
	deadlines := map[string]time.Time{}
	inspect := func(ctx context.Context, ref string) (KitInfo, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Errorf("inspect(%s): context has no deadline", ref)
		}
		mu.Lock()
		deadlines[ref] = dl
		mu.Unlock()
		return KitInfo{Kind: "mixin"}, nil
	}

	start := time.Now()
	admitKits(context.Background(), inspect, map[string]string{"a": "/a", "b": "/b"}, quietLog())

	mu.Lock()
	defer mu.Unlock()
	if deadlines["/a"].Equal(deadlines["/b"]) {
		t.Fatalf("kit deadlines are identical (%v): inspection is sharing one context for the whole batch instead of bounding each kit independently", deadlines["/a"])
	}
	// Each deadline should also actually be bounded by kitInspectTimeout, not
	// some other duration. Generous slack above the upper bound absorbs
	// goroutine-dispatch jitter without weakening what this catches: a
	// deadline computed from the wrong timeout (e.g. a forgotten override)
	// would miss by far more than that.
	window := start.Add(kitInspectTimeout + time.Second)
	for ref, dl := range deadlines {
		if dl.Before(start) || dl.After(window) {
			t.Fatalf("deadline for %s (%v) is outside [start, start+kitInspectTimeout] (%v..%v)", ref, dl, start, window)
		}
	}
}

func TestAdmitKits_EmptyConfigIsEmpty(t *testing.T) {
	inspect := func(context.Context, string) (KitInfo, error) {
		t.Fatal("inspect must not be called with no kits configured")
		return KitInfo{}, nil
	}
	if got := admitKits(context.Background(), inspect, nil, quietLog()); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestKitNames_SortedForAStableAdvertisement(t *testing.T) {
	got := kitNames(map[string]string{"zed": "/z", "alpha": "/a", "mid": "/m"})
	want := []string{"alpha", "mid", "zed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFake_AdmittedKitsAndCreate(t *testing.T) {
	f := NewFake("tools", "extras")

	if got, want := f.AdmittedKits(), []string{"extras", "tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AdmittedKits: want %v, got %v", got, want)
	}

	if _, err := f.Create(context.Background(), CreateSpec{Name: "s1", Kits: []string{"tools"}}); err != nil {
		t.Fatalf("create with a known kit: want nil, got %v", err)
	}

	_, err := f.Create(context.Background(), CreateSpec{Name: "s2", Kits: []string{"nope"}})
	if err == nil {
		t.Fatal("create with an unknown kit: want an error, got nil")
	}
}

func TestFake_NoKitsConfigured(t *testing.T) {
	f := NewFake()
	if got := f.AdmittedKits(); len(got) != 0 {
		t.Fatalf("want no kits, got %v", got)
	}
}

func TestHasResources(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"absent field is no resources", nil, false},
		{"explicit null is no resources", []byte("null"), false},
		{"empty object is no resources", []byte("{}"), false},
		{"empty object with a space is no resources", []byte("{ }"), false},
		{"a list is resources (no list form here; fails closed)", []byte("[]"), true},
		{"a populated object is resources", []byte(`{"cpu":2}`), true},
		{"a key with a null value is resources (one key, non-empty)", []byte(`{"cpu":null}`), true},
		{"unparseable garbage is resources", []byte(`{"cpu":`), true}, // fail closed
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasResources(tc.raw); got != tc.want {
				t.Fatalf("hasResources(%s): want %v, got %v", tc.raw, tc.want, got)
			}
		})
	}
}

// TestSDKBackend_Create_UnknownKit proves the unknown-kit check on the SDK
// backend (the branch that actually enforces ADR-0022: a name the operator
// did not declare is not usable) runs before the backend touches the SDK
// client. It constructs an SDKBackend with a nil client and no workspaces, so
// a client call anywhere in Create's path before the kits loop would panic
// instead of returning the expected error.
func TestSDKBackend_Create_UnknownKit(t *testing.T) {
	b := &SDKBackend{kits: map[string]string{"known": "/known"}}

	_, err := b.Create(context.Background(), CreateSpec{Name: "s1", Kits: []string{"nope"}})
	if err == nil {
		t.Fatal("create with an unknown kit: want an error, got nil")
	}
	if !strings.Contains(err.Error(), `unknown kit "nope"`) {
		t.Fatalf("want an unknown-kit error, got %v", err)
	}
}

func TestHasVolumes(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"absent field is no volumes", nil, false},
		{"explicit null is no volumes", []byte("null"), false},
		{"empty object is no volumes", []byte("{}"), false},
		{"empty object with a space is no volumes", []byte("{ }"), false},
		{"empty list is no volumes", []byte("[]"), false},
		{"a populated list is volumes", []byte(`[{"host":"/x"}]`), true},
		{"unparseable garbage is volumes", []byte(`[{"host":`), true}, // fail closed
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasVolumes(tc.raw); got != tc.want {
				t.Fatalf("hasVolumes(%s): want %v, got %v", tc.raw, tc.want, got)
			}
		})
	}
}
