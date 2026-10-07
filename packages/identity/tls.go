package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Nodes secure their connections with TLS 1.3, each presenting a certificate
// it signed itself with its node key. No certificate authority is involved:
// completing the handshake proves the other side holds the private key for
// the certificate it showed, and the ID derived from that key says who it
// is. Whether that node is welcome is decided separately, by ID.

// certificate returns a self-signed certificate for the node's key.
func (i *Identity) certificate() tls.Certificate {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: i.ID()},
		// The key is the identity, so the certificate never needs renewing.
		NotBefore: time.Unix(0, 0),
		NotAfter:  time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, _ := x509.CreateCertificate(rand.Reader, template, template, i.key.Public(), i.key) // cannot fail for these inputs
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: i.key}
}

// ServerTLS returns the TLS settings for accepting connections as this node.
// Every client must present a certificate, but any node's is accepted: the
// server learns who is calling from PeerID and decides what they may do.
func (i *Identity) ServerTLS() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{i.certificate()},
		ClientAuth:   tls.RequireAnyClientCert,
	}
}

// ClientTLS returns the TLS settings for connecting, as this node, to the
// node with ID serverID. The handshake fails if anyone else answers.
func (i *Identity) ClientTLS(serverID string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{i.certificate()},
		// Certificates are self-signed, so the usual chain and host name
		// checks do not apply. The check that matters is made below.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			got, err := PeerID(state)
			if err != nil {
				return err
			}
			if got != serverID {
				return fmt.Errorf("connected to node %s, expected %s", got, serverID)
			}
			return nil
		},
	}
}

// PeerID returns the ID of the node at the other end of a TLS connection.
func PeerID(state tls.ConnectionState) (string, error) {
	if len(state.PeerCertificates) == 0 {
		return "", errors.New("the other side presented no certificate")
	}
	key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("the other side's certificate holds a %T, not a node key", state.PeerCertificates[0].PublicKey)
	}
	public, _ := crypto.UnmarshalEd25519PublicKey(key) // cannot fail for a key of the right length, which TLS has checked
	id, _ := peer.IDFromPublicKey(public)
	return id.String(), nil
}
