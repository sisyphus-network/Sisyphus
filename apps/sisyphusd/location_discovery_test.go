package main

import (
	"database/sql"
	"errors"
	"flag"
	"path/filepath"
	"strings"
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
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{}, defaults, true, locationDiscoveryFlagOverrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got != defaults {
			t.Fatalf("settings = %#v, want defaults %#v", got, defaults)
		}
	})

	t.Run("uses saved settings when configured", func(t *testing.T) {
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{settings: saved, configured: true}, defaults, true, locationDiscoveryFlagOverrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got != saved {
			t.Fatalf("settings = %#v, want saved %#v", got, saved)
		}
	})

	t.Run("disables unsupported gateway discovery", func(t *testing.T) {
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{settings: defaults, configured: true}, defaults, false, locationDiscoveryFlagOverrides{})
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
		if _, err := startupLocationDiscovery(fakeLocationDiscoveryStore{err: wantErr}, defaults, true, locationDiscoveryFlagOverrides{}); !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})

	t.Run("explicit flags override saved settings", func(t *testing.T) {
		locateGateway := false
		locateCountry := true
		stunServer := ""
		got, err := startupLocationDiscovery(fakeLocationDiscoveryStore{settings: saved, configured: true}, defaults, true, locationDiscoveryFlagOverrides{
			LocateGateway: &locateGateway,
			LocateCountry: &locateCountry,
			StunServer:    &stunServer,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := nodedb.LocationDiscovery{LocateGateway: false, LocateCountry: true, StunServer: ""}
		if got != want {
			t.Fatalf("settings = %#v, want explicit overrides %#v", got, want)
		}
	})
}

func TestLocationDiscoveryFlagOverridesTrackOnlyExplicitFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	locateGateway := fs.Bool("locate-gateway", false, "")
	locateCountry := fs.Bool("locate-country", false, "")
	stunServer := fs.String("stun-server", "default", "")
	if err := fs.Parse([]string{"--locate-gateway=false", "--locate-country", "--stun-server="}); err != nil {
		t.Fatal(err)
	}

	got := locationDiscoveryFlagOverridesFrom(fs, locateGateway, locateCountry, stunServer)
	if got.LocateGateway == nil || *got.LocateGateway {
		t.Errorf("explicit false gateway flag = %v, want pointer to false", got.LocateGateway)
	}
	if got.LocateCountry == nil || !*got.LocateCountry {
		t.Errorf("explicit true country flag = %v, want pointer to true", got.LocateCountry)
	}
	if got.StunServer == nil || *got.StunServer != "" {
		t.Errorf("explicit empty STUN flag = %v, want pointer to empty string", got.StunServer)
	}

	omitted := flag.NewFlagSet("omitted", flag.ContinueOnError)
	omittedGateway := omitted.Bool("locate-gateway", false, "")
	omittedCountry := omitted.Bool("locate-country", false, "")
	omittedSTUN := omitted.String("stun-server", "default", "")
	if err := omitted.Parse(nil); err != nil {
		t.Fatal(err)
	}
	got = locationDiscoveryFlagOverridesFrom(omitted, omittedGateway, omittedCountry, omittedSTUN)
	if got.LocateGateway != nil || got.LocateCountry != nil || got.StunServer != nil {
		t.Fatalf("omitted flags = %#v, want no overrides", got)
	}
}

// A node whose saved discovery settings cannot be read does not start with
// settings its owner did not choose.
func TestADaemonWhoseDiscoverySettingsCannotBeReadDoesNotStart(t *testing.T) {
	damaged := t.TempDir()
	db, err := nodedb.Open(filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE location_discovery`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--discovery", "off", "--data-dir", damaged, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load location discovery settings") {
		t.Errorf("starting with discovery settings that cannot be read: %v", err)
	}
}
