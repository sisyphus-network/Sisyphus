package main

import (
	"errors"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

type fakeLocationDiscoveryStore struct {
	settings   nodedb.LocationDiscovery
	configured bool
	err        error
}

func (s fakeLocationDiscoveryStore) LocationDiscovery() (nodedb.LocationDiscovery, bool, error) {
	return s.settings, s.configured, s.err
}

func TestSupportsGatewayDiscovery(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows", "freebsd", "openbsd", "netbsd", "dragonfly"} {
		if !supportsGatewayDiscovery(goos) {
			t.Errorf("%s should support gateway discovery", goos)
		}
	}
	for _, goos := range []string{"aix", "android", "plan9", "solaris", "unknown"} {
		if supportsGatewayDiscovery(goos) {
			t.Errorf("%s should not support gateway discovery", goos)
		}
	}
}

func TestStartupLocationDiscovery(t *testing.T) {
	defaults := nodedb.LocationDiscovery{LocateGateway: true, LocateCountry: true, StunServer: "default"}
	saved := nodedb.LocationDiscovery{LocateGateway: false, LocateCountry: false, StunServer: "saved"}

	t.Run("uses defaults when not configured", func(t *testing.T) {
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{}, defaults, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != defaults {
			t.Fatalf("settings = %#v, want defaults %#v", got, defaults)
		}
	})

	t.Run("uses saved settings when configured", func(t *testing.T) {
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{settings: saved, configured: true}, defaults, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != saved {
			t.Fatalf("settings = %#v, want saved %#v", got, saved)
		}
	})

	t.Run("disables unsupported gateway discovery", func(t *testing.T) {
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{settings: defaults, configured: true}, defaults, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.LocateGateway {
			t.Fatal("LocateGateway = true, want false on unsupported platform")
		}
		if !got.LocateCountry || got.StunServer != defaults.StunServer {
			t.Fatalf("other settings were changed: %#v", got)
		}
	})

	t.Run("returns storage errors", func(t *testing.T) {
		wantErr := errors.New("database unavailable")
		if _, err := startupLocationDiscovery(fakeLocationDiscoveryStore{err: wantErr}, defaults, true); !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})
}
