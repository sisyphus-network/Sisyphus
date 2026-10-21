package geo

import (
	"errors"
	"math"
)

// Share is how much of a node's location its owner lets other members of
// a pool see. The node itself always knows the location as it was given.
type Share string

// The shares, from the most an owner can give away to the least. Country
// is the default, so that a stranger in a pool does not see where an owner
// lives unless the owner says so.
const (
	ShareExact    Share = "exact"
	ShareAbout1km Share = "about-1km"
	ShareCity     Share = "city"
	ShareCountry  Share = "country"
	ShareNothing  Share = "nothing"
)

// CheckCoordinates says whether a latitude and a longitude, in degrees,
// are a place on the Earth: finite, the latitude in [-90, 90] and the
// longitude in [-180, 180].
func CheckCoordinates(latitude, longitude float64) error {
	if math.IsNaN(latitude) || math.IsInf(latitude, 0) || latitude < -90 || latitude > 90 {
		return errors.New("latitude must be a number of degrees from -90 to 90")
	}
	if math.IsNaN(longitude) || math.IsInf(longitude, 0) || longitude < -180 || longitude > 180 {
		return errors.New("longitude must be a number of degrees from -180 to 180")
	}
	return nil
}

// Shared returns as much of a location as share lets others see: the
// coordinates unchanged for exact, rounded to 0.01° (about a kilometre)
// for about-1km, rounded to 0.1° for city, and no coordinates at all,
// with ok false, for country, nothing or a share it does not know. A
// coordinate that rounds to zero is positive zero.
func Shared(latitude, longitude float64, share Share) (lat, lon float64, ok bool) {
	switch share {
	case ShareExact:
		return latitude, longitude, true
	case ShareAbout1km:
		return round(latitude, 100), round(longitude, 100), true
	case ShareCity:
		return round(latitude, 10), round(longitude, 10), true
	}
	return 0, 0, false
}

func round(degrees, per float64) float64 {
	rounded := math.Round(degrees*per) / per
	if rounded == 0 {
		return 0 // and not -0, which would print as "-0"
	}
	return rounded
}
