// Package citytable says which city an Internet address is in, from DB-IP's
// free "IP to City Lite" table, which the node's owner downloads on request
// (https://db-ip.com/db/download/ip-to-city-lite). The table is published
// monthly under the Creative Commons Attribution 4.0 licence, which asks
// that wherever what it says is used, it is credited: Attribution, with a
// link to AttributionURL.
//
// What it gives is an estimate. An address is placed where its network is
// registered or routed from, which for a home connection is often the
// provider's nearest city, tens of kilometres from the machine, and now
// and then further. Looking an address up asks no one: the table is on
// the machine. Downloading it shows db-ip.com the machine's address.
package citytable

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/sisyphus-network/Sisyphus/packages/geo"
)

// Attribution and AttributionURL are the credit the table's licence asks
// for, shown wherever what it says is used.
const (
	Attribution    = "IP Geolocation by DB-IP"
	AttributionURL = "https://db-ip.com"
)

// Place is where the table puts an address: a city, the region it is in,
// the two-letter code of its country, and its coordinates in degrees,
// north and east positive. City or region may be empty.
type Place struct {
	City, Region, Country string
	Latitude, Longitude   float64
}

// Table is a table of address ranges and the places they are in, kept as
// parallel arrays so that millions of ranges cost little more than their
// numbers. Places are kept once each, and their names once each.
type Table struct {
	v4 marks[uint32]
	// IPv6 addresses are told apart by their first 64 bits, the network a
	// provider hands a home or a site: a /64 is placed by the range that
	// holds its first address.
	v6 marks[uint64]
	// The places, by index: their coordinates, and the names of their
	// country, region and city as indexes into names.
	lat, lon              []float32
	country, region, city []uint32
	names                 []string
}

// marks are where the place changes, in order: from each address in from
// on, the place of the same index in place, or nowhere.
type marks[T uint32 | uint64] struct {
	from  []T
	place []uint32
}

// nowhere is the place of addresses the table does not place.
const nowhere = math.MaxUint32

// Lookup returns where the table puts addr, and false if it puts it
// nowhere or addr is no one's address on the internet (see geo.Global).
func (t *Table) Lookup(addr netip.Addr) (Place, bool) {
	addr = addr.Unmap()
	if !geo.Global(addr) {
		return Place{}, false
	}
	raw := addr.AsSlice()
	if addr.Is4() {
		return t.place(find(t.v4, binary.BigEndian.Uint32(raw)))
	}
	return t.place(find(t.v6, binary.BigEndian.Uint64(raw[:8])))
}

// Size returns how many places in the table a place begins, for each kind
// of address, and how many places there are.
func (t *Table) Size() (v4, v6, places int) {
	return len(t.v4.from), len(t.v6.from), len(t.lat)
}

// find returns the place of the last mark at or before at.
func find[T uint32 | uint64](m marks[T], at T) uint32 {
	after := sort.Search(len(m.from), func(i int) bool { return m.from[i] > at })
	if after == 0 {
		return nowhere
	}
	return m.place[after-1]
}

func (t *Table) place(i uint32) (Place, bool) {
	if i == nowhere {
		return Place{}, false
	}
	return Place{
		City: t.names[t.city[i]], Region: t.names[t.region[i]], Country: t.names[t.country[i]],
		Latitude: degrees(t.lat[i]), Longitude: degrees(t.lon[i]),
	}, true
}

// degrees gives a coordinate kept to float32's precision, a metre or so,
// as the shortest decimal that reads back as it: -27.4767 rather than
// -27.476699829101562.
func degrees(f float32) float64 {
	d, _ := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	return d
}

// Parse reads a table as DB-IP publishes it, a CSV file, gzipped or not,
// with a row for each range of addresses, in order and not overlapping:
//
//	start,end,continent,country,region,city,latitude,longitude
//
// with IPv4 rows before or after IPv6 ones. A row whose country is ZZ or
// empty places its range nowhere, as do the gaps between rows. A row that
// cannot be read is refused, with its line.
func Parse(r io.Reader) (*Table, error) {
	in := bufio.NewReaderSize(r, 1<<16)
	if head, _ := in.Peek(2); len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		unzipped, err := gzip.NewReader(in)
		if err != nil {
			return nil, fmt.Errorf("read the city table: %w", err)
		}
		r = unzipped
	} else {
		r = in
	}
	rows := csv.NewReader(r)
	rows.FieldsPerRecord = 8
	rows.ReuseRecord = true
	b := newBuilder()
	v4 := family[uint32]{zero: netip.IPv4Unspecified(), last: netip.AddrFrom4([4]byte{255, 255, 255, 255}), span: span4}
	v6 := family[uint64]{zero: netip.IPv6Unspecified(), last: netip.AddrFrom16([16]byte{0: 255, 1: 255, 2: 255, 3: 255, 4: 255, 5: 255, 6: 255, 7: 255, 8: 255, 9: 255, 10: 255, 11: 255, 12: 255, 13: 255, 14: 255, 15: 255}), span: span6}
	for {
		row, err := rows.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the city table: %w", err)
		}
		line, _ := rows.FieldPos(0)
		start, end, place, err := b.row(row)
		if err == nil {
			if start.Is4() {
				err = v4.add(start, end, place)
			} else {
				err = v6.add(start, end, place)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read the city table: line %d: %w", line, err)
		}
	}
	if !v4.started && !v6.started {
		return nil, errors.New("read the city table: it has no rows")
	}
	v4.finish()
	v6.finish()
	b.t.v4, b.t.v6 = v4.marks, v6.marks
	return b.t, nil
}

// builder keeps the places and names of a table being read, each once.
type builder struct {
	t      *Table
	names  map[string]uint32
	places map[placeKey]uint32
}

type placeKey struct {
	country, region, city uint32
	lat, lon              float32
}

func newBuilder() *builder {
	return &builder{t: &Table{}, names: map[string]uint32{}, places: map[placeKey]uint32{}}
}

// row reads a row's range and place.
func (b *builder) row(row []string) (start, end netip.Addr, place uint32, err error) {
	if start, err = netip.ParseAddr(row[0]); err != nil {
		return start, end, 0, fmt.Errorf("the range's start: %w", err)
	}
	if end, err = netip.ParseAddr(row[1]); err != nil {
		return start, end, 0, fmt.Errorf("the range's end: %w", err)
	}
	if start.Is4() != end.Is4() {
		return start, end, 0, errors.New("the range starts and ends on different kinds of address")
	}
	if end.Less(start) {
		return start, end, 0, errors.New("the range ends before it starts")
	}
	lat, err := strconv.ParseFloat(row[6], 64)
	if err != nil {
		return start, end, 0, fmt.Errorf("the latitude: %w", err)
	}
	lon, err := strconv.ParseFloat(row[7], 64)
	if err != nil {
		return start, end, 0, fmt.Errorf("the longitude: %w", err)
	}
	if err := geo.CheckCoordinates(lat, lon); err != nil {
		return start, end, 0, err
	}
	if row[3] == "" || row[3] == "ZZ" {
		return start, end, nowhere, nil
	}
	key := placeKey{country: b.name(row[3]), region: b.name(row[4]), city: b.name(row[5]), lat: float32(lat), lon: float32(lon)}
	place, known := b.places[key]
	if !known {
		place = uint32(len(b.t.lat))
		b.places[key] = place
		b.t.lat, b.t.lon = append(b.t.lat, key.lat), append(b.t.lon, key.lon)
		b.t.country, b.t.region, b.t.city = append(b.t.country, key.country), append(b.t.region, key.region), append(b.t.city, key.city)
	}
	return start, end, place, nil
}

// name returns the index of a name, keeping a copy of it the first time:
// the reader's row shares its memory with the rest of the line.
func (b *builder) name(s string) uint32 {
	n, known := b.names[s]
	if !known {
		n = uint32(len(b.t.names))
		s = strings.Clone(s)
		b.names[s] = n
		b.t.names = append(b.t.names, s)
	}
	return n
}

// family builds the marks of one kind of address from its rows in order.
type family[T uint32 | uint64] struct {
	zero, last netip.Addr
	// span gives the run of positions in marks that a range of addresses
	// places, if any.
	span    func(start, end netip.Addr) (from T, ok bool)
	marks   marks[T]
	started bool
	// next is the first address no row has covered yet, and full says
	// that the rows have covered the last one.
	next netip.Addr
	full bool
}

func (f *family[T]) add(start, end netip.Addr, place uint32) error {
	if f.started && (f.full || start.Less(f.next)) {
		return errors.New("the range starts before the one above it ends: the rows must be in order and not overlap")
	}
	gap := f.zero
	if f.started {
		gap = f.next
	}
	if start != gap {
		f.cover(gap, start.Prev(), nowhere)
	}
	f.cover(start, end, place)
	f.started = true
	f.full = end == f.last
	f.next = end.Next()
	return nil
}

// finish places the addresses after the last row nowhere.
func (f *family[T]) finish() {
	if f.started && !f.full {
		f.cover(f.next, f.last, nowhere)
	}
}

func (f *family[T]) cover(start, end netip.Addr, place uint32) {
	from, ok := f.span(start, end)
	if !ok {
		return
	}
	if n := len(f.marks.place); n > 0 && f.marks.place[n-1] == place {
		return
	}
	f.marks.from = append(f.marks.from, from)
	f.marks.place = append(f.marks.place, place)
}

func span4(start, _ netip.Addr) (uint32, bool) {
	return binary.BigEndian.Uint32(start.AsSlice()), true
}

// span6 gives the first /64 whose first address is in the range, if any
// is: a /64 is placed by the range that holds its first address.
func span6(start, end netip.Addr) (uint64, bool) {
	s, e := start.As16(), end.As16()
	first, last := binary.BigEndian.Uint64(s[:8]), binary.BigEndian.Uint64(e[:8])
	if binary.BigEndian.Uint64(s[8:]) != 0 {
		if first == last {
			return 0, false
		}
		first++
	}
	return first, true
}
