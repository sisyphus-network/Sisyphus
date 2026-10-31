package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// requestActionApproval forwards MCP initialize information as reported
// context only. The local API deliberately records it as unverified.
func (s *server) requestActionApproval(ctx context.Context, req *mcp.CallToolRequest, action string) (*nodepb.RequestActionConfirmationResponse, error) {
	var name, version string
	if req != nil && req.Session != nil {
		if initialized := req.Session.InitializeParams(); initialized != nil && initialized.ClientInfo != nil {
			name, version = initialized.ClientInfo.Name, initialized.ClientInfo.Version
		}
	}
	return s.Node.RequestActionConfirmation(s.as(ctx), &nodepb.RequestActionConfirmationRequest{
		Action: action, RequesterClientName: name, RequesterClientVersion: version,
	})
}
