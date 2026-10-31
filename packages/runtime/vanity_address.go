package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	//lint:ignore SA1019 Bitcoin's addresses are made with RIPEMD-160, which is deprecated for new designs, not for reading old ones.
	"golang.org/x/crypto/ripemd160"
	"golang.org/x/crypto/sha3"
)

// The kinds of address the vanity workload searches for.
const (
	// VanityP2PKH is a Base58Check pay-to-public-key-hash address: Bitcoin's
	// 1..., Ravencoin's R..., Litecoin's L..., by the version byte.
	VanityP2PKH = "p2pkh"
	// VanityP2WPKH is a Bech32 native SegWit v0 address: bc1q...
	VanityP2WPKH = "p2wpkh"
	// VanityETH is an Ethereum address, in EIP-55 mixed case: 0x...
	VanityETH = "eth"
)

// The alphabets addresses are written in.
const (
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	bech32Charset  = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	hexDigits      = "0123456789abcdef"
)

// vanityAddress is how one kind of address is made from a public key, and
// what a pattern for it may say.
type vanityAddress struct {
	Kind string `json:"kind"`
	// Version is a P2PKH address's version byte.
	Version byte `json:"version,omitempty"`
	// HRP is a SegWit address's human-readable part.
	HRP string `json:"hrp,omitempty"`
	// Compressed is whether a P2PKH address hashes the compressed public
	// key, as every wallet made since about 2012 does, or the
	// uncompressed one.
	Compressed bool `json:"compressed,omitempty"`
}

// address returns the address of the point p, which must be affine (Z is
// one) and normalised. buf is scratch space of at least 65 bytes.
func (v vanityAddress) address(p *secp256k1.JacobianPoint, buf []byte) string {
	switch v.Kind {
	case VanityETH:
		p.X.PutBytesUnchecked(buf[0:32])
		p.Y.PutBytesUnchecked(buf[32:64])
		return ethAddress(buf[:64])
	case VanityP2WPKH:
		h := hash160(compressedKey(p, buf))
		return segwitAddress(v.HRP, h[:])
	default:
		key := compressedKey(p, buf)
		if !v.Compressed {
			buf[0] = 4
			p.Y.PutBytesUnchecked(buf[33:65])
			key = buf[:65]
		}
		h := hash160(key)
		return base58Check(v.Version, h[:])
	}
}

// compressedKey writes the 33-byte compressed public key of p to buf.
func compressedKey(p *secp256k1.JacobianPoint, buf []byte) []byte {
	buf[0] = 2
	if p.Y.IsOdd() {
		buf[0] = 3
	}
	p.X.PutBytesUnchecked(buf[1:33])
	return buf[:33]
}

// hash160 is RIPEMD-160 of SHA-256, which Bitcoin hashes a key with.
func hash160(b []byte) [20]byte {
	sum := sha256.Sum256(b)
	r := ripemd160.New()
	r.Write(sum[:])
	var out [20]byte
	r.Sum(out[:0])
	return out
}

// base58Check is the Base58Check address of a 20-byte hash under a version
// byte.
func base58Check(version byte, hash []byte) string {
	payload := make([]byte, 0, 25)
	payload = append(payload, version)
	payload = append(payload, hash...)
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	return base58Encode(append(payload, second[:4]...))
}

// base58Encode writes b in Base58, a '1' for each leading zero byte.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	// The digits, least significant first.
	digits := make([]byte, 0, len(b)*138/100+1)
	for _, c := range b[zeros:] {
		carry := int(c)
		for i := range digits {
			carry += int(digits[i]) << 8
			digits[i] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append(digits, byte(carry%58))
			carry /= 58
		}
	}
	out := make([]byte, zeros+len(digits))
	for i := range zeros {
		out[i] = '1'
	}
	for i, d := range digits {
		out[len(out)-1-i] = base58Alphabet[d]
	}
	return string(out)
}

// segwitAddress is the Bech32 address of a version 0 witness program
// (BIP 173).
func segwitAddress(hrp string, program []byte) string {
	data := []byte{0}
	// Eight bits at a time in, five at a time out. 20 bytes are exactly
	// 32 groups, so nothing is left over.
	acc, bits := 0, 0
	for _, b := range program {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			data = append(data, byte(acc>>bits&31))
		}
	}
	values := make([]byte, 0, 2*len(hrp)+1+len(data)+6)
	for i := range len(hrp) {
		values = append(values, hrp[i]>>5)
	}
	values = append(values, 0)
	for i := range len(hrp) {
		values = append(values, hrp[i]&31)
	}
	values = append(values, data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	var out strings.Builder
	out.WriteString(hrp)
	out.WriteByte('1')
	for _, d := range data {
		out.WriteByte(bech32Charset[d])
	}
	for i := range 6 {
		out.WriteByte(bech32Charset[mod>>(5*(5-i))&31])
	}
	return out.String()
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i, g := range gen {
			if top>>i&1 == 1 {
				chk ^= g
			}
		}
	}
	return chk
}

// ethAddress is the EIP-55 address of a public key given as its 64 bytes
// of X and Y.
func ethAddress(xy []byte) string {
	h := keccak256(xy)
	return eip55(h[12:])
}

// eip55 writes a 20-byte address in hex, a letter in upper case where the
// matching nibble of the Keccak-256 of its lower-case hex is 8 or more.
func eip55(addr []byte) string {
	out := []byte(hex.EncodeToString(addr))
	h := keccak256(out)
	for i, c := range out {
		if c >= 'a' && h[i/2]>>(4*(1-i%2))&0xf >= 8 {
			out[i] = c - 'a' + 'A'
		}
	}
	return "0x" + string(out)
}

func keccak256(b []byte) []byte {
	k := sha3.NewLegacyKeccak256()
	k.Write(b)
	return k.Sum(nil)
}

// otherCase is c in the other case, or c itself if it has none.
func otherCase(c byte) byte {
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 'A'
	case c >= 'A' && c <= 'Z':
		return c - 'A' + 'a'
	}
	return c
}

// caseVariants returns every way of writing s in the alphabet, each letter
// as given or, unless exact, in the other case too.
func caseVariants(s, alphabet string, exact bool) []string {
	variants := []string{""}
	for i := range len(s) {
		options := []byte{s[i]}
		if o := otherCase(s[i]); !exact && o != s[i] {
			options = append(options, o)
		}
		var next []string
		for _, v := range variants {
			for _, c := range options {
				if strings.IndexByte(alphabet, c) >= 0 {
					next = append(next, v+string(c))
				}
			}
		}
		variants = next
	}
	return variants
}

// perChar is the chance that a character of an address drawn evenly from
// an alphabet of size n matches c: one in n, or two in n where c in
// either case is in the alphabet and case does not matter.
func perChar(c byte, alphabet string, exact bool) float64 {
	return float64(len(caseVariants(string(c), alphabet, exact))) / float64(len(alphabet))
}

var two192 = new(big.Int).Lsh(big.NewInt(1), 192)

// base58PrefixChance is the chance that the Base58Check address of a
// random 20-byte hash under the version byte begins with prefix, written
// exactly so (case-sensitive) or in either case.
//
// An address is the 25 bytes of version, hash and checksum read as one
// number N, written in Base58, after a '1' for each leading zero byte. N
// is spread evenly over [version·2¹⁹², (version+1)·2¹⁹²), the hash and
// checksum being as good as random, so the chance is how much of that
// range gives addresses that begin so. That is exact, which matters: the
// first characters after the version's are far from even. A Bitcoin
// address's second character is one of 2 to Q in all but the one in 23
// that are a character shorter, Q a third as often as the others, so "1P"
// is about 2.5 times as likely as 1 in 58, and "1z" about 23 times less.
func base58PrefixChance(version byte, prefix string, exact bool) float64 {
	total := 0.0
	for _, p := range caseVariants(prefix, base58Alphabet, exact) {
		total += base58ExactPrefixChance(version, p)
	}
	return total
}

func base58ExactPrefixChance(version byte, prefix string) float64 {
	rest := strings.TrimLeft(prefix, "1")
	ones := len(prefix) - len(rest)
	pow256 := func(n int) *big.Int { return new(big.Int).Lsh(big.NewInt(1), uint(8*n)) }
	lo := new(big.Int).Lsh(big.NewInt(int64(version)), 192)
	hi := new(big.Int).Lsh(big.NewInt(int64(version)+1), 192)
	// As many leading zero bytes as the prefix has ones, or at least as
	// many if it is nothing but ones: N is below 256^(25-ones), and unless
	// the prefix is all ones, at least 256^(24-ones). A prefix is at most
	// vanityMaxPattern characters, so ones is well under 24.
	if limit := pow256(25 - ones); hi.Cmp(limit) > 0 {
		hi = limit
	}
	if floor := pow256(24 - ones); rest != "" && lo.Cmp(floor) < 0 {
		lo = floor
	}
	count := new(big.Int)
	if lo.Cmp(hi) >= 0 {
		return 0
	}
	if rest == "" {
		count.Sub(hi, lo)
	} else {
		// The numbers whose Base58 begins with rest are those in
		// [T·58^m, (T+1)·58^m) for some m, T being rest's own value. Its
		// first digit is not 0 ('1'), so for each m these are numbers of
		// another length, and do not overlap.
		value := new(big.Int)
		for i := range len(rest) {
			value.Mul(value, big.NewInt(58)).Add(value, big.NewInt(int64(strings.IndexByte(base58Alphabet, rest[i]))))
		}
		next := new(big.Int).Add(value, big.NewInt(1))
		a, b := new(big.Int), new(big.Int)
		for scale := big.NewInt(1); ; scale.Mul(scale, big.NewInt(58)) {
			a.Mul(value, scale)
			if a.Cmp(hi) >= 0 {
				break
			}
			b.Mul(next, scale)
			if b.Cmp(hi) > 0 {
				b.Set(hi)
			}
			if a.Cmp(lo) < 0 {
				a.Set(lo)
			}
			if b.Cmp(a) > 0 {
				count.Add(count, b.Sub(b, a))
			}
		}
	}
	chance, _ := new(big.Rat).SetFrac(count, two192).Float64()
	return chance
}
