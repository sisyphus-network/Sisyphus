package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
)

// far is the far end of a tunnel: a service that hands each call to handle.
type far struct {
	pb.UnimplementedTunnelServiceServer
	handle func(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error
}

func (f far) Open(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	return f.handle(stream)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// link is a tunnel set up for a test: near is the address to connect to on
// this side, and what arrives goes to the far side's handler.
type link struct {
	near   string
	logs   *logBuffer
	client *grpc.ClientConn
	stop   context.CancelFunc
	// stopped is closed once the near side has stopped accepting.
	stopped chan struct{}
}

func listen(t *testing.T) *net.TCPListener {
	t.Helper()
	lis, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return lis
}

func newLink(t *testing.T, handle func(grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error) *link {
	t.Helper()
	serverLis := listen(t)
	srv := grpc.NewServer()
	pb.RegisterTunnelServiceServer(srv, far{handle: handle})
	go srv.Serve(serverLis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(serverLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	near := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l := &link{near: near.Addr().String(), logs: new(logBuffer), client: conn, stop: cancel, stopped: make(chan struct{})}
	go func() {
		defer close(l.stopped)
		Forward(ctx, near, pb.NewTunnelServiceClient(conn), pb.TunnelTarget_TUNNEL_TARGET_SWARM, slog.New(slog.NewTextHandler(l.logs, nil)))
	}()
	return l
}

func (l *link) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", l.near)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	return conn
}

// toService returns a far-end handler that carries each tunnel to the TCP
// service at addr, checking that the caller named its target first.
func toService(t *testing.T, addr string) func(grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	return func(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
		first, err := stream.Recv()
		if err != nil || first.GetTarget() != pb.TunnelTarget_TUNNEL_TARGET_SWARM {
			t.Errorf("first message %v, %v; want the target named", first, err)
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return err
		}
		return Serve(conn.(*net.TCPConn), stream)
	}
}

// serve runs a TCP service that hands each connection to handle.
func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	lis := listen(t)
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return lis.Addr().String()
}

func echo(conn net.Conn) {
	defer conn.Close()
	io.Copy(conn, conn)
}

func TestBytesGoThroughATunnelBothWaysUnchanged(t *testing.T) {
	l := newLink(t, toService(t, serve(t, echo)))

	// Several connections at once, each with more than fits in one message.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sent := make([]byte, 3<<20)
			rand.Read(sent)
			conn := l.dial(t)
			go conn.Write(sent)
			got := make([]byte, len(sent))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Errorf("reading the echo: %v", err)
				return
			}
			if !bytes.Equal(got, sent) {
				t.Error("what came back through the tunnel is not what went in")
			}
		}()
	}
	wg.Wait()
	if logged := l.logs.String(); logged != "" {
		t.Errorf("a working tunnel logged:\n%s", logged)
	}
}

func TestClosingEitherEndClosesTheOther(t *testing.T) {
	// The service says its piece and hangs up.
	l := newLink(t, toService(t, serve(t, func(conn net.Conn) {
		conn.Write([]byte("goodbye"))
		conn.Close()
	})))
	got, err := io.ReadAll(l.dial(t))
	if err != nil || string(got) != "goodbye" {
		t.Errorf("read %q, %v; want the service's last words and then the end", got, err)
	}

	// The caller says its piece and hangs up. The service gets all of it.
	heard := make(chan string, 1)
	l = newLink(t, toService(t, serve(t, func(conn net.Conn) {
		defer conn.Close()
		all, _ := io.ReadAll(conn)
		heard <- string(all)
	})))
	conn := l.dial(t)
	conn.Write([]byte("hello"))
	conn.Close()
	select {
	case got := <-heard:
		if got != "hello" {
			t.Errorf("the service heard %q before the caller hung up, want all of it", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the service was never told that the caller had gone")
	}

	// A caller that has finished speaking can still be answered.
	l = newLink(t, toService(t, serve(t, func(conn net.Conn) {
		defer conn.Close()
		all, _ := io.ReadAll(conn)
		conn.Write([]byte("you said: " + string(all)))
	})))
	asker := l.dial(t).(*net.TCPConn)
	asker.Write([]byte("anyone there?"))
	asker.CloseWrite()
	if answer, err := io.ReadAll(asker); err != nil || string(answer) != "you said: anyone there?" {
		t.Errorf("the answer to a caller that had finished speaking: %q, %v", answer, err)
	}
	if logged := l.logs.String(); logged != "" {
		t.Errorf("tunnels closed in good order logged:\n%s", logged)
	}
}

func TestATunnelTheFarEndRefusesIsClosedAndReported(t *testing.T) {
	l := newLink(t, func(grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
		return status.Error(codes.FailedPrecondition, "nothing here to tunnel to")
	})
	if got, err := io.ReadAll(l.dial(t)); len(got) != 0 {
		t.Errorf("read %q, %v from a refused tunnel", got, err)
	}
	waitForLog(t, l.logs, "a tunnel failed")
	if !strings.Contains(l.logs.String(), "nothing here to tunnel to") {
		t.Errorf("the refusal's reason was not logged:\n%s", l.logs)
	}
}

func TestATunnelThatCannotBeOpenedIsClosedAndReported(t *testing.T) {
	l := newLink(t, toService(t, serve(t, echo)))
	l.client.Close()
	if got, _ := io.ReadAll(l.dial(t)); len(got) != 0 {
		t.Errorf("read %q through a tunnel that could not be opened", got)
	}
	waitForLog(t, l.logs, "could not open a tunnel")
}

func TestStoppingClosesTheListenerAndWhatIsInFlight(t *testing.T) {
	l := newLink(t, toService(t, serve(t, echo)))
	conn := l.dial(t)
	conn.Write([]byte("ping"))
	if _, err := io.ReadFull(conn, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}

	l.stop()
	<-l.stopped
	if _, err := net.Dial("tcp", l.near); err == nil {
		t.Error("the near end still accepts connections after being stopped")
	}
	if got, _ := io.ReadAll(conn); len(got) != 0 {
		t.Errorf("read %q from a connection whose tunnel was stopped", got)
	}
	// Being stopped is not a failure.
	if logged := l.logs.String(); logged != "" {
		t.Errorf("stopping logged:\n%s", logged)
	}
}

func waitForLog(t *testing.T, logs *logBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never logged %q; logged:\n%s", want, logs)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// deaf is a stream that cannot be sent on and never has anything to give.
type deaf struct{ release chan struct{} }

var errDeaf = errors.New("nobody is listening")

func (deaf) Send(*pb.TunnelData) error { return errDeaf }

func (deaf) CloseSend() error { return nil }

func (d deaf) Recv() (*pb.TunnelData, error) {
	<-d.release
	return nil, io.EOF
}

func TestATunnelStopsWhenItsStreamCannotBeSentOn(t *testing.T) {
	lis := listen(t)
	defer lis.Close()
	ours, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close()
	theirs, err := lis.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	stream := deaf{release: make(chan struct{})}
	defer close(stream.release)
	ours.Write([]byte("anyone?"))
	if err := call(theirs, stream); !errors.Is(err, errDeaf) {
		t.Errorf("error %v, want the failure to send", err)
	}
	// And the connection is closed.
	ours.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := ours.Read(make([]byte, 1)); err == nil {
		t.Error("the connection is still open after its tunnel gave up")
	}
}

// sink is a service that reads and discards whatever it is sent.
func sink(conn net.Conn) {
	defer conn.Close()
	io.Copy(io.Discard, conn)
}

// throughput writes b.N megabytes to addr and waits for them to be taken.
func throughput(b *testing.B, addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		if _, err := conn.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
	conn.Close()
}

// The two benchmarks send the same bytes to the same service on this
// machine, straight there and through a tunnel, to show what the tunnel
// itself costs. Run them with: go test -bench . ./apps/sisyphusd/tunnel
func BenchmarkDirect(b *testing.B) {
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	defer lis.Close()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go sink(conn)
		}
	}()
	throughput(b, lis.Addr().String())
}

func BenchmarkTunnel(b *testing.B) {
	service, _ := net.Listen("tcp", "127.0.0.1:0")
	defer service.Close()
	go func() {
		for {
			conn, err := service.Accept()
			if err != nil {
				return
			}
			go sink(conn)
		}
	}()
	serverLis, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer()
	pb.RegisterTunnelServiceServer(srv, far{handle: func(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
		stream.Recv()
		conn, err := net.Dial("tcp", service.Addr().String())
		if err != nil {
			return err
		}
		return Serve(conn.(*net.TCPConn), stream)
	}})
	go srv.Serve(serverLis)
	defer srv.Stop()
	client, _ := grpc.NewClient(serverLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer client.Close()
	near, _ := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Forward(ctx, near, pb.NewTunnelServiceClient(client), pb.TunnelTarget_TUNNEL_TARGET_SWARM, slog.New(slog.DiscardHandler))
	throughput(b, near.Addr().String())
}
