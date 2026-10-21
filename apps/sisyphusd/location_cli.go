package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/sisyphus-network/Sisyphus/packages/geo"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// locationCommand shows, sets and clears where the owner says the node is.
// Like modelCommand it works on the node's database directly, so the node
// need not be running; one that is gives the new location the next time
// it is asked for its info.
func locationCommand(args []string) error {
	if len(args) == 0 {
		return errLocationCommand
	}
	fs := flag.NewFlagSet("sisyphusd location "+args[0], flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's data")
	lat := fs.Float64("lat", 0, "set: the latitude in degrees, from -90 to 90, north positive")
	lon := fs.Float64("lon", 0, "set: the longitude in degrees, from -180 to 180, east positive")
	share := fs.String("share", string(geo.ShareCountry), "set: how much of it other members of a pool may see: exact, about-1km, city, country or nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var location nodedb.OwnerLocation
	switch args[0] {
	case "show", "clear":
	case "set":
		if !given["lat"] || !given["lon"] {
			return errors.New("give the location as --lat and --lon")
		}
		if err := geo.CheckCoordinates(*lat, *lon); err != nil {
			return err
		}
		switch geo.Share(*share) {
		case geo.ShareExact, geo.ShareAbout1km, geo.ShareCity, geo.ShareCountry, geo.ShareNothing:
		default:
			return fmt.Errorf("unknown share %q: expected exact, about-1km, city, country or nothing", *share)
		}
		location = nodedb.OwnerLocation{Latitude: *lat, Longitude: *lon, Share: *share}
	default:
		return errLocationCommand
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	db, err := nodedb.Open(filepath.Join(*dataDir, "node.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "set":
		if err := db.SetOwnerLocation(location); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "this node is at %s, %s; nothing is sent to the pool yet\n", degrees(location.Latitude), degrees(location.Longitude))
		return nil
	case "clear":
		if err := db.ClearOwnerLocation(); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "this node's location is forgotten")
		return nil
	}
	saved, set, err := db.OwnerLocation()
	if err != nil {
		return err
	}
	if !set {
		fmt.Fprintln(stdout, "no location has been set")
		return nil
	}
	fmt.Fprintf(stdout, "latitude:  %s\nlongitude: %s\nshare:     %s\n", degrees(saved.Latitude), degrees(saved.Longitude), saved.Share)
	return nil
}

var errLocationCommand = errors.New("expected location show, set or clear")

func degrees(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
