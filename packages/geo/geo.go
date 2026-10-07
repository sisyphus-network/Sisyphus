// Package geo says which country an Internet address is registered in,
// from a table carried inside the program. Nothing is asked of anyone: a
// node can place itself and the nodes it knows without telling a service
// outside who it is or whom it talks to.
//
// The table is made from what the five regional Internet registries
// publish of the address blocks they have handed out. It gives the country
// a block was registered to, which is nearly always where it is used and
// now and then is not: a company's block registered at its head office and
// used abroad, or a satellite or mobile network.
package geo

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed countries.bin.gz
var embedded []byte

// Table is a table of address blocks and the countries they are in.
type Table struct {
	countries []string
	// Each list is of the addresses at which the country changes, in
	// order, with the country from there on: an index into countries, or
	// nowhere for addresses in no block.
	v4 []mark[uint32]
	// IPv6 addresses are told apart by their first 64 bits, which is as
	// fine as registries hand them out.
	v6 []mark[uint64]
}

type mark[T uint32 | uint64] struct {
	from    T
	country uint8
}

const nowhere = 255

const magic = "SGEO1"

// builtin is the table carried in the program, read when first wanted. It
// is made by this package and its tests check that it reads.
var builtin = sync.OnceValue(func() *Table {
	table, _ := Read(bytes.NewReader(embedded))
	return table
})

// Country returns the two-letter code of the country addr is registered
// in, by the built-in table, or nothing if it is in no block there, or is
// an address with no place in the world: a private, loopback or link-local
// one.
func Country(addr netip.Addr) string { return builtin().Country(addr) }

// Country returns the two-letter code of the country addr is registered
// in, or nothing.
func (t *Table) Country(addr netip.Addr) string {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() {
		return ""
	}
	raw := addr.AsSlice()
	if addr.Is4() {
		return find(t, t.v4, binary.BigEndian.Uint32(raw))
	}
	return find(t, t.v6, binary.BigEndian.Uint64(raw[:8]))
}

// find returns the country of the last mark at or before at.
func find[T uint32 | uint64](t *Table, marks []mark[T], at T) string {
	after := sort.Search(len(marks), func(i int) bool { return marks[i].from > at })
	if after == 0 || marks[after-1].country == nowhere {
		return ""
	}
	return t.countries[marks[after-1].country]
}

// Size returns how many places in the table a country begins or ends, for
// each kind of address.
func (t *Table) Size() (v4, v6 int) { return len(t.v4), len(t.v6) }

// block is a run of addresses and its country.
type block[T uint32 | uint64] struct {
	from, to T // to is the last address in it
	country  string
}

// Compile makes a table from what the regional registries publish: their
// "delegated extended" files, one reader for each. Blocks that are not
// handed out to anyone are left out.
func Compile(sources ...io.Reader) (*Table, error) {
	var v4 []block[uint32]
	var v6 []block[uint64]
	for _, source := range sources {
		lines := bufio.NewScanner(source)
		for lines.Scan() {
			// registry|country|kind|start|size|date|status|...
			f := strings.Split(lines.Text(), "|")
			if len(f) < 7 || len(f[1]) != 2 || f[1] == "ZZ" || (f[6] != "allocated" && f[6] != "assigned") {
				continue
			}
			start, err := netip.ParseAddr(f[3])
			size, sizeErr := strconv.ParseUint(f[4], 10, 64)
			if err != nil || sizeErr != nil || size == 0 {
				continue
			}
			switch {
			case f[2] == "ipv4" && start.Is4():
				// The size is a count of addresses.
				from := binary.BigEndian.Uint32(start.AsSlice())
				v4 = append(v4, block[uint32]{from, from + uint32(size-1), strings.ToUpper(f[1])})
			case f[2] == "ipv6" && start.Is6() && size <= 64:
				// The size is the length of the prefix.
				from := binary.BigEndian.Uint64(start.AsSlice()[:8])
				v6 = append(v6, block[uint64]{from, from | (^uint64(0) >> size), strings.ToUpper(f[1])})
			}
		}
		if err := lines.Err(); err != nil {
			return nil, fmt.Errorf("read a registry's file: %w", err)
		}
	}
	if len(v4) == 0 && len(v6) == 0 {
		return nil, errors.New("the registries' files name no address blocks")
	}
	t := &Table{}
	index := map[string]uint8{}
	number := func(country string) uint8 {
		n, known := index[country]
		if !known {
			n = uint8(len(t.countries))
			index[country] = n
			t.countries = append(t.countries, country)
		}
		return n
	}
	t.v4 = marks(v4, number)
	t.v6 = marks(v6, number)
	if len(t.countries) >= nowhere {
		return nil, fmt.Errorf("the registries' files name %d countries, more than there are", len(t.countries))
	}
	return t, nil
}

// marks turns blocks into the places where the country changes.
func marks[T uint32 | uint64](blocks []block[T], number func(string) uint8) []mark[T] {
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].from < blocks[j].from })
	var out []mark[T]
	put := func(from T, country uint8) {
		// A block that begins at or before the end of the one before, as
		// two registries' may, takes its place from there on.
		for len(out) > 0 && out[len(out)-1].from >= from {
			out = out[:len(out)-1]
		}
		if len(out) == 0 || out[len(out)-1].country != country {
			out = append(out, mark[T]{from, country})
		}
	}
	for _, b := range blocks {
		put(b.from, number(b.country))
		// After the block comes nowhere, until the next says otherwise;
		// unless the block ends with the last address there is.
		if b.to+1 != 0 {
			put(b.to+1, nowhere)
		}
	}
	return out
}

// Write writes the table in the form Read reads.
func (t *Table) Write(w io.Writer) error {
	packed := gzip.NewWriter(w)
	var b bytes.Buffer
	b.WriteString(magic)
	b.WriteByte(uint8(len(t.countries)))
	for _, c := range t.countries {
		b.WriteString(c)
	}
	binary.Write(&b, binary.BigEndian, uint32(len(t.v4)))
	for _, m := range t.v4 {
		binary.Write(&b, binary.BigEndian, m.from)
		b.WriteByte(m.country)
	}
	binary.Write(&b, binary.BigEndian, uint32(len(t.v6)))
	for _, m := range t.v6 {
		binary.Write(&b, binary.BigEndian, m.from)
		b.WriteByte(m.country)
	}
	if _, err := packed.Write(b.Bytes()); err != nil {
		return err
	}
	return packed.Close()
}

var errDamaged = errors.New("it is not a table of countries, or is a damaged one")

// Read reads a table that Write wrote.
func Read(r io.Reader) (*Table, error) {
	unpacked, err := gzip.NewReader(r)
	if err != nil {
		return nil, errDamaged
	}
	raw, err := io.ReadAll(unpacked)
	if err != nil || len(raw) < len(magic)+1 || string(raw[:len(magic)]) != magic {
		return nil, errDamaged
	}
	raw = raw[len(magic):]
	t := &Table{}
	n := int(raw[0])
	raw = raw[1:]
	if len(raw) < 2*n {
		return nil, errDamaged
	}
	for i := range n {
		t.countries = append(t.countries, string(raw[2*i:2*i+2]))
	}
	raw = raw[2*n:]
	if t.v4, raw, err = readMarks[uint32](raw, 4, n); err != nil {
		return nil, err
	}
	if t.v6, raw, err = readMarks[uint64](raw, 8, n); err != nil || len(raw) != 0 {
		return nil, errDamaged
	}
	return t, nil
}

// readMarks reads one list of marks, each width bytes of address and one
// of country, and returns what is left.
func readMarks[T uint32 | uint64](raw []byte, width, countries int) ([]mark[T], []byte, error) {
	if len(raw) < 4 {
		return nil, nil, errDamaged
	}
	n := int(binary.BigEndian.Uint32(raw))
	raw = raw[4:]
	if len(raw) < n*(width+1) {
		return nil, nil, errDamaged
	}
	out := make([]mark[T], n)
	for i := range out {
		at := raw[i*(width+1):]
		if width == 4 {
			out[i].from = T(binary.BigEndian.Uint32(at))
		} else {
			out[i].from = T(binary.BigEndian.Uint64(at))
		}
		out[i].country = at[width]
		if int(out[i].country) >= countries && out[i].country != nowhere {
			return nil, nil, errDamaged
		}
	}
	return out, raw[n*(width+1):], nil
}
