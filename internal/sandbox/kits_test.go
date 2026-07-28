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
