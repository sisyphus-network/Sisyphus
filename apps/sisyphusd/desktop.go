package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

// What follows is what a node does for its desktop client that the command
// line does with a file of the user's choosing or a restart.

// sealingKey returns the key this node seals private files and jobs with
// for its desktop client, kept in file and made the first time. It is a
// key like any made by "key new", so the command line can use the same one
// with --key-file.
func sealingKey(file string) (sealed.Key, error) {
	key, err := readKey(file)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := keyCommand([]string{"new", file}); err != nil {
		return sealed.Key{}, err
	}
	return readKey(file)
}

// joinFromDesktop returns what joins this node to another node's pool
// while it runs. The invitation is used as "pool join" uses it. Admitted
// as a worker, the node then says it will take the other's work, which is
// what starts it working for it, and its host is connected to the other's
// and the address noted, so that it is found again after a restart. Once
// connected it knows where to ask for the work, and look has it do so at
// once: it may have looked already, on saying it would take the work, and
// found nowhere to ask.
func joinFromDesktop(dataDir string, ident *identity.Identity, connect func(ctx context.Context, address string) error, book api.AddressBook, takes api.WorkFor, look func()) func(ctx context.Context, addr, invitation string) (string, access.Role, error) {
	return func(ctx context.Context, addr, invitation string) (string, access.Role, error) {
		role, known, err := joinPool(ctx, dataDir, ident, addr, invitation)
		if err != nil {
			return "", "", err
		}
		id := known[addr]
		if role != pb.Role_ROLE_WORKER {
			return id, access.Client, nil
		}
		if err := takes.Set(id, true); err != nil {
			return "", "", fmt.Errorf("joined the pool of %s, but could not record that this node takes its work: %w", id, err)
		}
		for _, at := range multiaddrs(addr) {
			full := at + "/p2p/" + id
			if err := connect(ctx, full); err != nil {
				return "", "", fmt.Errorf("joined the pool of %s, and will work for it once it is found, but could not connect to it at %s now: %w", id, addr, err)
			}
			// The node is connected whether or not that can be written down.
			book.AddBootstrapPeer(nodedb.BootstrapPeer{PeerID: id, Address: full})
		}
		look()
		return id, access.Worker, nil
	}
}
