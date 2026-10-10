package replication

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"sort"
	"time"

	"github.com/ipfs/go-cid"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// A coordinator does not take a follower's word for what it holds. Each time
// a follower says what it holds, the coordinator picks some of those blobs
// and asks, of each, for the digest of a few random bytes of it under a
// number just made up. Only something that has those bytes to hand can
// answer, and the follower must answer at once.
//
// What an answer shows is that the follower could produce those bytes of
// that blob within the time allowed. It does not show that it holds the
// rest, though a follower cannot know which bytes will be asked for; that
// it will hold them tomorrow; that it would hand them over; or that two
// followers are not one disk. A follower that keeps nothing and fetches a
// blob from elsewhere when asked passes, if it is quick enough: the time
// allowed is short to make that hard for anything large, and no shorter
// than a slow follower needs.
const (
	// challengesEach is how many blobs a follower is asked about each time
	// it says what it holds.
	challengesEach = 4
	// challengeSpan is the most of a blob that is asked about at once.
	challengeSpan = 64 << 10
	// challengeWindow is how long a follower has to answer.
	challengeWindow = 30 * time.Second
)

// challenge is one question put to a follower, and when.
type challenge struct {
	cid    string
	offset uint64
	length uint32
	nonce  []byte
	issued time.Time
}

// Answer returns what shows that the bytes a challenge named are held: the
// SHA-256 of its nonce and then those bytes.
func Answer(nonce, named []byte) []byte {
	digest := sha256.New()
	digest.Write(nonce)
	digest.Write(named)
	return digest.Sum(nil)
}

// opener is the part of a store that a challenge is made of, or answered
// from.
type opener interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
}

// read returns length bytes of a blob from offset.
func read(ctx context.Context, store opener, id string, offset uint64, length uint32) ([]byte, error) {
	c, err := cid.Decode(id)
	if err != nil {
		return nil, err
	}
	blob, err := store.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	if _, err := blob.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, err
	}
	named := make([]byte, length)
	if _, err := io.ReadFull(blob, named); err != nil {
		return nil, err
	}
	return named, nil
}

// random returns a number below n, which must not be zero.
func random(n uint64) uint64 {
	var raw [8]byte
	rand.Read(raw[:]) // never fails; see crypto/rand
	return binary.BigEndian.Uint64(raw[:]) % n
}

// challenges picks the blobs to ask a follower about, of those it says it
// holds and this node has pinned, and makes a question of each. Those it
// failed to show last time come first, then those it has never shown, then
// those shown longest ago; among equals the choice is by chance, so that a
// follower cannot tell what will be asked.
func (m *Manager) challenges(ctx context.Context, now time.Time, holding []string, pinned map[string]time.Time, failed map[string]struct{}, shown map[string]time.Time) []challenge {
	type candidate struct {
		cid   string
		rank  int
		since time.Time
		draw  uint64
	}
	var candidates []candidate
	for _, id := range holding {
		if _, kept := pinned[id]; !kept {
			continue
		}
		c := candidate{cid: id, rank: 1, draw: random(1 << 62)}
		if _, was := failed[id]; was {
			c.rank = 0
		} else if at, was := shown[id]; was {
			c.rank, c.since = 2, at
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		switch {
		case x.rank != y.rank:
			return x.rank < y.rank
		case !x.since.Equal(y.since):
			return x.since.Before(y.since)
		}
		return x.draw < y.draw
	})
	var asked []challenge
	for _, c := range candidates {
		if len(asked) == challengesEach {
			break
		}
		// The question is about bytes this node can read for itself. A blob
		// it cannot open, it cannot ask about.
		parsed, _ := cid.Decode(c.cid) // pinned, so a CID
		blob, err := m.store.Open(ctx, parsed)
		if err != nil {
			continue
		}
		size := blob.Size()
		blob.Close()
		length := min(size, challengeSpan)
		ask := challenge{cid: c.cid, length: uint32(length), nonce: make([]byte, 16), issued: now}
		ask.offset = random(size - length + 1)
		rand.Read(ask.nonce) // never fails; see crypto/rand
		asked = append(asked, ask)
	}
	return asked
}

// judge checks a follower's answers against the questions it was last put,
// and returns, for each blob asked about, whether it showed it holds it. A
// question with no answer, a wrong answer, or one that came too late, is
// not shown. A blob this node can no longer read for itself is left out:
// there is nothing to check an answer against.
func (m *Manager) judge(ctx context.Context, now time.Time, pending []challenge, answers []*pb.ChallengeAnswer) map[string]bool {
	verdicts := make(map[string]bool, len(pending))
	for _, asked := range pending {
		named, err := read(ctx, m.store, asked.cid, asked.offset, asked.length)
		if err != nil {
			continue
		}
		want := Answer(asked.nonce, named)
		verdicts[asked.cid] = false
		for _, given := range answers {
			if given.GetCid() == asked.cid && bytes.Equal(given.GetNonce(), asked.nonce) && bytes.Equal(given.GetDigest(), want) && now.Sub(asked.issued) <= challengeWindow {
				verdicts[asked.cid] = true
			}
		}
	}
	return verdicts
}
