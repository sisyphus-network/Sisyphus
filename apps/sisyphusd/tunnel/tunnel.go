// Package tunnel carries TCP connections inside gRPC streams, so that one
// node can reach a service on another through the connection the two
// already have instead of through a port opened for the purpose.
package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Stream is either end of a TunnelService.Open call.
type Stream interface {
	Send(*pb.TunnelData) error
	Recv() (*pb.TunnelData, error)
}

// A tunnel behaves as the connection it stands in for would. What one side
// writes before closing reaches the other, and a side that has finished
// writing can still be answered. It ends when the service closes its
// connection, or when either end of the stream fails.

// Serve is the far end of a tunnel: it moves bytes between the stream and a
// connection to the service the caller asked for, until the service closes
// the connection or the stream fails, and then closes the connection.
func Serve(conn *net.TCPConn, stream Stream) error {
	defer conn.Close()
	go func() {
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				// The caller has no more to say, and may yet be answered.
				conn.CloseWrite()
				return
			}
			if err != nil {
				conn.Close()
				return
			}
			// A failed write shows up as a failed read in pump.
			conn.Write(msg.GetData())
		}
	}()
	return pump(conn, stream)
}

// callerStream is the calling end of a TunnelService.Open call.
type callerStream interface {
	Stream
	CloseSend() error
}

// call is the near end of a tunnel: it moves bytes between a connection made
// to this end and the stream, and closes the connection when the tunnel
// ends. It returns nil if the tunnel ended in good order.
func call(conn *net.TCPConn, stream callerStream) error {
	defer conn.Close()
	received := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				// Nothing more is coming, so the connection has no further
				// use. Closing it ends pump, if it has not ended already.
				received <- err
				conn.Close()
				return
			}
			conn.Write(msg.GetData())
		}
	}()
	if err := pump(conn, stream); err != nil {
		return err
	}
	// Whoever connected has finished. Say so, and wait for the service to
	// finish in its turn.
	stream.CloseSend()
	if err := <-received; !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// pump sends what it reads from conn down the stream until there is no more
// to read. It fails only if the stream does.
func pump(conn net.Conn, stream Stream) error {
	buffer := make([]byte, 256<<10)
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			// The stream may keep what it is given after Send returns.
			if err := stream.Send(&pb.TunnelData{Data: append([]byte(nil), buffer[:n]...)}); err != nil {
				return err
			}
		}
		if err != nil {
			return nil
		}
	}
}

// Forward accepts connections on lis and carries each to target on the node
// that client talks to, until ctx ends. It closes lis.
func Forward(ctx context.Context, lis *net.TCPListener, client pb.TunnelServiceClient, target pb.TunnelTarget, log *slog.Logger) {
	stop := context.AfterFunc(ctx, func() { lis.Close() })
	defer stop()
	for {
		conn, err := lis.AcceptTCP()
		if err != nil {
			return
		}
		go func() {
			// Ending the call tells the far end that this one is done.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			stream, err := client.Open(ctx)
			if err != nil {
				conn.Close()
				log.Warn("could not open a tunnel", "error", err)
				return
			}
			// If this fails the stream is broken, which call finds out.
			stream.Send(&pb.TunnelData{Target: target})
			if err := call(conn, stream); err != nil && ctx.Err() == nil {
				log.Warn("a tunnel failed", "error", err)
			}
		}()
	}
}
