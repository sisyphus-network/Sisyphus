package runtime

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Vanity searches the pool for a wallet address that begins or ends as its
// requester wants, without any worker, or the coordinator, ever learning a
// key that can spend from it. It is the split-key search vanitygen's pool
// mode used:
//
//  1. The requester makes a key pair (a, A = a·G) on their own machine and
//     keeps a. Only the public key A goes in the job.
//  2. Each task searches its own range of scalars b for one where the
//     address of A + b·G matches, and returns that b.
//  3. The requester adds a + b on their own machine: the private key of the
//     address found. Knowing b and A, a worker knows nothing of a + b.
//
// A result is checked by the coordinator as it comes in, by working out the
// address of A + b·G itself (Check), so a worker cannot pass off a b that
// does not match. secp256k1 coins only: Bitcoin and its forks, Ravencoin,
// Ethereum and the EVM chains.
type Vanity struct {
	// MaxTries is the most keys a job may expect to have to try, on
	// average, to find a match; a pattern harder than that is refused, as
	// is a job that asks to search more. Zero is DefaultVanityMaxTries.
	MaxTries uint64
}

// DefaultVanityMaxTries is how hard a vanity job may be when the
// coordinator does not say: 2⁴⁰ tries, about 1.1 trillion. That is a Base58
// prefix of six free characters, or a hex one of ten.
const DefaultVanityMaxTries = 1 << 40

const (
	// vanityMaxPattern is the most characters a prefix or a suffix may
	// have, far beyond what any cap allows of a Base58 one.
	vanityMaxPattern = 12
	// vanityMargin is how many times the expected tries a job searches
	// unless it says: a match is then found 95 times in 100.
	vanityMargin = 3
	// vanityBatch is how many points are brought to affine coordinates
	// with one field inversion.
	vanityBatch = 256
)

// VanityParams are a vanity job's parameters.
type VanityParams struct {
	// PublicKey is the requester's public key A, in hex: 66 digits
	// beginning 02 or 03, or 130 beginning 04.
	PublicKey string `json:"public_key"`
	// Kind is p2pkh (the default), p2wpkh or eth.
	Kind string `json:"kind,omitempty"`
	// Version is a p2pkh address's version byte: 0 for Bitcoin (the
	// default), 60 (0x3c) for Ravencoin, 48 for Litecoin, 30 for Dogecoin.
	Version *int `json:"version,omitempty"`
	// HRP is a p2wpkh address's human-readable part: bc (the default),
	// tb, ltc.
	HRP string `json:"hrp,omitempty"`
	// Compressed is whether a p2pkh address is of the compressed public
	// key. Default true.
	Compressed *bool `json:"compressed,omitempty"`
	// Prefix and Suffix are what the address must begin and end with,
	// the whole address as written: "1Abc", "RSisy", "bc1qxyz", "0xbeef".
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// CaseSensitive is whether letters must match in case: by default
	// they must for p2pkh, and need not for eth. Bech32 has one case.
	CaseSensitive *bool `json:"case_sensitive,omitempty"`
	// Tries is how many keys to search across the job. By default three
	// times the expected tries, which finds a match 95 times in 100, or
	// the cap if that is less.
	Tries uint64 `json:"tries,omitempty"`
}

// vanityTask is a task's payload: the pattern, worked out, and the range of
// b it searches, from Start for Count keys.
type vanityTask struct {
	vanityAddress
	// PublicKey is A, compressed, in hex.
	PublicKey     string `json:"public_key"`
	Prefix        string `json:"prefix,omitempty"`
	Suffix        string `json:"suffix,omitempty"`
	CaseSensitive bool   `json:"case_sensitive"`
	Start         string `json:"start"`
	Count         uint64 `json:"count"`
}

// VanityOutput is a task's output: the first b in its range whose address
// matches, as 64 hex digits, and that address, if one did; and how many
// keys it tried.
type VanityOutput struct {
	Found   bool   `json:"found"`
	Offset  string `json:"offset,omitempty"`
	Address string `json:"address,omitempty"`
	Tried   uint64 `json:"tried"`
}

// VanityResult is a job's result: the b of the first task that found one,
// the address of A + b·G, and how many keys the job tried in all.
type VanityResult struct {
	Offset  string `json:"offset"`
	Address string `json:"address"`
	Tried   uint64 `json:"tried"`
}

func (Vanity) Name() string { return "vanity" }

func (Vanity) Describe() string {
	return `Searches for a wallet address that begins or ends as you choose, without any worker learning its key: you keep a private key a, send only its public key A, and the pool finds b such that A + b·G has a matching address. You then add a + b yourself, offline. Parameters: {"public_key": "<A in hex: 66 digits from 02 or 03, or 130 from 04>", "kind": "p2pkh" | "p2wpkh" | "eth", "prefix": "<the address's beginning, as written, e.g. 1Abc, bc1qxyz, 0xbeef>", "suffix": "<its end>", "version": <p2pkh version byte, 0 Bitcoin, 60 Ravencoin>, "hrp": "<p2wpkh's bc, tb, ltc>", "compressed": <p2pkh, default true>, "case_sensitive": <default true for p2pkh, false for eth>, "tries": <keys to search, default 3 times the expected>}. Never send a private key. Result: {"offset": "<b, 64 hex digits>", "address": "<the address of A + b·G>", "tried": <keys searched>}; the job fails if no key in the range matched.`
}

// cap is the most tries a job may expect, or search.
func (v Vanity) cap() uint64 {
	if v.MaxTries == 0 {
		return DefaultVanityMaxTries
	}
	return v.MaxTries
}

func (v Vanity) Split(_ context.Context, _ Blobs, params []byte, parts int) ([][]byte, error) {
	task, tries, err := v.parse(params)
	if err != nil {
		return nil, err
	}
	var seed [32]byte
	crand.Read(seed[:]) // never fails: it crashes the program first.
	var start secp256k1.ModNScalar
	start.SetBytes(&seed)
	n := min(uint64(max(parts, 1)), tries)
	payloads := make([][]byte, 0, n)
	var from uint64
	for i := range n {
		// Spread the remainder, as primes does.
		to := tries/n*(i+1) + tries%n*(i+1)/n
		var at secp256k1.ModNScalar
		at.Add2(&start, scalarOf(from))
		task.Start = hexScalar(&at)
		task.Count = to - from
		payloads = append(payloads, mustJSON(task))
		from = to
	}
	return payloads, nil
}

// parse checks a job's parameters and works them out into a task, without
// its range, and the number of keys to search.
func (v Vanity) parse(params []byte) (vanityTask, uint64, error) {
	var task vanityTask
	var named map[string]json.RawMessage
	if err := json.Unmarshal(params, &named); err != nil {
		return task, 0, fmt.Errorf("vanity: %w", err)
	}
	for name := range named {
		for _, secret := range []string{"priv", "secret", "wif", "seed", "mnemonic", "xprv"} {
			if strings.Contains(strings.ToLower(name), secret) {
				return task, 0, fmt.Errorf("vanity: the parameters name %q. %s", name, neverPrivate)
			}
		}
	}
	var p VanityParams
	strict := json.NewDecoder(bytes.NewReader(params))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&p); err != nil {
		return task, 0, fmt.Errorf("vanity: %w", err)
	}
	key, err := parsePublicKey(p.PublicKey)
	if err != nil {
		return task, 0, err
	}
	task.PublicKey = hex.EncodeToString(key.SerializeCompressed())
	task.Kind = p.Kind
	task.Compressed = p.Compressed == nil || *p.Compressed
	task.CaseSensitive = p.CaseSensitive == nil || *p.CaseSensitive
	switch p.Kind {
	case "", VanityP2PKH:
		task.Kind = VanityP2PKH
		if p.Version != nil {
			if *p.Version < 0 || *p.Version > 255 {
				return task, 0, fmt.Errorf("vanity: version is a byte, from 0 to 255, not %d", *p.Version)
			}
			task.Version = byte(*p.Version)
		}
	case VanityP2WPKH:
		task.HRP = "bc"
		if p.HRP != "" {
			task.HRP = p.HRP
		}
		if len(task.HRP) > 16 || strings.Trim(task.HRP, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			return task, 0, fmt.Errorf("vanity: hrp is the lower-case letters and digits a SegWit address begins with, such as bc, tb or ltc, up to 16, not %q", task.HRP)
		}
		task.Compressed = true
		// Bech32 is written in one case.
		task.CaseSensitive = true
		p.Prefix, p.Suffix = strings.ToLower(p.Prefix), strings.ToLower(p.Suffix)
	case VanityETH:
		task.Compressed = false
		task.CaseSensitive = p.CaseSensitive != nil && *p.CaseSensitive
	default:
		return task, 0, fmt.Errorf("vanity: kind is p2pkh, p2wpkh or eth, not %q", p.Kind)
	}
	if p.Version != nil && task.Kind != VanityP2PKH || p.Compressed != nil && task.Kind != VanityP2PKH || p.HRP != "" && task.Kind != VanityP2WPKH {
		return task, 0, errors.New("vanity: version and compressed are for p2pkh addresses, and hrp for p2wpkh")
	}
	task.Prefix, task.Suffix = p.Prefix, p.Suffix
	expected, err := task.expectedTries()
	if err != nil {
		return task, 0, err
	}
	limit := v.cap()
	if expected > float64(limit) {
		return task, 0, fmt.Errorf("vanity: this pattern takes about %.3g tries on average to find, and this coordinator takes jobs of at most %d (--vanity-max-tries); shorten it, or match without case", expected, limit)
	}
	tries := p.Tries
	switch {
	case tries == 0:
		tries = limit
		if margin := math.Ceil(vanityMargin * expected); margin < float64(limit) {
			tries = uint64(margin)
		}
	case tries > limit:
		return task, 0, fmt.Errorf("vanity: tries asks to search %d keys, and this coordinator takes jobs of at most %d (--vanity-max-tries)", tries, limit)
	}
	return task, tries, nil
}

// neverPrivate says why a vanity job never takes a private key.
const neverPrivate = "Never send a private key: anyone running the job could spend from it. " +
	"Send only your public key, A = a·G: 66 hex digits beginning 02 or 03, or 130 beginning 04. " +
	"The pool finds b such that the address of A + b·G matches, and you add a + b on your own machine, offline, to get the key of that address. " +
	"Nobody but you ever has both a and b."

// parsePublicKey reads a public key in hex, refusing anything shaped like
// a private key, with a reason that says why.
func parsePublicKey(s string) (*secp256k1.PublicKey, error) {
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	_, notHex := hex.DecodeString(digits)
	switch {
	case len(digits) == 64 && notHex == nil:
		return nil, errors.New("vanity: public_key is 64 hex digits, the size of a private key, not a public one. " + neverPrivate)
	case strings.HasPrefix(s, "xprv") || strings.HasPrefix(s, "tprv"):
		return nil, errors.New("vanity: public_key is an extended private key. " + neverPrivate)
	case len(s) >= 50 && len(s) <= 53 && strings.Trim(s, base58Alphabet) == "":
		return nil, errors.New("vanity: public_key looks like a private key in wallet import format. " + neverPrivate)
	}
	b, err := hex.DecodeString(digits)
	switch {
	case err != nil:
		return nil, fmt.Errorf("vanity: public_key is not hex: %w", err)
	case !(len(b) == 33 && (b[0] == 2 || b[0] == 3)) && !(len(b) == 65 && b[0] == 4):
		return nil, fmt.Errorf("vanity: public_key is %d hex digits; a public key is 66 beginning 02 or 03, or 130 beginning 04", len(digits))
	}
	key, err := secp256k1.ParsePubKey(b)
	if err != nil {
		return nil, fmt.Errorf("vanity: public_key is not a point of secp256k1: %w", err)
	}
	return key, nil
}

// expectedTries checks the task's pattern and returns how many keys it
// takes on average to find an address that matches: one over the chance
// that one key's does.
func (t vanityTask) expectedTries() (float64, error) {
	if len(t.Prefix) > vanityMaxPattern || len(t.Suffix) > vanityMaxPattern {
		return 0, fmt.Errorf("vanity: a prefix or suffix is at most %d characters", vanityMaxPattern)
	}
	var fixed, alphabet string
	switch t.Kind {
	case VanityP2PKH:
		alphabet = base58Alphabet
	case VanityP2WPKH:
		fixed, alphabet = t.HRP+"1q", bech32Charset
	default:
		fixed, alphabet = "0x", hexDigits+"ABCDEF"
	}
	// What a prefix says of the part every address of the kind begins with
	// must be that part; what follows is matched by chance.
	head := min(len(t.Prefix), len(fixed))
	if t.Prefix[:head] != fixed[:head] {
		return 0, fmt.Errorf("vanity: every %s address here begins %s, so the prefix must too", t.Kind, fixed)
	}
	free := t.Prefix[head:]
	for _, s := range []string{free, t.Suffix} {
		for i := range len(s) {
			if len(caseVariants(s[i:i+1], alphabet, t.CaseSensitive)) == 0 {
				return 0, fmt.Errorf("vanity: %q is not a character of a %s address (%s)", s[i], t.Kind, alphabet)
			}
		}
	}
	chance := 1.0
	switch t.Kind {
	case VanityP2PKH:
		chance = base58PrefixChance(t.Version, t.Prefix, t.CaseSensitive)
		for i := range len(t.Suffix) {
			chance *= perChar(t.Suffix[i], base58Alphabet, t.CaseSensitive)
		}
	case VanityP2WPKH:
		chance = math.Pow(32, -float64(len(free)+len(t.Suffix)))
	default:
		// A digit is one of 16. A letter is too, in either case, and one
		// of 32 in its case, which EIP-55's checksum sets as if by chance.
		for _, c := range free + t.Suffix {
			chance /= 16
			if t.CaseSensitive && c > '9' {
				chance /= 2
			}
		}
	}
	if chance == 0 {
		return 0, fmt.Errorf("vanity: no %s address with version %d begins %s", t.Kind, t.Version, t.Prefix)
	}
	return 1 / chance, nil
}

// matches reports whether an address matches the task's pattern.
func (t vanityTask) matches(address string) bool {
	if len(address) < len(t.Prefix)+len(t.Suffix) {
		return false
	}
	begins, ends := address[:len(t.Prefix)], address[len(address)-len(t.Suffix):]
	if t.CaseSensitive {
		return begins == t.Prefix && ends == t.Suffix
	}
	return strings.EqualFold(begins, t.Prefix) && strings.EqualFold(ends, t.Suffix)
}

// parseTask reads a task's payload, with its public key and its start.
func parseTask(payload []byte) (vanityTask, secp256k1.JacobianPoint, secp256k1.ModNScalar, error) {
	var t vanityTask
	var a secp256k1.JacobianPoint
	var start secp256k1.ModNScalar
	if err := json.Unmarshal(payload, &t); err != nil {
		return t, a, start, fmt.Errorf("vanity task: %w", err)
	}
	key, err := parsePublicKey(t.PublicKey)
	if err != nil {
		return t, a, start, err
	}
	key.AsJacobian(&a)
	if err := parseScalar(t.Start, &start); err != nil {
		return t, a, start, fmt.Errorf("vanity task: start: %w", err)
	}
	return t, a, start, nil
}

func parseScalar(s string, into *secp256k1.ModNScalar) error {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("%q is not 64 hex digits", s)
	}
	if into.SetByteSlice(b) {
		return fmt.Errorf("%s is not less than the order of the curve", s)
	}
	return nil
}

func (Vanity) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	t, a, start, err := parseTask(payload)
	if err != nil {
		return nil, err
	}
	report := Report(ctx)
	report.Log(fmt.Sprintf("searching %d keys for a %s address beginning %q and ending %q", t.Count, t.Kind, t.Prefix, t.Suffix))
	var g, bG, p secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(scalarOf(1), &g)
	g.ToAffine()
	// p = A + start·G, and each key after it is the one before plus G. A
	// point at infinity, where b = -a, would spoil a batch's inversion;
	// its chance is one in 2²⁵⁶, and a match it spoilt would not pass
	// Check.
	secp256k1.ScalarBaseMultNonConst(&start, &bG)
	secp256k1.AddNonConst(&a, &bG, &p)
	batch := make([]secp256k1.JacobianPoint, vanityBatch)
	products := make([]secp256k1.FieldVal, vanityBatch)
	buf := make([]byte, 65)
	for done := uint64(0); done < t.Count; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if done%(1<<20) == 0 {
			report.Progress(float64(done) / float64(t.Count))
		}
		n := int(min(vanityBatch, t.Count-done))
		batch[0] = p
		for i := 1; i < n; i++ {
			secp256k1.AddNonConst(&batch[i-1], &g, &batch[i])
		}
		toAffine(batch[:n], products)
		secp256k1.AddNonConst(&batch[n-1], &g, &p)
		for i := range n {
			if address := t.address(&batch[i], buf); t.matches(address) {
				var b secp256k1.ModNScalar
				b.Add2(&start, scalarOf(done+uint64(i)))
				return mustJSON(VanityOutput{Found: true, Offset: hexScalar(&b), Address: address, Tried: done + uint64(i) + 1}), nil
			}
		}
		done += uint64(n)
	}
	return mustJSON(VanityOutput{Tried: t.Count}), nil
}

// toAffine brings points to affine coordinates with one field inversion
// for them all (Montgomery's trick), using products as scratch space.
func toAffine(points []secp256k1.JacobianPoint, products []secp256k1.FieldVal) {
	products[0].Set(&points[0].Z)
	for i := 1; i < len(points); i++ {
		products[i].Mul2(&products[i-1], &points[i].Z)
	}
	var inverse, zInv, z2 secp256k1.FieldVal
	inverse.Set(&products[len(points)-1]).Inverse()
	for i := len(points) - 1; i >= 0; i-- {
		zInv.Set(&inverse)
		if i > 0 {
			// 1/Zi is 1/(Z0…Zi) times Z0…Zi-1; and 1/(Z0…Zi-1) is
			// 1/(Z0…Zi) times Zi, for the next one down.
			zInv.Mul(&products[i-1])
			inverse.Mul(&points[i].Z)
		}
		z2.SquareVal(&zInv)
		points[i].X.Mul(&z2).Normalize()
		points[i].Y.Mul(z2.Mul(&zInv)).Normalize()
		points[i].Z.SetInt(1)
	}
}

// Check checks a task's output against its payload without running it
// again: that b is in the task's range, that A + b·G has the address the
// output says, and that it matches. A worker that says it found nothing
// cannot be caught so, only by running the task again, as --audit-share
// does.
func (Vanity) Check(payload, output []byte) error {
	t, a, start, err := parseTask(payload)
	if err != nil {
		return err
	}
	var out VanityOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return fmt.Errorf("vanity: the output is not one: %w", err)
	}
	if !out.Found {
		if out.Tried != t.Count || out.Offset != "" || out.Address != "" {
			return fmt.Errorf("vanity: an output that found nothing tried all %d keys and names none", t.Count)
		}
		return nil
	}
	var b secp256k1.ModNScalar
	if err := parseScalar(out.Offset, &b); err != nil {
		return fmt.Errorf("vanity: offset: %w", err)
	}
	// i = b - start must be one of the task's.
	var i secp256k1.ModNScalar
	i.NegateVal(&start).Add(&b)
	index := i.Bytes()
	at := binary.BigEndian.Uint64(index[24:])
	if !bytes.Equal(index[:24], make([]byte, 24)) || at >= t.Count || out.Tried != at+1 {
		return fmt.Errorf("vanity: offset %s is not the key the task tried last of the %d from %s", out.Offset, t.Count, t.Start)
	}
	var bG, p secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&b, &bG)
	secp256k1.AddNonConst(&a, &bG, &p)
	p.ToAffine()
	address := t.address(&p, make([]byte, 65))
	switch {
	case address != out.Address:
		return fmt.Errorf("vanity: the address of A + b·G for offset %s is %s, not %s", out.Offset, address, out.Address)
	case !t.matches(address):
		return fmt.Errorf("vanity: %s does not match the pattern", address)
	}
	return nil
}

// Aggregate takes the first task's match. Every task's output has been
// checked as it came in, so a match here is one. A job that found none
// fails, saying so.
func (Vanity) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	var result VanityResult
	for i, output := range outputs {
		var out VanityOutput
		if err := json.Unmarshal(output, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		result.Tried += out.Tried
		if out.Found && result.Address == "" {
			result.Offset, result.Address = out.Offset, out.Address
		}
	}
	if result.Address == "" {
		return nil, fmt.Errorf("vanity: none of the %d keys searched has a matching address; submit the job again to search others", result.Tried)
	}
	return mustJSON(result), nil
}

// scalarOf is n as a scalar.
func scalarOf(n uint64) *secp256k1.ModNScalar {
	var b [32]byte
	binary.BigEndian.PutUint64(b[24:], n)
	var s secp256k1.ModNScalar
	s.SetBytes(&b)
	return &s
}

func hexScalar(s *secp256k1.ModNScalar) string {
	b := s.Bytes()
	return hex.EncodeToString(b[:])
}
