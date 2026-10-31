package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// keyOf is the private key k, a small number, as a scalar.
func keyOf(k uint64) *secp256k1.ModNScalar { return scalarOf(k) }

// addressOf is the address of the private key k, made the long way: from
// the key's public key, not by the workload's search.
func addressOf(k *secp256k1.ModNScalar, kind vanityAddress) string {
	var p secp256k1.JacobianPoint
	secp256k1.NewPrivateKey(k).PubKey().AsJacobian(&p)
	return kind.address(&p, make([]byte, 65))
}

func publicHex(k *secp256k1.ModNScalar) string {
	return hex.EncodeToString(secp256k1.NewPrivateKey(k).PubKey().SerializeCompressed())
}

// The addresses of the private keys 1, 2 and 3, as wallets give them. The
// Bech32 one of key 1 is BIP 173's own example
// (https://github.com/bitcoin/bips/blob/master/bip-0173.mediawiki#examples),
// whose program is the hash of the compressed generator point.
func TestVanityAddressesOfKnownKeys(t *testing.T) {
	for _, tt := range []struct {
		key  uint64
		kind vanityAddress
		want string
	}{
		{1, vanityAddress{Kind: VanityP2PKH, Compressed: true}, "1BgGZ9tcN4rm9KBzDn7KprQz87SZ26SAMH"},
		{1, vanityAddress{Kind: VanityP2PKH}, "1EHNa6Q4Jz2uvNExL497mE43ikXhwF6kZm"},
		{2, vanityAddress{Kind: VanityP2PKH, Compressed: true}, "1cMh228HTCiwS8ZsaakH8A8wze1JR5ZsP"},
		{3, vanityAddress{Kind: VanityP2PKH, Compressed: true}, "1CUNEBjYrCn2y1SdiUMohaKUi4wpP326Lb"},
		{1, vanityAddress{Kind: VanityP2WPKH, HRP: "bc", Compressed: true}, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{1, vanityAddress{Kind: VanityETH}, "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"},
		{2, vanityAddress{Kind: VanityETH}, "0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF"},
		{3, vanityAddress{Kind: VanityETH}, "0x6813Eb9362372EEF6200f3b1dbC3f819671cBA69"},
	} {
		if got := addressOf(keyOf(tt.key), tt.kind); got != tt.want {
			t.Errorf("key %d as %+v: %s, want %s", tt.key, tt.kind, got, tt.want)
		}
	}
	// Ravencoin's version byte gives the same hash an address beginning R.
	raven := addressOf(keyOf(1), vanityAddress{Kind: VanityP2PKH, Version: 0x3c, Compressed: true})
	if !strings.HasPrefix(raven, "R") || len(raven) != 34 {
		t.Errorf("Ravencoin address of key 1: %s", raven)
	}
	if got := base58Encode([]byte{0, 0, 1}); got != "112" {
		t.Errorf("base58 of 0 0 1: %s", got)
	}
}

// EIP-55's own test vectors
// (https://eips.ethereum.org/EIPS/eip-55#test-cases): all caps, all lower
// case, and mixed.
func TestEIP55Vectors(t *testing.T) {
	for _, want := range []string{
		"0x52908400098527886E0F7030069857D2E4169EE7",
		"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
		"0xde709f2102306220921060314715629080e2fb77",
		"0x27b1fdb04752bbc536007a920d24acb045561c26",
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		b, _ := hex.DecodeString(strings.ToLower(want[2:]))
		if got := eip55(b); got != want {
			t.Errorf("EIP-55 of %s: %s", want, got)
		}
	}
}

// The batched inversion gives each point as its own inversion does.
func TestVanityBatchToAffine(t *testing.T) {
	var g secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(keyOf(7), &g)
	points := make([]secp256k1.JacobianPoint, 5)
	points[0] = g
	for i := 1; i < len(points); i++ {
		secp256k1.AddNonConst(&points[i-1], &g, &points[i])
	}
	want := make([]secp256k1.JacobianPoint, len(points))
	copy(want, points)
	toAffine(points, make([]secp256k1.FieldVal, len(points)))
	for i := range points {
		want[i].ToAffine()
		if !points[i].X.Equals(&want[i].X) || !points[i].Y.Equals(&want[i].Y) || !points[i].Z.IsOne() {
			t.Errorf("point %d differs", i)
		}
	}
}

func TestVanityBase58PrefixChance(t *testing.T) {
	for _, tt := range []struct {
		version byte
		prefix  string
		exact   bool
		want    float64
	}{
		{0, "", true, 1},
		{0, "1", true, 1},
		// A second '1' is a zero byte at the head of the hash.
		{0, "11", true, 1.0 / 256},
		{0, "111", true, 1.0 / 65536},
		// Every Ravencoin address begins R, and no Bitcoin one does.
		{0x3c, "R", true, 1},
		{0, "R", true, 0},
		{0x3c, "1", true, 0},
		// 'l' is not Base58, but 'L' is.
		{0, "1l", true, 0},
	} {
		if got := base58PrefixChance(tt.version, tt.prefix, tt.exact); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("chance of %q under version %d: %g, want %g", tt.prefix, tt.version, got, tt.want)
		}
	}
	// Either case is the sum of each.
	either := base58PrefixChance(0, "1ab", false)
	sum := base58PrefixChance(0, "1ab", true) + base58PrefixChance(0, "1Ab", true) + base58PrefixChance(0, "1aB", true) + base58PrefixChance(0, "1AB", true)
	if math.Abs(either-sum) > 1e-15 || either == 0 {
		t.Errorf("1ab in either case: %g, the cases summed: %g", either, sum)
	}
	// The second character of a Bitcoin address is far from even: 'P' is
	// common, 'Q' less so, and 'z' rare.
	if p, q, z := base58PrefixChance(0, "1P", true), base58PrefixChance(0, "1Q", true), base58PrefixChance(0, "1z", true); p < 2/58.0 || q > 1/58.0 || z > 1/58.0/20 {
		t.Errorf("1P: %g, 1Q: %g, 1z: %g", p, q, z)
	}
}

// What the exact sum says is what random hashes give.
func TestVanityBase58ChanceMatchesSampling(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	const samples = 200_000
	prefixes := []string{"1A", "1P", "1Q", "1a", "1Z"}
	counts := make([]int, len(prefixes))
	hash := make([]byte, 20)
	for range samples {
		for i := range hash {
			hash[i] = byte(rng.Uint32())
		}
		address := base58Check(0, hash)
		for i, p := range prefixes {
			if strings.HasPrefix(address, p) {
				counts[i]++
			}
		}
	}
	for i, p := range prefixes {
		chance := base58PrefixChance(0, p, true)
		want := chance * samples
		if math.Abs(float64(counts[i])-want) > 5*math.Sqrt(want)+1 {
			t.Errorf("%s: %d of %d sampled, %g expected", p, counts[i], samples, want)
		}
	}
}

func vanityParams(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVanityExpectedTries(t *testing.T) {
	a := publicHex(keyOf(12345))
	for _, tt := range []struct {
		params map[string]any
		want   float64
	}{
		{map[string]any{"public_key": a, "kind": "eth", "prefix": "0xab"}, 256},
		{map[string]any{"public_key": a, "kind": "eth", "prefix": "0", "suffix": "00"}, 256},
		{map[string]any{"public_key": a, "kind": "p2wpkh", "prefix": "BC1QAC", "suffix": "z"}, 32 * 32 * 32},
		{map[string]any{"public_key": a, "kind": "p2wpkh", "hrp": "tb", "prefix": "tb"}, 1},
		{map[string]any{"public_key": a, "prefix": "1", "suffix": "zz"}, 58 * 58},
		{map[string]any{"public_key": a, "prefix": "1", "suffix": "zz", "case_sensitive": false}, 58 * 58 / 4},
		{map[string]any{"public_key": a, "suffix": "1"}, 58},
		{map[string]any{"public_key": a, "version": 60, "prefix": "R"}, 1},
	} {
		task, _, err := (Vanity{}).parse(vanityParams(t, tt.params))
		if err != nil {
			t.Errorf("%v: %v", tt.params, err)
			continue
		}
		if got, _ := task.expectedTries(); math.Abs(got-tt.want) > 1e-9*tt.want {
			t.Errorf("%v: %g tries, want %g", tt.params, got, tt.want)
		}
	}
	// A hex letter in its case is one in 32, as EIP-55 sets it.
	task, _, _ := (Vanity{}).parse(vanityParams(t, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xAb", "case_sensitive": true}))
	if got, _ := task.expectedTries(); got != 1024 {
		t.Errorf("0xAb in its case: %g", got)
	}
}

// The tries searched are three times the expected unless the job says,
// and never past the cap; a pattern expected to take more is refused.
func TestVanityCap(t *testing.T) {
	a := publicHex(keyOf(99))
	_, tries, err := (Vanity{}).parse(vanityParams(t, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xabc"}))
	if err != nil || tries != 3*4096 {
		t.Errorf("default tries: %d, %v", tries, err)
	}
	_, tries, err = (Vanity{MaxTries: 5000}).parse(vanityParams(t, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xabc"}))
	if err != nil || tries != 5000 {
		t.Errorf("tries under a cap of 5000: %d, %v", tries, err)
	}
	_, tries, err = (Vanity{}).parse(vanityParams(t, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xabc", "tries": 100}))
	if err != nil || tries != 100 {
		t.Errorf("tries asked for: %d, %v", tries, err)
	}
	for _, tt := range []struct {
		cap    uint64
		params map[string]any
		want   string
	}{
		{4000, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xabc"}, "about 4.1e+03 tries on average"},
		{0, map[string]any{"public_key": a, "kind": "eth", "prefix": "0x0123456789", "suffix": "a"}, "at most 1099511627776 (--vanity-max-tries)"},
		{5000, map[string]any{"public_key": a, "kind": "eth", "prefix": "0xa", "tries": 5001}, "tries asks to search 5001 keys"},
	} {
		if _, _, err := (Vanity{MaxTries: tt.cap}).parse(vanityParams(t, tt.params)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v under %d: %v", tt.params, tt.cap, err)
		}
	}
}

// Nothing shaped like a private key is taken, and the reason says why.
func TestVanityRefusesPrivateKeys(t *testing.T) {
	key := keyOf(424242).Bytes()
	private := hex.EncodeToString(key[:])
	for _, tt := range []struct {
		params string
		want   string
	}{
		{`{"public_key":"` + private + `"}`, "64 hex digits, the size of a private key"},
		{`{"public_key":"0x` + private + `"}`, "64 hex digits, the size of a private key"},
		{`{"public_key":"KwDiBf89QgGbjEhKnhXJuH7LrciVrZi3qYjgd9M7rFU73sVHnoWn"}`, "wallet import format"},
		{`{"public_key":"5HueCGU8rMjxEXxiPuD5BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ"}`, "wallet import format"},
		{`{"public_key":"xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"}`, "extended private key"},
		{`{"public_key":"` + publicHex(keyOf(1)) + `","private_key":"` + private + `"}`, `name "private_key"`},
		{`{"public_key":"` + publicHex(keyOf(1)) + `","Seed":"x"}`, `name "Seed"`},
	} {
		_, _, err := (Vanity{}).parse([]byte(tt.params))
		if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "Never send a private key") {
			t.Errorf("%s: %v", tt.params, err)
		}
	}
}

func TestVanityRefusesBadParameters(t *testing.T) {
	a := publicHex(keyOf(5))
	var uncompressed string
	{
		b := secp256k1.NewPrivateKey(keyOf(5)).PubKey().SerializeUncompressed()
		uncompressed = hex.EncodeToString(b)
	}
	offCurve := "02" + strings.Repeat("ff", 32)
	for _, tt := range []struct {
		params string
		want   string
	}{
		{`[]`, "vanity: json"},
		{`{"public_key":"` + a + `","colour":"red"}`, "unknown field"},
		{`{"public_key":"zz"}`, "not hex"},
		{`{"public_key":"` + a[2:] + `"}`, "64 hex digits"},
		{`{"public_key":"05` + a[2:] + `"}`, "a public key is 66 beginning 02 or 03"},
		{`{"public_key":"` + a + `00"}`, "68 hex digits"},
		{`{"public_key":"` + offCurve + `"}`, "not a point of secp256k1"},
		{`{"public_key":"` + a + `","kind":"solana"}`, "kind is p2pkh, p2wpkh or eth"},
		{`{"public_key":"` + a + `","version":256}`, "version is a byte"},
		{`{"public_key":"` + a + `","version":-1}`, "version is a byte"},
		{`{"public_key":"` + a + `","kind":"p2wpkh","hrp":"BC"}`, "hrp is the lower-case"},
		{`{"public_key":"` + a + `","kind":"p2wpkh","hrp":"abcdefghijklmnopq"}`, "hrp is the lower-case"},
		{`{"public_key":"` + a + `","kind":"eth","version":0}`, "version and compressed are for p2pkh"},
		{`{"public_key":"` + a + `","kind":"eth","compressed":true}`, "version and compressed are for p2pkh"},
		{`{"public_key":"` + a + `","hrp":"bc"}`, "hrp for p2wpkh"},
		{`{"public_key":"` + a + `","prefix":"1234567890123"}`, "at most 12 characters"},
		{`{"public_key":"` + a + `","suffix":"1234567890123"}`, "at most 12 characters"},
		{`{"public_key":"` + a + `","kind":"eth","prefix":"beef"}`, "begins 0x, so the prefix must too"},
		{`{"public_key":"` + a + `","kind":"p2wpkh","prefix":"bc1p"}`, "begins bc1q"},
		{`{"public_key":"` + a + `","kind":"eth","prefix":"0xbeeg"}`, `'g' is not a character of a eth address`},
		{`{"public_key":"` + a + `","prefix":"10"}`, `'0' is not a character of a p2pkh address`},
		{`{"public_key":"` + a + `","prefix":"1I","case_sensitive":true}`, `'I' is not`},
		{`{"public_key":"` + a + `","kind":"p2wpkh","suffix":"b"}`, `'b' is not`},
		{`{"public_key":"` + a + `","prefix":"R"}`, "no p2pkh address with version 0 begins R"},
	} {
		if _, err := (Vanity{}).Split(context.Background(), nil, []byte(tt.params), 2); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.params, err, tt.want)
		}
	}
	// An uncompressed key, and one with 0x before it, are taken.
	for _, key := range []string{uncompressed, "0x" + a} {
		if _, err := (Vanity{}).Split(context.Background(), nil, []byte(`{"public_key":"`+key+`","prefix":"1"}`), 1); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	// In either case, 'I' stands for 'i', which Base58 has.
	if _, err := (Vanity{}).Split(context.Background(), nil, []byte(`{"public_key":"`+a+`","prefix":"1I","case_sensitive":false}`), 1); err != nil {
		t.Error(err)
	}
}

// runVanity splits a job, runs every task and checks each output as the
// coordinator would, and puts them together.
func runVanity(t *testing.T, params []byte, parts int) (VanityResult, []vanityTask, [][]byte) {
	t.Helper()
	ctx := context.Background()
	payloads, err := (Vanity{}).Split(ctx, nil, params, parts)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []vanityTask
	var outputs [][]byte
	for _, payload := range payloads {
		var task vanityTask
		if err := json.Unmarshal(payload, &task); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
		output, err := (Vanity{}).Execute(ctx, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := Builtin().Check("vanity", payload, output); err != nil {
			t.Fatalf("an honest output did not pass: %v", err)
		}
		outputs = append(outputs, output)
	}
	result, err := (Vanity{}).Aggregate(ctx, nil, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var r VanityResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatal(err)
	}
	return r, tasks, outputs
}

// The whole scheme: the requester keeps a, the pool finds b, and a + b is
// the key of the address found.
func TestVanityFindsAKeyOnlyTheRequesterCanComplete(t *testing.T) {
	var seed [32]byte
	copy(seed[:], "the requester's own secret seed!")
	var a secp256k1.ModNScalar
	a.SetBytes(&seed)
	for _, tt := range []struct {
		params map[string]any
		kind   vanityAddress
	}{
		// Each searches 20 times the keys it expects to need, so that a
		// match is all but certain wherever the random start falls.
		{map[string]any{"kind": "eth", "prefix": "0xab", "suffix": "c", "tries": 20 * 4096}, vanityAddress{Kind: VanityETH}},
		{map[string]any{"prefix": "1Ab", "tries": 20 * 1400}, vanityAddress{Kind: VanityP2PKH, Compressed: true}},
		{map[string]any{"prefix": "RA", "version": 60, "compressed": false, "tries": 20 * 60}, vanityAddress{Kind: VanityP2PKH, Version: 60}},
		{map[string]any{"kind": "p2wpkh", "prefix": "bc1qz", "suffix": "x", "tries": 20 * 1024}, vanityAddress{Kind: VanityP2WPKH, HRP: "bc", Compressed: true}},
	} {
		tt.params["public_key"] = publicHex(&a)
		result, tasks, _ := runVanity(t, vanityParams(t, tt.params), 3)
		var b, key secp256k1.ModNScalar
		if err := parseScalar(result.Offset, &b); err != nil {
			t.Fatal(err)
		}
		key.Add2(&a, &b)
		if got := addressOf(&key, tt.kind); got != result.Address {
			t.Errorf("%v: a + b has address %s, the job found %s", tt.params, got, result.Address)
		}
		if !tasks[0].matches(result.Address) || len(tasks) != 3 {
			t.Errorf("%v: %s in %d tasks", tt.params, result.Address, len(tasks))
		}
	}
}

// A task that matches nothing tries all its keys, across batches; a job
// in which no task did fails.
func TestVanityJobThatFindsNothingFails(t *testing.T) {
	params := vanityParams(t, map[string]any{"public_key": publicHex(keyOf(77)), "kind": "eth", "prefix": "0xabcd", "suffix": "ef01", "tries": 600})
	payloads, err := (Vanity{}).Split(context.Background(), nil, params, 1)
	if err != nil || len(payloads) != 1 {
		t.Fatal(payloads, err)
	}
	output, err := (Vanity{}).Execute(context.Background(), nil, payloads[0])
	var out VanityOutput
	if err != nil || json.Unmarshal(output, &out) != nil {
		t.Fatal(string(output), err)
	}
	if out.Found {
		// One time in seven million.
		t.Fatalf("found %s", out.Address)
	}
	if out.Tried != 600 {
		t.Errorf("tried %d", out.Tried)
	}
	if err := (Vanity{}).Check(payloads[0], output); err != nil {
		t.Error(err)
	}
	if _, err := (Vanity{}).Aggregate(context.Background(), nil, [][]byte{output, output}); err == nil || !strings.Contains(err.Error(), "none of the 1200 keys") {
		t.Errorf("a job that found nothing: %v", err)
	}
	if _, err := (Vanity{}).Aggregate(context.Background(), nil, [][]byte{[]byte("nope")}); err == nil {
		t.Error("an output that is not one was taken")
	}
}

// A worker that lies is caught by Check.
func TestVanityCheckCatchesALyingWorker(t *testing.T) {
	a := keyOf(31337)
	params := vanityParams(t, map[string]any{"public_key": publicHex(a), "kind": "eth", "prefix": "0xa", "tries": 2000})
	payloads, err := (Vanity{}).Split(context.Background(), nil, params, 2)
	if err != nil {
		t.Fatal(err)
	}
	payload := payloads[0]
	honest, err := (Vanity{}).Execute(context.Background(), nil, payload)
	if err != nil {
		t.Fatal(err)
	}
	var out VanityOutput
	if err := json.Unmarshal(honest, &out); err != nil || !out.Found {
		t.Fatalf("%s %v", honest, err)
	}
	var task vanityTask
	_ = json.Unmarshal(payload, &task)
	var start secp256k1.ModNScalar
	_ = parseScalar(task.Start, &start)
	// A key of the task's that does not match: the first of them.
	var before secp256k1.ModNScalar
	var beforeAddress string
	var beforeTried uint64
	for i := range task.Count {
		before.Add2(&start, keyOf(i))
		var sum secp256k1.ModNScalar
		sum.Add2(a, &before)
		if beforeAddress = addressOf(&sum, task.vanityAddress); !task.matches(beforeAddress) {
			beforeTried = i + 1
			break
		}
	}
	var outside secp256k1.ModNScalar
	outside.Add2(&start, keyOf(task.Count))

	lie := func(o VanityOutput) []byte { return mustJSON(o) }
	for _, tt := range []struct {
		name   string
		output []byte
		want   string
	}{
		{"not an output", []byte("{"), "is not one"},
		{"found nothing, early", lie(VanityOutput{Tried: 1}), "tried all"},
		{"found nothing, naming one", lie(VanityOutput{Tried: task.Count, Address: out.Address}), "names none"},
		{"an offset that is not one", lie(VanityOutput{Found: true, Offset: "xyz", Address: out.Address, Tried: out.Tried}), "offset"},
		{"another b, said to give the address", lie(VanityOutput{Found: true, Offset: hexScalar(&before), Address: out.Address, Tried: beforeTried}), "is " + beforeAddress + ", not"},
		{"a b that does not match", lie(VanityOutput{Found: true, Offset: hexScalar(&before), Address: beforeAddress, Tried: beforeTried}), "does not match"},
		{"a b outside the task", lie(VanityOutput{Found: true, Offset: hexScalar(&outside), Address: out.Address, Tried: task.Count + 1}), "is not the key the task tried last"},
		{"tried miscounted", lie(VanityOutput{Found: true, Offset: out.Offset, Address: out.Address, Tried: out.Tried + 1}), "is not the key the task tried last"},
	} {
		if err := (Vanity{}).Check(payload, tt.output); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}
	if err := (Vanity{}).Check([]byte("{"), honest); err == nil {
		t.Error("a payload that is not one was taken")
	}
	// A workload that does not check its outputs takes any.
	if err := Builtin().Check("primes", nil, []byte("anything")); err != nil {
		t.Error(err)
	}
}

func TestVanityExecuteRefusesBadPayloads(t *testing.T) {
	a := publicHex(keyOf(8))
	for _, payload := range []string{
		`{`,
		`{"kind":"eth","public_key":"zz","start":"00","count":1}`,
		`{"kind":"eth","public_key":"` + a + `","start":"00","count":1}`,
		`{"kind":"eth","public_key":"` + a + `","start":"` + strings.Repeat("ff", 32) + `","count":1}`,
	} {
		if _, err := (Vanity{}).Execute(context.Background(), nil, []byte(payload)); err == nil {
			t.Errorf("%s was run", payload)
		}
	}
}

func TestVanityStopsWhenCancelledAndReports(t *testing.T) {
	params := vanityParams(t, map[string]any{"public_key": publicHex(keyOf(9)), "kind": "eth", "prefix": "0xa", "tries": 10})
	payloads, err := (Vanity{}).Split(context.Background(), nil, params, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Vanity{}).Execute(ctx, nil, payloads[0]); err == nil {
		t.Error("a cancelled task ran")
	}
	said := new(heard)
	if _, err := (Vanity{}).Execute(WithReporter(context.Background(), said), nil, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(said.lines) != 1 || !strings.Contains(said.lines[0], `searching 10 keys for a eth address beginning "0xa"`) || len(said.progress) != 1 {
		t.Errorf("lines %q, progress %v", said.lines, said.progress)
	}
	if !strings.Contains((Vanity{}).Describe(), "Never send a private key") {
		t.Error((Vanity{}).Describe())
	}
}

// matches is false for an address too short to hold the pattern.
func TestVanityMatchesNothingTooShort(t *testing.T) {
	if (vanityTask{Prefix: "abc", Suffix: "def", CaseSensitive: true}).matches("abcde") {
		t.Error("matched")
	}
}
