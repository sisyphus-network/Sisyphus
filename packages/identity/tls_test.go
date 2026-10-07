package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// handshake connects client to server over loopback and returns what each
// side concluded: the ID the server saw, and either side's error.
func handshake(t *testing.T, server, client *tls.Config) (seenByServer string, serverErr, clientErr error) {
	t.Helper()
	lis, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	type result struct {
		id  string
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			accepted <- result{err: err}
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := tlsConn.Handshake(); err != nil {
			accepted <- result{err: err}
			return
		}
		id, err := PeerID(tlsConn.ConnectionState())
		// Let the client finish reading the handshake before closing.
		tlsConn.Read(make([]byte, 1))
		accepted <- result{id: id, err: err}
	}()

	conn, clientErr := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", lis.Addr().String(), client)
	if clientErr == nil {
		conn.Close()
	}
	got := <-accepted
	return got.id, got.err, clientErr
}

func TestNodesIdentifyEachOtherOverTLS(t *testing.T) {
	server, _ := create(t)
	client, _ := create(t)

	seen, serverErr, clientErr := handshake(t, server.ServerTLS(), client.ClientTLS(server.ID()))
	if serverErr != nil || clientErr != nil {
		t.Fatalf("handshake failed: server %v, client %v", serverErr, clientErr)
	}
	if seen != client.ID() {
		t.Errorf("the server took the client for %s, it is %s", seen, client.ID())
	}
}

func TestClientRefusesAServerThatIsNotTheNodeItExpected(t *testing.T) {
	server, _ := create(t)
	impostor, _ := create(t)
	client, _ := create(t)

	_, _, clientErr := handshake(t, impostor.ServerTLS(), client.ClientTLS(server.ID()))
	if clientErr == nil || !strings.Contains(clientErr.Error(), "connected to node "+impostor.ID()+", expected "+server.ID()) {
		t.Errorf("client error %v, want it to name both nodes", clientErr)
	}
}

// foreignCertificate is a certificate with a key of a kind nodes do not use.
func foreignCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "not a node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestPeersWithoutANodeKeyAreNotGivenAnID(t *testing.T) {
	server, _ := create(t)
	client, _ := create(t)
	foreign := foreignCertificate(t)

	// A client with some other kind of certificate completes the handshake,
	// since the server accepts any, but cannot be identified.
	_, serverErr, _ := handshake(t, server.ServerTLS(), &tls.Config{Certificates: []tls.Certificate{foreign}, InsecureSkipVerify: true})
	if serverErr == nil || !strings.Contains(serverErr.Error(), "not a node key") {
		t.Errorf("server error %v, want the client's certificate refused as not a node key", serverErr)
	}

	// A client refuses a server with one.
	_, _, clientErr := handshake(t, &tls.Config{Certificates: []tls.Certificate{foreign}}, client.ClientTLS(server.ID()))
	if clientErr == nil || !strings.Contains(clientErr.Error(), "not a node key") {
		t.Errorf("client error %v, want the server's certificate refused as not a node key", clientErr)
	}

	if _, err := PeerID(tls.ConnectionState{}); err == nil {
		t.Error("PeerID gave an ID for a connection with no certificate")
	}
}

func TestServerInsistsOnAClientCertificate(t *testing.T) {
	server, _ := create(t)
	_, serverErr, _ := handshake(t, server.ServerTLS(), &tls.Config{InsecureSkipVerify: true})
	if serverErr == nil {
		t.Error("the server accepted a client that presented no certificate")
	}
}

func TestOnlyTLS13IsSpoken(t *testing.T) {
	server, _ := create(t)
	client, _ := create(t)
	old := client.ClientTLS(server.ID())
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, _, clientErr := handshake(t, server.ServerTLS(), old); clientErr == nil {
		t.Error("a TLS 1.2 handshake succeeded")
	}
}
