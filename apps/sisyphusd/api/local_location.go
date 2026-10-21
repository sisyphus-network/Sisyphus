package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/geo"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// OwnerLocationStore keeps where the node's owner says the node is.
type OwnerLocationStore interface {
	OwnerLocation() (nodedb.OwnerLocation, bool, error)
	SetOwnerLocation(nodedb.OwnerLocation) error
	ClearOwnerLocation() error
}

// locationShares are the shares the API speaks of, as the node saves them.
// An unspecified share is saved as country, the privacy default.
var locationShares = map[nodepb.LocationShare]geo.Share{
	nodepb.LocationShare_LOCATION_SHARE_UNSPECIFIED: geo.ShareCountry,
	nodepb.LocationShare_LOCATION_SHARE_NOTHING:     geo.ShareNothing,
	nodepb.LocationShare_LOCATION_SHARE_COUNTRY:     geo.ShareCountry,
	nodepb.LocationShare_LOCATION_SHARE_CITY:        geo.ShareCity,
	nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM:   geo.ShareAbout1km,
	nodepb.LocationShare_LOCATION_SHARE_EXACT:       geo.ShareExact,
}

// GetNodeLocation returns where the owner says this node is. The token is
// required because the answer can be where the owner lives.
func (s *localService) GetNodeLocation(ctx context.Context, _ *nodepb.GetNodeLocationRequest) (*nodepb.GetNodeLocationResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.OwnerLocation == nil {
		return nil, status.Error(codes.FailedPrecondition, "the node's location is unavailable")
	}
	location, err := s.ownerLocation()
	if err != nil {
		return nil, asError(err)
	}
	return &nodepb.GetNodeLocationResponse{Location: location}, nil
}

// SetNodeLocation saves where the owner says this node is, or forgets it
// when no location is given. It takes effect at once: everyone watching the
// node's state is told.
func (s *localService) SetNodeLocation(ctx context.Context, req *nodepb.SetNodeLocationRequest) (*nodepb.GetNodeLocationResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.OwnerLocation == nil {
		return nil, status.Error(codes.FailedPrecondition, "the node's location is unavailable")
	}
	given := req.GetLocation()
	if given == nil {
		if err := s.cfg.OwnerLocation.ClearOwnerLocation(); err != nil {
			return nil, asError(err)
		}
		s.nodeInfoChanged()
		return &nodepb.GetNodeLocationResponse{}, nil
	}
	if err := geo.CheckCoordinates(given.GetLatitude(), given.GetLongitude()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	share, known := locationShares[given.GetShare()]
	if !known {
		return nil, status.Errorf(codes.InvalidArgument, "unknown location share %d", given.GetShare())
	}
	location := nodedb.OwnerLocation{Latitude: given.GetLatitude(), Longitude: given.GetLongitude(), Share: string(share)}
	if err := s.cfg.OwnerLocation.SetOwnerLocation(location); err != nil {
		return nil, asError(err)
	}
	s.nodeInfoChanged()
	return &nodepb.GetNodeLocationResponse{Location: nodeLocation(location)}, nil
}

// ownerLocation returns the saved location as the API gives it, or nil if
// there is none.
func (s *localService) ownerLocation() (*nodepb.NodeLocation, error) {
	location, set, err := s.cfg.OwnerLocation.OwnerLocation()
	if err != nil || !set {
		return nil, err
	}
	return nodeLocation(location), nil
}

func nodeLocation(location nodedb.OwnerLocation) *nodepb.NodeLocation {
	// A share the node does not know is read as country, the default.
	share := nodepb.LocationShare_LOCATION_SHARE_COUNTRY
	for kind, saved := range locationShares {
		if kind != nodepb.LocationShare_LOCATION_SHARE_UNSPECIFIED && string(saved) == location.Share {
			share = kind
		}
	}
	return &nodepb.NodeLocation{Latitude: location.Latitude, Longitude: location.Longitude, Share: share}
}

func (s *localService) nodeInfoChanged() {
	if s.cfg.NodeInfoChanges != nil {
		s.cfg.NodeInfoChanges.Notify()
	}
}
