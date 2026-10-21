package geo

import (
	"math"
	"testing"
)

func TestSharedGivesAwayOnlyAsMuchAsTheOwnerChose(t *testing.T) {
	for _, test := range []struct {
		name     string
		lat, lon float64
		share    Share
		wantLat  float64
		wantLon  float64
		wantOK   bool
	}{
		{"exact is unchanged", 51.50735, -0.12776, ShareExact, 51.50735, -0.12776, true},
		{"about 1 km rounds to hundredths", 51.50735, -0.12776, ShareAbout1km, 51.51, -0.13, true},
		{"city rounds to tenths", 51.50735, -0.12776, ShareCity, 51.5, -0.1, true},
		{"the southern and western hemispheres round away from zero", -33.86785, -151.20732, ShareAbout1km, -33.87, -151.21, true},
		{"and towards it", -33.86312, -151.20321, ShareCity, -33.9, -151.2, true},
		{"the north pole", 90, 0, ShareCity, 90, 0, true},
		{"the south pole", -90, 0, ShareAbout1km, -90, 0, true},
		{"near the north pole rounds onto it", 89.996, 12.3456, ShareAbout1km, 90, 12.35, true},
		{"near the south pole rounds onto it", -89.97, 0, ShareCity, -90, 0, true},
		{"the antimeridian from the east", 0, 180, ShareCity, 0, 180, true},
		{"the antimeridian from the west", 0, -180, ShareAbout1km, 0, -180, true},
		{"near the antimeridian rounds onto it", 64.2, 179.996, ShareAbout1km, 64.2, 180, true},
		{"and from the other side", -16.5, -179.97, ShareCity, -16.5, -180, true},
		{"exact keeps the poles and antimeridian", -90, -180, ShareExact, -90, -180, true},
		{"country gives no coordinates", 51.50735, -0.12776, ShareCountry, 0, 0, false},
		{"nothing gives no coordinates", 51.50735, -0.12776, ShareNothing, 0, 0, false},
		{"an unknown share gives no coordinates", 51.50735, -0.12776, Share("street"), 0, 0, false},
		{"no share gives no coordinates", 51.50735, -0.12776, "", 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			lat, lon, ok := Shared(test.lat, test.lon, test.share)
			if lat != test.wantLat || lon != test.wantLon || ok != test.wantOK {
				t.Fatalf("Shared(%v, %v, %q) = %v, %v, %v; want %v, %v, %v", test.lat, test.lon, test.share, lat, lon, ok, test.wantLat, test.wantLon, test.wantOK)
			}
		})
	}
}

// A small negative coordinate that rounds to zero is written as 0, not -0.
func TestSharedNeverGivesNegativeZero(t *testing.T) {
	for _, share := range []Share{ShareAbout1km, ShareCity} {
		lat, lon, ok := Shared(-0.004, -0.004, share)
		if !ok || lat != 0 || lon != 0 || math.Signbit(lat) || math.Signbit(lon) {
			t.Errorf("%s: Shared(-0.004, -0.004) = %v, %v, %v", share, lat, lon, ok)
		}
	}
}

func TestCheckCoordinatesAcceptsOnlyPlacesOnTheEarth(t *testing.T) {
	for _, place := range [][2]float64{{0, 0}, {90, 180}, {-90, -180}, {51.5, -0.1}, {-33.9, 151.2}} {
		if err := CheckCoordinates(place[0], place[1]); err != nil {
			t.Errorf("CheckCoordinates(%v, %v) = %v", place[0], place[1], err)
		}
	}
	for _, place := range [][2]float64{
		{90.0001, 0}, {-90.0001, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	} {
		if err := CheckCoordinates(place[0], place[1]); err == nil || err.Error() != "latitude must be a number of degrees from -90 to 90" {
			t.Errorf("CheckCoordinates(%v, %v) = %v", place[0], place[1], err)
		}
	}
	for _, place := range [][2]float64{
		{0, 180.0001}, {0, -180.0001}, {0, math.NaN()}, {0, math.Inf(1)}, {0, math.Inf(-1)},
	} {
		if err := CheckCoordinates(place[0], place[1]); err == nil || err.Error() != "longitude must be a number of degrees from -180 to 180" {
			t.Errorf("CheckCoordinates(%v, %v) = %v", place[0], place[1], err)
		}
	}
}
