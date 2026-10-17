package main

import "testing"

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
