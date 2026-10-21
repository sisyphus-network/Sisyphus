package coordinator

import (
	"github.com/sisyphus-network/Sisyphus/packages/geo"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// sharedLocation is what a worker said of where it is, as the coordinator
// keeps it and passes it on to the pool: nil when the worker said nothing
// or gave coordinates that are no place on the Earth. The worker rounded
// it to the precision its owner chose; the coordinator cannot know that
// precision, and passes on no more than it was given.
func sharedLocation(said *pb.SharedLocation) *pb.SharedLocation {
	if said == nil || geo.CheckCoordinates(said.GetLatitude(), said.GetLongitude()) != nil {
		return nil
	}
	return &pb.SharedLocation{Latitude: said.GetLatitude(), Longitude: said.GetLongitude()}
}
