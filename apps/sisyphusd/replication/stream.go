package replication

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Keep each message below gRPC's default 4 MiB receive limit, and bound the
// memory needed to assemble one signed replication response.
const (
	ReplicationFrameSize = 1 << 20
	maxReplicationFrames = 64
	maxReplicationBytes  = ReplicationFrameSize * maxReplicationFrames
)

// ReplicationHellos encodes one request as bounded hello frames. This includes
// the follower's holding list and previous signed list, either of which can
// outgrow gRPC's default message limit on its own.
func ReplicationHellos(request *pb.ReplicateRequest) ([]*pb.ReplicationHello, error) {
	if request == nil {
		return nil, fmt.Errorf("replication request is nil")
	}
	if size := proto.Size(request); size > maxReplicationBytes {
		return nil, fmt.Errorf("replication request is %d bytes; limit is %d", size, maxReplicationBytes)
	}
	encoded, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode replication request: %w", err)
	}
	count := (len(encoded) + ReplicationFrameSize - 1) / ReplicationFrameSize
	if count == 0 {
		count = 1
	}
	sequence := uint64(0)
	if list := request.GetList(); list != nil {
		sequence = list.GetSequence()
	}
	frames := make([]*pb.ReplicationHello, 0, count)
	for index := range count {
		start := index * ReplicationFrameSize
		end := min(start+ReplicationFrameSize, len(encoded))
		frames = append(frames, &pb.ReplicationHello{
			Sequence: sequence,
			Index:    uint32(index),
			Count:    uint32(count),
			Payload:  encoded[start:end],
		})
	}
	return frames, nil
}

func sendReplicationHellos(send func(*pb.ReplicationHello) error, frames []*pb.ReplicationHello) error {
	for _, frame := range frames {
		if err := send(frame); err != nil {
			return err
		}
	}
	return nil
}

// ReadReplicationHello assembles one complete request from the follower. A
// partial or malformed request never reaches the manager for signature
// verification or state changes.
func ReadReplicationHello(recv func() (*pb.ReplicationHello, error)) (*pb.ReplicateRequest, error) {
	first, err := recv()
	if err != nil {
		return nil, err
	}
	if first.GetIndex() != 0 || first.GetCount() == 0 || first.GetCount() > maxReplicationFrames {
		return nil, fmt.Errorf("invalid first replication hello: index %d, count %d", first.GetIndex(), first.GetCount())
	}
	count, sequence := first.GetCount(), first.GetSequence()
	if len(first.GetPayload()) > ReplicationFrameSize {
		return nil, fmt.Errorf("replication hello is %d bytes; limit is %d", len(first.GetPayload()), ReplicationFrameSize)
	}
	encoded := make([]byte, 0, len(first.GetPayload())*int(count))
	encoded = append(encoded, first.GetPayload()...)
	for index := uint32(1); index < count; index++ {
		frame, err := recv()
		if err != nil {
			return nil, fmt.Errorf("receive replication hello %d of %d: %w", index, count, err)
		}
		if frame.GetIndex() != index || frame.GetCount() != count || frame.GetSequence() != sequence {
			return nil, fmt.Errorf("inconsistent replication hello %d: sequence %d, index %d, count %d", index, frame.GetSequence(), frame.GetIndex(), frame.GetCount())
		}
		if len(frame.GetPayload()) > ReplicationFrameSize {
			return nil, fmt.Errorf("replication hello is %d bytes; limit is %d", len(frame.GetPayload()), ReplicationFrameSize)
		}
		encoded = append(encoded, frame.GetPayload()...)
	}
	var request pb.ReplicateRequest
	if err := proto.Unmarshal(encoded, &request); err != nil {
		return nil, fmt.Errorf("decode replication hello: %w", err)
	}
	if list := request.GetList(); list != nil && list.GetSequence() != sequence {
		return nil, fmt.Errorf("replication hello sequence %d does not match signed list sequence %d", sequence, list.GetSequence())
	}
	if request.GetList() == nil && sequence != 0 {
		return nil, fmt.Errorf("replication hello has sequence %d but no signed list", sequence)
	}
	return &request, nil
}

// ReplicationFrames encodes one response as bounded, sequenced frames.
func ReplicationFrames(response *pb.ReplicateResponse) ([]*pb.ReplicationFrame, error) {
	if response == nil {
		return nil, fmt.Errorf("replication response is nil")
	}
	if size := proto.Size(response); size > maxReplicationBytes {
		return nil, fmt.Errorf("replication response is %d bytes; limit is %d", size, maxReplicationBytes)
	}
	encoded, err := proto.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode replication response: %w", err)
	}
	count := (len(encoded) + ReplicationFrameSize - 1) / ReplicationFrameSize
	if count == 0 {
		count = 1
	}
	sequence := uint64(0)
	if list := response.GetList(); list != nil {
		sequence = list.GetSequence()
	}
	frames := make([]*pb.ReplicationFrame, 0, count)
	for index := range count {
		start := index * ReplicationFrameSize
		end := min(start+ReplicationFrameSize, len(encoded))
		frames = append(frames, &pb.ReplicationFrame{
			Sequence: sequence,
			Index:    uint32(index),
			Count:    uint32(count),
			Payload:  encoded[start:end],
		})
	}
	return frames, nil
}

// ReadReplicationResponse receives and validates one complete framed response.
// It leaves the stream open for the next response, which later protocol slices
// will use for pushed changes.
func ReadReplicationResponse(recv func() (*pb.ReplicationFrame, error)) (*pb.ReplicateResponse, error) {
	first, err := recv()
	if err != nil {
		return nil, err
	}
	if first.GetIndex() != 0 || first.GetCount() == 0 || first.GetCount() > maxReplicationFrames {
		return nil, fmt.Errorf("invalid first replication frame: index %d, count %d", first.GetIndex(), first.GetCount())
	}
	count, sequence := first.GetCount(), first.GetSequence()
	if len(first.GetPayload()) > ReplicationFrameSize {
		return nil, fmt.Errorf("replication frame is %d bytes; limit is %d", len(first.GetPayload()), ReplicationFrameSize)
	}
	encoded := make([]byte, 0, len(first.GetPayload())*int(count))
	encoded = append(encoded, first.GetPayload()...)
	for index := uint32(1); index < count; index++ {
		frame, err := recv()
		if err != nil {
			return nil, fmt.Errorf("receive replication frame %d of %d: %w", index, count, err)
		}
		if frame.GetIndex() != index || frame.GetCount() != count || frame.GetSequence() != sequence {
			return nil, fmt.Errorf("inconsistent replication frame %d: sequence %d, index %d, count %d", index, frame.GetSequence(), frame.GetIndex(), frame.GetCount())
		}
		if len(frame.GetPayload()) > ReplicationFrameSize {
			return nil, fmt.Errorf("replication frame is %d bytes; limit is %d", len(frame.GetPayload()), ReplicationFrameSize)
		}
		encoded = append(encoded, frame.GetPayload()...)
	}
	var response pb.ReplicateResponse
	if err := proto.Unmarshal(encoded, &response); err != nil {
		return nil, fmt.Errorf("decode replication response: %w", err)
	}
	if list := response.GetList(); list != nil && list.GetSequence() != sequence {
		return nil, fmt.Errorf("replication frame sequence %d does not match signed list sequence %d", sequence, list.GetSequence())
	}
	if response.GetList() == nil && sequence != 0 {
		return nil, fmt.Errorf("replication response has sequence %d but no signed list", sequence)
	}
	return &response, nil
}
