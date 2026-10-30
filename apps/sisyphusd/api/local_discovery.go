package api

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// LocationDiscoveryStore keeps a node's consent for optional location sources.
type LocationDiscoveryStore interface {
	LocationDiscovery() (nodedb.LocationDiscovery, bool, error)
	SetLocationDiscovery(nodedb.LocationDiscovery) error
}

// GetLocationDiscoverySettings returns what the node saved and what it is
// running with. The token is required because the result reveals disclosure
// preferences and the matching setter changes them.
func (s *localService) GetLocationDiscoverySettings(ctx context.Context, _ *nodepb.GetLocationDiscoverySettingsRequest) (*nodepb.LocationDiscoverySettingsState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	return s.locationDiscoverySnapshot()
}

func (s *localService) locationDiscoverySnapshot() (*nodepb.LocationDiscoverySettingsState, error) {
	if s.cfg.LocationDiscovery == nil {
		return nil, status.Error(codes.FailedPrecondition, "location discovery settings are unavailable")
	}
	saved, configured, err := s.cfg.LocationDiscovery.LocationDiscovery()
	if err != nil {
		return nil, asError(err)
	}
	if !configured {
		saved = s.cfg.LocationDiscoveryRunning
	}
	return locationDiscoveryState(saved, s.cfg.LocationDiscoveryRunning, s.cfg.GatewayDiscoverySupported, configured), nil
}

// SetLocationDiscoverySettings saves the node's location-source consent. A
// separate daemon is not controlled by Electron's process lifecycle, so the
// owner restarts that daemon to apply the saved settings.
func (s *localService) SetLocationDiscoverySettings(ctx context.Context, req *nodepb.SetLocationDiscoverySettingsRequest) (*nodepb.LocationDiscoverySettingsState, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.LocationDiscovery == nil {
		return nil, status.Error(codes.FailedPrecondition, "location discovery settings are unavailable")
	}
	if req.GetSettings() == nil {
		return nil, status.Error(codes.InvalidArgument, "location discovery settings are required")
	}
	settings := nodedb.LocationDiscovery{
		LocateGateway: req.GetSettings().GetLocateGateway(),
		LocateCountry: req.GetSettings().GetLocateCountry(),
		StunServer:    strings.TrimSpace(req.GetSettings().GetStunServer()),
	}
	if settings.LocateGateway && !s.cfg.GatewayDiscoverySupported {
		return nil, status.Error(codes.FailedPrecondition, "gateway discovery is not supported on this daemon's operating system")
	}
	server, err := normalizeSTUNServer(settings.StunServer)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	settings.StunServer = server
	if err := s.cfg.LocationDiscovery.SetLocationDiscovery(settings); err != nil {
		return nil, asError(err)
	}
	s.notifySharedStateChanged()
	return locationDiscoveryState(settings, s.cfg.LocationDiscoveryRunning, s.cfg.GatewayDiscoverySupported, true), nil
}

func locationDiscoveryState(saved, running nodedb.LocationDiscovery, gatewaySupported, configured bool) *nodepb.LocationDiscoverySettingsState {
	effectiveSaved := saved
	if !gatewaySupported {
		effectiveSaved.LocateGateway = false
	}
	return &nodepb.LocationDiscoverySettingsState{
		Saved: &nodepb.LocationDiscoverySettings{
			LocateGateway: saved.LocateGateway,
			LocateCountry: saved.LocateCountry,
			StunServer:    saved.StunServer,
		},
		Running: &nodepb.LocationDiscoverySettings{
			LocateGateway: running.LocateGateway,
			LocateCountry: running.LocateCountry,
			StunServer:    running.StunServer,
		},
		GatewaySupported: gatewaySupported,
		RestartNeeded:    effectiveSaved != running,
		Configured:       configured,
	}
}

func normalizeSTUNServer(server string) (string, error) {
	if server == "" {
		return "", nil
	}
	if len(server) > 260 || strings.ContainsAny(server, " \t\r\n/@?#") {
		return "", errors.New("enter a STUN server as host[:port]")
	}
	if ip := net.ParseIP(server); ip != nil {
		return server, nil
	}
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		if strings.Contains(server, ":") || !validDiscoveryHostname(server) {
			return "", errors.New("enter a STUN server as host[:port]")
		}
		return server, nil
	}
	if host == "" || (!validDiscoveryHostname(host) && net.ParseIP(host) == nil) {
		return "", errors.New("enter a STUN server as host[:port]")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("enter a STUN server as host[:port]")
	}
	if net.ParseIP(host) != nil {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	return host + ":" + strconv.Itoa(port), nil
}

func validDiscoveryHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}
