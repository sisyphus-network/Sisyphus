package runtime

import "testing"

func requireRealImages(t *testing.T) {
	t.Helper()
	if !realImagesEnabled {
		t.Skip("real-image tests require the realimages build tag")
	}
}
