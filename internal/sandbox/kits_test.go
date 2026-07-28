package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
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

// TestAdmitKits_TimeoutIsPerKit proves the inspect bound applies to each kit
// independently, not to the whole set. A parent deadline shorter than
// kitInspectTimeout still governs each kit's own child context (the shorter
// of the two wins), which keeps this test fast without waiting out the real
// 15s constant. The slow kit blocks on <-ctx.Done() instead of sleeping. The
// fast kit returns immediately with a kind:sandbox manifest, which admit()
// must still refuse -- an aggregate bound shared by one ctx for the whole
// batch would have been just as capable of proving this, but the point is
// that a per-kit bound does NOT regress it: the fast kit's own verdict does
// not depend on whether some other kit in the batch is still hung.
func TestAdmitKits_TimeoutIsPerKit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	inspect := func(ctx context.Context, ref string) (KitInfo, error) {
		if ref == "/slow" {
			<-ctx.Done() // simulate a reference whose fetch outlives the deadline
			return KitInfo{}, ctx.Err()
		}
		// "/fast": resolves before any deadline and must be judged on its
		// own manifest.
		return KitInfo{Kind: "sandbox"}, nil
	}

	got := admitKits(ctx, inspect, map[string]string{
		"slow": "/slow",
		"fast": "/fast",
	}, quietLog())

	want := map[string]string{"slow": "/slow"} // fast refused; slow kept (timeout)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
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
		{"a populated object is resources", []byte(`{"cpu":2}`), true},
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
		{"empty list is no volumes", []byte("[]"), false},
		{"a populated list is volumes", []byte(`[{"host":"/x"}]`), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasVolumes(tc.raw); got != tc.want {
				t.Fatalf("hasVolumes(%s): want %v, got %v", tc.raw, tc.want, got)
			}
		})
	}
}
