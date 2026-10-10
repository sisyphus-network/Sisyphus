package sealed

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/sealed/sealedtest"
)

// vector is one worked example of sealing, for someone checking the
// construction or writing another implementation of it: what goes in, each
// value on the way, and what comes out. Bytes are in hexadecimal.
type vector struct {
	Name string `json:"name"`
	// SealingKey is the 32-byte key everything is derived from.
	SealingKey string `json:"sealing_key"`
	// JobID is given for what a job stores; a stored file has none, and
	// its key ID comes of its content.
	JobID string `json:"job_id,omitempty"`
	// Plaintext is given whole when short. A long one is described, and is
	// PlaintextLength bytes with the SHA-256 given.
	Plaintext       string `json:"plaintext,omitempty"`
	PlaintextNote   string `json:"plaintext_note,omitempty"`
	PlaintextLength int    `json:"plaintext_length"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
	// KeyID is written in the blob's header; DerivedKey is the key it
	// names, and the two subkeys are taken from that.
	KeyID         string `json:"key_id"`
	DerivedKey    string `json:"derived_key"`
	EncryptionKey string `json:"encryption_key"`
	NonceKey      string `json:"nonce_key"`
	// FirstNonce is the nonce of the first chunk.
	FirstNonce string `json:"first_chunk_nonce"`
	Chunks     int    `json:"chunks"`
	// Sealed is the whole blob when short; its length and SHA-256 always.
	Sealed       string `json:"sealed,omitempty"`
	SealedLength int    `json:"sealed_length"`
	SealedSHA256 string `json:"sealed_sha256"`
}

const vectorsFile = "testdata/vectors.json"

// workedExamples seals a fixed set of inputs and records every value on
// the way.
func workedExamples(t *testing.T) []vector {
	t.Helper()
	key, err := ParseKey(sealedtest.OldKey)
	if err != nil {
		t.Fatal(err)
	}
	long := pattern(2*chunkSize + 5)
	past := pattern(keySpan + 5)
	var out []vector
	for _, in := range []struct {
		name, job, note string
		plain           []byte
	}{
		{"what a job stores: nothing", "job-1", "", nil},
		{"what a job stores: a few bytes", "job-1", "", []byte("sisyphus")},
		{"what a job stores: the same bytes, for another job", "job-2", "", []byte("sisyphus")},
		{"what a job stores: two whole chunks and five bytes", "job-1", "byte i is the top byte of x(i+1), where x(0) = 1 and x(n+1) = x(n)*6364136223846793005 + 1442695040888963407 mod 2^64", long},
		{"a stored file: a few bytes", "", "", []byte("sisyphus")},
		{"a stored file: longer than the 4 MiB its key comes of", "", "the same sequence as above, 4 MiB and five bytes of it", past},
	} {
		grant := key.ForJob(in.job)
		sealing := EncryptWith(grant, bytes.NewReader(in.plain))
		if in.job == "" {
			grant = key.forFile(in.plain[:min(len(in.plain), keySpan)])
			sealing = Encrypt(key, bytes.NewReader(in.plain))
		}
		blob, err := io.ReadAll(sealing)
		if err != nil {
			t.Fatal(err)
		}
		encKey, _ := hkdf.Key(sha256.New, grant.Key[:], nil, "sisyphus sealed blob: encryption", 32)
		macKey, _ := hkdf.Key(sha256.New, grant.Key[:], nil, "sisyphus sealed blob: nonces", 32)
		v := vector{
			Name: in.name, SealingKey: hex.EncodeToString(key[:]), JobID: in.job,
			PlaintextNote: in.note, PlaintextLength: len(in.plain), PlaintextSHA256: sum(in.plain),
			KeyID: hex.EncodeToString(grant.ID[:]), DerivedKey: hex.EncodeToString(grant.Key[:]),
			EncryptionKey: hex.EncodeToString(encKey), NonceKey: hex.EncodeToString(macKey),
			FirstNonce:   hex.EncodeToString(blob[HeaderSize : HeaderSize+nonceSize]),
			Chunks:       max(1, (len(in.plain)+chunkSize-1)/chunkSize),
			SealedLength: len(blob), SealedSHA256: sum(blob),
		}
		if len(in.plain) <= 64 {
			v.Plaintext, v.Sealed = hex.EncodeToString(in.plain), hex.EncodeToString(blob)
		}
		// Each example opens to what went in, with the key it names.
		if got, err := openAll(NewRing(grant), blob); err != nil || got != string(in.plain) {
			t.Fatalf("%s does not open to its plaintext: %v", in.name, err)
		}
		out = append(out, v)
	}
	return out
}

func sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// The worked examples in testdata/vectors.json are what this code gives. A
// change to the construction changes them, and is then a change someone
// chose to make: run the tests with SEALED_UPDATE_VECTORS=1 to write them
// again, and say in the change why.
func TestTheWorkedExamplesAreWhatSealingGives(t *testing.T) {
	now, _ := json.MarshalIndent(workedExamples(t), "", "  ")
	now = append(now, '\n')
	if os.Getenv("SEALED_UPDATE_VECTORS") != "" {
		if err := os.WriteFile(vectorsFile, now, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kept, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, now) {
		t.Errorf("sealing no longer gives what %s says it gives; if the construction was changed on purpose, write the examples again with SEALED_UPDATE_VECTORS=1", vectorsFile)
	}
	// The examples say what is claimed of the construction: one job's key
	// is not another's, the same bytes sealed for two jobs differ, and a
	// file's key comes of its first 4 MiB and no more.
	var examples []vector
	if err := json.Unmarshal(kept, &examples); err != nil {
		t.Fatal(err)
	}
	byName := map[string]vector{}
	for _, v := range examples {
		byName[v.Name] = v
	}
	one, other := byName["what a job stores: a few bytes"], byName["what a job stores: the same bytes, for another job"]
	if one.Sealed == "" || one.KeyID == other.KeyID || one.DerivedKey == other.DerivedKey || one.Sealed == other.Sealed {
		t.Error("the examples do not show two jobs sealing the same bytes differently")
	}
	if !strings.HasPrefix(one.Sealed, hex.EncodeToString([]byte(magic))+one.KeyID+one.FirstNonce) {
		t.Error("an example's blob does not begin with the magic, its key ID and its first nonce")
	}
}
