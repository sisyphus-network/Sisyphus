package replication

import (
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func receiveFrames(frames ...*pb.ReplicationFrame) func() (*pb.ReplicationFrame, error) {
	index := 0
	return func() (*pb.ReplicationFrame, error) {
		if index == len(frames) {
			return nil, io.EOF
		}
		frame := frames[index]
		index++
		return frame, nil
	}
}

func receiveHellos(frames ...*pb.ReplicationHello) func() (*pb.ReplicationHello, error) {
	index := 0
	return func() (*pb.ReplicationHello, error) {
		if index == len(frames) {
			return nil, io.EOF
		}
		frame := frames[index]
		index++
		return frame, nil
	}
}

func TestReplicationHelloIsReassembledFromBoundedFrames(t *testing.T) {
	request := &pb.ReplicateRequest{Store: "node/store", KeepsLists: true, AnswersChallenges: true, List: &pb.KeepList{Store: "node/store", Sequence: 42}}
	for range 20_000 {
		request.Holding = append(request.Holding, strings.Repeat("b", 75))
	}
	frames, err := ReplicationHellos(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frame(s), want a multi-frame request", len(frames))
	}
	for index, frame := range frames {
		if frame.GetSequence() != request.GetList().GetSequence() || frame.GetIndex() != uint32(index) || frame.GetCount() != uint32(len(frames)) {
			t.Errorf("frame %d has sequence/index/count %d/%d/%d", index, frame.GetSequence(), frame.GetIndex(), frame.GetCount())
		}
		if len(frame.GetPayload()) > ReplicationFrameSize {
			t.Errorf("frame %d is %d bytes", index, len(frame.GetPayload()))
		}
	}
	got, err := ReadReplicationHello(receiveHellos(frames...))
	if err != nil || !proto.Equal(got, request) {
		t.Fatalf("reassembled request = %v, error %v; want original request", got, err)
	}
}

func TestReplicationHellosRefuseNilOversizedAndInvalidRequests(t *testing.T) {
	if _, err := ReplicationHellos(nil); err == nil {
		t.Fatal("a nil request was framed")
	}
	tooLarge := &pb.ReplicateRequest{Holding: []string{strings.Repeat("x", maxReplicationBytes+1)}}
	if _, err := ReplicationHellos(tooLarge); err == nil {
		t.Fatal("a request larger than the assembly cap was framed")
	}
	if _, err := ReplicationHellos(&pb.ReplicateRequest{Holding: []string{"\xff"}}); err == nil {
		t.Fatal("a request with invalid UTF-8 was framed")
	}
	frames, err := ReplicationHellos(&pb.ReplicateRequest{})
	if err != nil || len(frames) != 1 || len(frames[0].GetPayload()) != 0 {
		t.Fatalf("empty request frames = %v, error %v; want one empty frame", frames, err)
	}
}

func TestSendReplicationHellosStopsOnSendError(t *testing.T) {
	frames, err := ReplicationHellos(&pb.ReplicateRequest{Store: "node/store"})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("follower disconnected")
	if err := sendReplicationHellos(func(*pb.ReplicationHello) error { return want }, frames); !errors.Is(err, want) {
		t.Errorf("send error = %v, want original error", err)
	}
	if err := sendReplicationHellos(func(*pb.ReplicationHello) error { return nil }, frames); err != nil {
		t.Errorf("send complete hello: %v", err)
	}
}

func TestReadReplicationHelloRejectsIncompleteAndMalformedFrames(t *testing.T) {
	sequenceMismatch := &pb.ReplicationHello{Sequence: 7, Index: 0, Count: 1, Payload: mustMarshal(t, &pb.ReplicateRequest{List: &pb.KeepList{Sequence: 8}})}
	missingList := &pb.ReplicationHello{Sequence: 7, Index: 0, Count: 1, Payload: mustMarshal(t, &pb.ReplicateRequest{})}
	tooMany := &pb.ReplicationHello{Index: 0, Count: maxReplicationFrames + 1}
	oversized := &pb.ReplicationHello{Index: 0, Count: 1, Payload: make([]byte, ReplicationFrameSize+1)}
	laterOversized := &pb.ReplicationHello{Sequence: 1, Index: 1, Count: 2, Payload: make([]byte, ReplicationFrameSize+1)}
	tests := []struct {
		name   string
		frames []*pb.ReplicationHello
		err    error
	}{
		{name: "cannot receive first frame", err: errors.New("disconnected")},
		{name: "first index is not zero", frames: []*pb.ReplicationHello{{Index: 1, Count: 1}}},
		{name: "lease cannot start the handshake", frames: []*pb.ReplicationHello{{Count: 1, Lease: true}}},
		{name: "zero frame count", frames: []*pb.ReplicationHello{{Count: 0}}},
		{name: "frame count exceeds cap", frames: []*pb.ReplicationHello{tooMany}},
		{name: "first frame exceeds cap", frames: []*pb.ReplicationHello{oversized}},
		{name: "request ended early", frames: []*pb.ReplicationHello{{Count: 2}}},
		{name: "frame index mismatch", frames: []*pb.ReplicationHello{{Count: 2}, {Index: 2, Count: 2}}},
		{name: "frame count mismatch", frames: []*pb.ReplicationHello{{Count: 2}, {Index: 1, Count: 3}}},
		{name: "lease cannot be part of a fragmented handshake", frames: []*pb.ReplicationHello{{Count: 2}, {Index: 1, Count: 2, Lease: true}}},
		{name: "sequence mismatch", frames: []*pb.ReplicationHello{{Count: 2, Sequence: 1}, {Index: 1, Count: 2, Sequence: 2}}},
		{name: "later frame exceeds cap", frames: []*pb.ReplicationHello{{Count: 2, Sequence: 1}, laterOversized}},
		{name: "malformed request", frames: []*pb.ReplicationHello{{Index: 0, Count: 1, Payload: []byte{0xff}}}},
		{name: "list sequence mismatch", frames: []*pb.ReplicationHello{sequenceMismatch}},
		{name: "sequence without signed list", frames: []*pb.ReplicationHello{missingList}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := receiveHellos(tt.frames...)
			if tt.err != nil {
				recv = func() (*pb.ReplicationHello, error) { return nil, tt.err }
			}
			if _, err := ReadReplicationHello(recv); err == nil {
				t.Fatal("malformed replication request was accepted")
			}
		})
	}
}

func TestReplicationResponseIsReassembledFromBoundedFrames(t *testing.T) {
	list := &pb.KeepList{Store: "node/store", Sequence: 42}
	for range 20_000 {
		list.Blobs = append(list.Blobs, &pb.KeptBlob{Cid: strings.Repeat("b", 75)})
	}
	want := &pb.ReplicateResponse{Store: "node/store", List: list}
	frames, err := ReplicationFrames(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frame(s), want a multi-frame list", len(frames))
	}
	for index, frame := range frames {
		if frame.GetSequence() != list.GetSequence() || frame.GetIndex() != uint32(index) || frame.GetCount() != uint32(len(frames)) {
			t.Errorf("frame %d has sequence/index/count %d/%d/%d", index, frame.GetSequence(), frame.GetIndex(), frame.GetCount())
		}
		if len(frame.GetPayload()) > ReplicationFrameSize {
			t.Errorf("frame %d is %d bytes", index, len(frame.GetPayload()))
		}
	}
	got, err := ReadReplicationResponse(receiveFrames(frames...))
	if err != nil || !proto.Equal(got, want) {
		t.Fatalf("reassembled response = %v, error %v; want original response", got, err)
	}
}

func TestReplicationFramesRefuseNilAndOversizedResponses(t *testing.T) {
	if _, err := ReplicationFrames(nil); err == nil {
		t.Fatal("a nil response was framed")
	}
	tooLarge := &pb.ReplicateResponse{Hold: []string{strings.Repeat("x", maxReplicationBytes+1)}}
	if _, err := ReplicationFrames(tooLarge); err == nil {
		t.Fatal("a response larger than the assembly cap was framed")
	}
	if _, err := ReplicationFrames(&pb.ReplicateResponse{Hold: []string{"\xff"}}); err == nil {
		t.Fatal("a response with invalid UTF-8 was framed")
	}
	frames, err := ReplicationFrames(&pb.ReplicateResponse{})
	if err != nil || len(frames) != 1 || len(frames[0].GetPayload()) != 0 {
		t.Fatalf("empty response frames = %v, error %v; want one empty frame", frames, err)
	}
}

func TestReadReplicationResponseRejectsIncompleteAndMalformedFrames(t *testing.T) {
	valid, err := ReplicationFrames(&pb.ReplicateResponse{Store: "node/store"})
	if err != nil {
		t.Fatal(err)
	}
	oversized := &pb.ReplicationFrame{Index: 0, Count: 1, Payload: make([]byte, ReplicationFrameSize+1)}
	tooMany := &pb.ReplicationFrame{Index: 0, Count: maxReplicationFrames + 1}
	badDecode := &pb.ReplicationFrame{Index: 0, Count: 1, Payload: []byte{0xff}}
	sequenceMismatch := &pb.ReplicationFrame{Sequence: 7, Index: 0, Count: 1, Payload: mustMarshal(t, &pb.ReplicateResponse{List: &pb.KeepList{Sequence: 8}})}
	missingList := &pb.ReplicationFrame{Sequence: 7, Index: 0, Count: 1, Payload: mustMarshal(t, &pb.ReplicateResponse{})}
	firstOversizedLater := &pb.ReplicationFrame{Sequence: 1, Index: 1, Count: 2, Payload: make([]byte, ReplicationFrameSize+1)}
	tests := []struct {
		name   string
		frames []*pb.ReplicationFrame
		err    error
	}{
		{name: "cannot receive the first frame", err: errors.New("disconnected")},
		{name: "first index is not zero", frames: []*pb.ReplicationFrame{{Index: 1, Count: 1}}},
		{name: "zero frame count", frames: []*pb.ReplicationFrame{{Count: 0}}},
		{name: "frame count exceeds assembly cap", frames: []*pb.ReplicationFrame{tooMany}},
		{name: "first frame exceeds byte cap", frames: []*pb.ReplicationFrame{oversized}},
		{name: "response ended before all frames arrived", frames: []*pb.ReplicationFrame{{Count: 2}}},
		{name: "frame indices do not match", frames: []*pb.ReplicationFrame{{Count: 2}, {Index: 2, Count: 2}}},
		{name: "frame counts do not match", frames: []*pb.ReplicationFrame{{Count: 2}, {Index: 1, Count: 3}}},
		{name: "frame sequences do not match", frames: []*pb.ReplicationFrame{{Count: 2, Sequence: 1}, {Index: 1, Count: 2, Sequence: 2}}},
		{name: "later frame exceeds byte cap", frames: []*pb.ReplicationFrame{{Count: 2, Sequence: 1}, firstOversizedLater}},
		{name: "serialized response is malformed", frames: []*pb.ReplicationFrame{badDecode}},
		{name: "list sequence differs from frame sequence", frames: []*pb.ReplicationFrame{sequenceMismatch}},
		{name: "sequence without a signed list", frames: []*pb.ReplicationFrame{missingList}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := receiveFrames(tt.frames...)
			if tt.err != nil {
				recv = func() (*pb.ReplicationFrame, error) { return nil, tt.err }
			}
			if _, err := ReadReplicationResponse(recv); err == nil {
				t.Fatal("malformed replication response was accepted")
			}
		})
	}
	if got, err := ReadReplicationResponse(receiveFrames(valid...)); err != nil || got.GetStore() != "node/store" {
		t.Fatalf("valid one-frame response = %v, error %v", got, err)
	}
}

func mustMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
