package replication

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"google.golang.org/protobuf/proto"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// RestoredOwner is who holds the pins on what a coordinator took back from
// its followers after losing its store. Who had pinned each blob before was
// known only to the store that was lost; until when was in the list.
const RestoredOwner = "restored"

// listHeading begins what is signed of a list, so that a signature of one
// can never be taken for a signature of anything else the node key signs.
const listHeading = "sisyphus keep list v1\n"

// signedBytes returns what a coordinator signs of a list: the store, the
// sequence, and each blob with its expiry, in the encoding blob.proto
// describes. Every text is preceded by its length, so no two lists encode
// alike.
func signedBytes(list *pb.KeepList) []byte {
	data := []byte(listHeading)
	data = binary.BigEndian.AppendUint64(data, uint64(len(list.GetStore())))
	data = append(data, list.GetStore()...)
	data = binary.BigEndian.AppendUint64(data, list.GetSequence())
	data = binary.BigEndian.AppendUint64(data, uint64(len(list.GetBlobs())))
	for _, blob := range list.GetBlobs() {
		data = binary.BigEndian.AppendUint64(data, uint64(len(blob.GetCid())))
		data = append(data, blob.GetCid()...)
		if blob.GetKeepUntil() == nil {
			data = append(data, 0)
			continue
		}
		data = append(data, 1)
		data = binary.BigEndian.AppendUint64(data, uint64(blob.GetKeepUntil().AsTime().UnixNano()))
	}
	return data
}

// loadList reads the list a follower keeps in file. A missing file means it
// has none.
func loadList(file string) (*pb.KeepList, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list := new(pb.KeepList)
	if err := proto.Unmarshal(data, list); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return list, nil
}

// saveList puts a list in file in place of the one there.
func saveList(file string, list *pb.KeepList) error {
	data, _ := proto.Marshal(list) // a message that was just decoded always encodes

	// Write beside the file and rename, so a crash leaves the old list or
	// the new one and never half of either.
	tmp := file + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}
