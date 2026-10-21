package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/geo/citytable"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// CityTableStore keeps the city table the owner may download: DB-IP's free
// "IP to City Lite", by which the node finds the city it is in.
type CityTableStore interface {
	Status() (citytable.Status, error)
	Download(context.Context) (citytable.Status, error)
	Remove() error
}

var errNoCityTable = status.Error(codes.FailedPrecondition, "this node keeps no city table")

// GetCityTable says whether the city table is there. Anything on the
// machine may ask.
func (s *localService) GetCityTable(context.Context, *nodepb.GetCityTableRequest) (*nodepb.CityTableState, error) {
	if s.cfg.CityTable == nil {
		return nil, errNoCityTable
	}
	return s.cityTableState(s.cfg.CityTable.Status())
}

// DownloadCityTable fetches the city table, in place of any there was, and
// waits for it. It shows db-ip.com this machine's address, so it is done
// only when asked, and only for a caller with the token.
func (s *localService) DownloadCityTable(ctx context.Context, _ *nodepb.DownloadCityTableRequest) (*nodepb.CityTableState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.CityTable == nil {
		return nil, errNoCityTable
	}
	got, err := s.cfg.CityTable.Download(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	s.locationChanged()
	return s.cityTableState(got, nil)
}

// RemoveCityTable deletes the city table, if it is there.
func (s *localService) RemoveCityTable(ctx context.Context, _ *nodepb.RemoveCityTableRequest) (*nodepb.CityTableState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.CityTable == nil {
		return nil, errNoCityTable
	}
	if err := s.cfg.CityTable.Remove(); err != nil {
		return nil, asError(err)
	}
	s.locationChanged()
	return s.cityTableState(citytable.Status{}, nil)
}

func (s *localService) cityTableState(got citytable.Status, err error) (*nodepb.CityTableState, error) {
	if err != nil {
		return nil, asError(err)
	}
	state := &nodepb.CityTableState{
		Installed: got.Installed, Edition: got.Edition, SizeBytes: uint64(got.Bytes),
		Attribution: citytable.Attribution, AttributionUrl: citytable.AttributionURL,
	}
	if got.Installed {
		state.DownloadedAtMs = got.Downloaded.UnixMilli()
	}
	return state, nil
}

// cityEstimate says where the city table puts the node, unless the owner
// has said where it is: their pin wins.
func (s *localService) cityEstimate() *nodepb.CityEstimate {
	if s.cfg.City == nil {
		return nil
	}
	if s.cfg.OwnerLocation != nil {
		if _, pinned, err := s.cfg.OwnerLocation.OwnerLocation(); pinned || err != nil {
			return nil
		}
	}
	place, source := s.cfg.City()
	if source == "" {
		return nil
	}
	return &nodepb.CityEstimate{
		City: place.City, Region: place.Region, CountryCode: place.Country,
		Latitude: place.Latitude, Longitude: place.Longitude, Source: source,
		Attribution: citytable.Attribution, AttributionUrl: citytable.AttributionURL,
	}
}
