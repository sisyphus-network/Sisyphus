package blobclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"

	"github.com/excho0/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

var (
	ctx     = context.Background()
	errNode = errors.New("node went away")
)

// fakeNode is a blob service client whose every step can be made to fail,
// standing in for a node that misbehaves or drops off mid-transfer.
type fakeNode struct {
	pb.BlobServiceClient

	// Uploads.
	putErr      error  // returned when the upload is started
	sendErr     error  // returned by every Send
	closeErr    error  // returned in place of the node's answer
	answerCID   string // the CID the node claims to have stored
	uploaded    bytes.Buffer
	uploadCalls int

	// Downloads.
	getErr    error    // returned when the download is started
	chunks    [][]byte // sent in order
	streamErr error    // returned after the chunks instead of a clean end
}

func (f *fakeNode) Put(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[pb.PutBlobRequest, pb.PutBlobResponse], error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	return &fakeUpload{node: f}, nil
}

type fakeUpload struct {
	grpc.ClientStream
	node *fakeNode
}

func (u *fakeUpload) Send(req *pb.PutBlobRequest) error {
	u.node.uploadCalls++
	if u.node.sendErr != nil {
		return u.node.sendErr
	}
	u.node.uploaded.Write(req.GetData())
	return nil
}

func (u *fakeUpload) CloseAndRecv() (*pb.PutBlobResponse, error) {
	if u.node.closeErr != nil {
		return nil, u.node.closeErr
	}
	return &pb.PutBlobResponse{Cid: u.node.answerCID}, nil
}

func (f *fakeNode) Get(context.Context, *pb.GetBlobRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.GetBlobResponse], error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &fakeDownload{chunks: f.chunks, err: f.streamErr}, nil
}

type fakeDownload struct {
	grpc.ClientStream
	chunks [][]byte
	err    error
}

func (d *fakeDownload) Recv() (*pb.GetBlobResponse, error) {
	if len(d.chunks) > 0 {
		chunk := d.chunks[0]
		d.chunks = d.chunks[1:]
		return &pb.GetBlobResponse{Data: chunk}, nil
	}
	if d.err != nil {
		return nil, d.err
	}
	return nil, io.EOF
}

func cidOf(t *testing.T, data []byte) cid.Cid {
	t.Helper()
	c, err := storage.CID(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cancelled() context.Context {
	c, cancel := context.WithCancel(ctx)
	cancel()
	return c
}

// big spans several upload messages; small fits in one.
var (
	big   = bytes.Repeat([]byte("push the boulder up the hill. "), 40_000)
	small = []byte("a short blob")
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errNode }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errNode }

func TestUploadSendsEverythingAndReturnsItsCID(t *testing.T) {
	node := &fakeNode{answerCID: cidOf(t, big).String()}
	got, err := blobclient.Upload(ctx, node, bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equals(cidOf(t, big)) {
		t.Errorf("returned %s, want %s", got, cidOf(t, big))
	}
	if !bytes.Equal(node.uploaded.Bytes(), big) {
		t.Errorf("node received %d bytes that differ from the %d sent", node.uploaded.Len(), len(big))
	}
	if node.uploadCalls < 2 {
		t.Errorf("a %d-byte blob went in %d message(s), want it split", len(big), node.uploadCalls)
	}
}

func TestUploadFailures(t *testing.T) {
	right := cidOf(t, small).String()
	tests := []struct {
		name string
		ctx  context.Context
		node *fakeNode
		src  io.Reader
		want string
	}{
		{"the upload cannot start", ctx, &fakeNode{putErr: errNode}, bytes.NewReader(small), "node went away"},
		{"the source fails", ctx, &fakeNode{answerCID: right}, io.MultiReader(bytes.NewReader(small), failingReader{}), "node went away"},
		{"the node ends the upload early", ctx, &fakeNode{sendErr: io.EOF, closeErr: errNode}, bytes.NewReader(big), "node went away"},
		{"the node gives no answer", ctx, &fakeNode{closeErr: errNode}, bytes.NewReader(small), "node went away"},
		{"the node answers with another CID", ctx, &fakeNode{answerCID: cidOf(t, big).String()}, bytes.NewReader(small), "the bytes sent hash to " + right},
		{"cancelled during a large upload", cancelled(), &fakeNode{answerCID: right}, bytes.NewReader(big), "context canceled"},
		{"cancelled during a small upload", cancelled(), &fakeNode{answerCID: right}, bytes.NewReader(small), "context canceled"},
	}
	for _, tt := range tests {
		_, err := blobclient.Upload(tt.ctx, tt.node, tt.src)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestUploadStopsSendingOnceTheNodeEndsTheUpload(t *testing.T) {
	node := &fakeNode{sendErr: io.EOF, closeErr: errNode}
	blobclient.Upload(ctx, node, bytes.NewReader(big))
	if node.uploadCalls != 1 {
		t.Errorf("sent %d messages after the node ended the upload, want 1", node.uploadCalls)
	}
}

func TestDownloadWritesTheBlobAfterCheckingIt(t *testing.T) {
	node := &fakeNode{chunks: [][]byte{big[:300_000], big[300_000:]}}
	var got bytes.Buffer
	if err := blobclient.Download(ctx, node, cidOf(t, big), &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), big) {
		t.Errorf("wrote %d bytes that differ from the %d sent", got.Len(), len(big))
	}
}

func TestDownloadFailures(t *testing.T) {
	want := cidOf(t, small)
	tests := []struct {
		name string
		ctx  context.Context
		node *fakeNode
		dst  io.Writer
		want string
	}{
		{"the download cannot start", ctx, &fakeNode{getErr: errNode}, io.Discard, "node went away"},
		{"the node stops part-way", ctx, &fakeNode{chunks: [][]byte{small[:4]}, streamErr: errNode}, io.Discard, "node went away"},
		{"the destination fails", ctx, &fakeNode{chunks: [][]byte{small}}, failingWriter{}, "node went away"},
		{"the bytes are not the blob asked for", ctx, &fakeNode{chunks: [][]byte{[]byte("something else")}}, io.Discard, "not the " + want.String() + " asked for"},
		{"cancelled", cancelled(), &fakeNode{chunks: [][]byte{small}}, io.Discard, "context canceled"},
	}
	for _, tt := range tests {
		err := blobclient.Download(tt.ctx, tt.node, want, tt.dst)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestFetchStoresTheBlobAfterCheckingIt(t *testing.T) {
	store := storage.NewMemory()
	node := &fakeNode{chunks: [][]byte{big[:300_000], big[300_000:]}}
	want := cidOf(t, big)
	if err := blobclient.Fetch(ctx, node, want, store); err != nil {
		t.Fatal(err)
	}
	blob, err := store.Open(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	if got, _ := io.ReadAll(blob); !bytes.Equal(got, big) {
		t.Error("the stored blob differs from what the node sent")
	}
}

func TestFetchFailures(t *testing.T) {
	want := cidOf(t, small)
	tests := []struct {
		name string
		node *fakeNode
		want string
	}{
		{"the download cannot start", &fakeNode{getErr: errNode}, "node went away"},
		{"the node stops part-way", &fakeNode{chunks: [][]byte{small[:4]}, streamErr: errNode}, "node went away"},
		{"the bytes are not the blob asked for", &fakeNode{chunks: [][]byte{[]byte("something else")}}, "not the " + want.String() + " asked for"},
	}
	for _, tt := range tests {
		store := storage.NewMemory()
		err := blobclient.Fetch(ctx, tt.node, want, store)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
		if has, _ := store.Has(ctx, want); has {
			t.Errorf("%s: the store holds the requested CID after a failed fetch", tt.name)
		}
	}
}
