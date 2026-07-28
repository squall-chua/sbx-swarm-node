package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
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
