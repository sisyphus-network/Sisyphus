package citytable

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// fixture is a small table in DB-IP's form: a range placed nowhere, two
// cities, a gap, two rows of one place, then IPv6, with a /64 split between
// ranges and a last range that runs to the end of the address space.
const fixture = `0.0.0.0,0.255.255.255,ZZ,ZZ,,,0,0
1.0.0.0,1.0.0.255,OC,AU,Queensland,"South Brisbane",-27.4767,153.017
1.0.1.0,1.0.3.255,AS,CN,Fujian,Wenquan,26.0998,119.297
1.0.8.0,1.0.15.255,EU,GR,"Central Macedonia","Ampelokipoi, Thessaloniki",40.653,22.9262
1.0.16.0,1.0.31.255,EU,GR,"Central Macedonia","Ampelokipoi, Thessaloniki",40.653,22.9262
2001:4860::,2001:4860:ffff:ffff:ffff:ffff:ffff:ffff,NA,US,California,"Mountain View",37.422,-122.085
2001:4861::,2001:4861::7fff,AF,MZ,"Maputo City","Maputo (Polana Cimento""A"")",-25.9771,32.5939
2001:4861::8000,2001:4861::ffff,EU,FR,,,46,2
2001:4861::1:0,2001:4861:0:1:ffff:ffff:ffff:ffff,EU,CH,Fribourg,Murten/Morat,46.9219,7.16595
2001:4861:0:2::,ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff,,,,,0,0
`

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write([]byte(text))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var (
	brisbane     = Place{City: "South Brisbane", Region: "Queensland", Country: "AU", Latitude: -27.4767, Longitude: 153.017}
	wenquan      = Place{City: "Wenquan", Region: "Fujian", Country: "CN", Latitude: 26.0998, Longitude: 119.297}
	thessaloniki = Place{City: "Ampelokipoi, Thessaloniki", Region: "Central Macedonia", Country: "GR", Latitude: 40.653, Longitude: 22.9262}
	mountainView = Place{City: "Mountain View", Region: "California", Country: "US", Latitude: 37.422, Longitude: -122.085}
	maputo       = Place{City: `Maputo (Polana Cimento"A")`, Region: "Maputo City", Country: "MZ", Latitude: -25.9771, Longitude: 32.5939}
	murten       = Place{City: "Murten/Morat", Region: "Fribourg", Country: "CH", Latitude: 46.9219, Longitude: 7.16595}
)

// checkFixture checks what a table read from fixture says of addresses at
// and either side of each of its boundaries.
func checkFixture(t *testing.T, table *Table) {
	t.Helper()
	for addr, want := range map[string]*Place{
		"1.0.0.0":          &brisbane,
		"1.0.0.255":        &brisbane,
		"::ffff:1.0.0.7":   &brisbane, // IPv4 written as IPv6
		"1.0.1.0":          &wenquan,
		"1.0.3.255":        &wenquan,
		"1.0.4.0":          nil, // the gap
		"1.0.7.255":        nil,
		"1.0.8.0":          &thessaloniki,
		"1.0.31.255":       &thessaloniki,
		"1.0.32.0":         nil, // past the last IPv4 row
		"203.0.112.1":      nil,
		"10.1.2.3":         nil, // no one's on the internet
		"2001:485f::1":     nil, // before the first IPv6 row
		"2001:4860::":      &mountainView,
		"2001:4860:ffff::": &mountainView,
		// The /64 that holds Maputo's first address is Maputo's, the
		// part of it in France too.
		"2001:4861::1":       &maputo,
		"2001:4861::9000":    &maputo,
		"2001:4861:0:1::":    &murten,
		"2001:4861:0:1:ff::": &murten,
		"2001:4861:0:2::1":   nil, // a row placed nowhere
		"2a00::1":            nil,
	} {
		got, ok := table.Lookup(netip.MustParseAddr(addr))
		if want == nil {
			if ok {
				t.Errorf("%s is placed at %+v; want nowhere", addr, got)
			}
			continue
		}
		if !ok || got != *want {
			t.Errorf("%s is placed at %+v, %v; want %+v", addr, got, ok, *want)
		}
	}
}

func TestATableIsReadPlainOrGzipped(t *testing.T) {
	for name, input := range map[string][]byte{"plain": []byte(fixture), "gzipped": gzipped(t, fixture)} {
		t.Run(name, func(t *testing.T) {
			table, err := Parse(bytes.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			checkFixture(t, table)
			// The two rows of one place make one mark, and the /64 of
			// France's range none of its own.
			if v4, v6, places := table.Size(); v4 != 6 || v6 != 5 || places != 7 {
				t.Errorf("size = %d, %d, %d; want 6, 5, 7", v4, v6, places)
			}
		})
	}
}

func TestRowsThatCannotBeReadAreRefusedWithTheirLine(t *testing.T) {
	good := "1.0.0.0,1.0.0.255,OC,AU,Queensland,Brisbane,-27.4767,153.017\n"
	for row, want := range map[string]string{
		"1.0.1.0,1.0.1.255,AS,CN,Fujian,Wenquan,26.0998\n":           "record on line 2: wrong number of fields",
		"1.0.1.0,\"1.0.1.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n": "line 2",
		"1.0.1.x,1.0.1.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n":   "line 2: the range's start",
		"1.0.1.0,1.0.1.256,AS,CN,Fujian,Wenquan,26.0998,119.297\n":   "line 2: the range's end",
		"1.0.1.0,2001:db8::1,AS,CN,Fujian,Wenquan,26.0998,119.297\n": "line 2: the range starts and ends on different kinds",
		"1.0.1.9,1.0.1.0,AS,CN,Fujian,Wenquan,26.0998,119.297\n":     "line 2: the range ends before it starts",
		"1.0.1.0,1.0.1.255,AS,CN,Fujian,Wenquan,north,119.297\n":     "line 2: the latitude",
		"1.0.1.0,1.0.1.255,AS,CN,Fujian,Wenquan,26.0998,east\n":      "line 2: the longitude",
		"1.0.1.0,1.0.1.255,AS,CN,Fujian,Wenquan,91,119.297\n":        "line 2: latitude must be",
		"1.0.0.128,1.0.1.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n": "line 2: the range starts before the one above it ends",
		"0.0.0.0,0.0.0.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n":   "line 2: the range starts before",
		"1.0.1.0,255.255.255.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n" + // the last address
			"1.0.2.0,1.0.2.255,AS,CN,Fujian,Wenquan,26.0998,119.297\n": "line 3: the range starts before",
	} {
		_, err := Parse(strings.NewReader(good + row))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("reading %q gave %v; want an error with %q", row, err, want)
		}
	}
	for name, input := range map[string][]byte{
		"empty":              nil,
		"bad gzip":           {0x1f, 0x8b, 0, 0},
		"truncated gzip":     gzipped(t, fixture)[:40],
		"a header row":       []byte("start,end,continent,country,region,city,latitude,longitude\n"),
		"only an empty line": []byte("\n"),
	} {
		if _, err := Parse(bytes.NewReader(input)); err == nil || !strings.HasPrefix(err.Error(), "read the city table") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestATableWrittenReadsBackTheSame(t *testing.T) {
	// More rows than one block of the file, so that it is written and
	// read in several.
	var csv strings.Builder
	csv.WriteString(fixture[:strings.Index(fixture, "2001:")])
	for i := range 20000 {
		fmt.Fprintf(&csv, "2.%d.%d.0,2.%d.%d.255,EU,DE,Region %d,City %d,50,%d\n", i>>8, i&255, i>>8, i&255, i%7, i, i%180)
	}
	csv.WriteString(fixture[strings.Index(fixture, "2001:"):])
	table, err := Parse(strings.NewReader(csv.String()))
	if err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := table.Write(&file, "2026-10"); err != nil {
		t.Fatal(err)
	}
	read, edition, err := Read(bytes.NewReader(file.Bytes()))
	if err != nil || edition != "2026-10" {
		t.Fatalf("read %q, %v", edition, err)
	}
	checkFixture(t, read)
	if got, ok := read.Lookup(netip.MustParseAddr("2.78.31.200")); !ok || got != (Place{City: "City 19999", Region: "Region 0", Country: "DE", Latitude: 50, Longitude: 19999 % 180}) {
		t.Errorf("2.78.31.200 = %+v, %v", got, ok)
	}
	if v4, _, places := read.Size(); v4 != 20007 || places != 20007 {
		t.Errorf("size %d, %d", v4, places)
	}
}

func TestAFileThatIsNotAWholeTableIsRefused(t *testing.T) {
	table, err := Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	table.Write(&file, "2026-10")
	whole := file.Bytes()
	header := len(magic) + 1 + len("2026-10")
	for name, damaged := range map[string][]byte{
		"empty":                    nil,
		"another file":             []byte("SGEO1 and so on"),
		"cut in the edition":       whole[:len(magic)+3],
		"cut in the counts":        whole[:header+5],
		"cut in a name's length":   whole[:header+16+2],
		"cut in a name":            whole[:header+16+4+1],
		"cut in the arrays":        whole[:len(whole)-3],
		"a count past any table":   withUint32(whole, header, maxCount+1),
		"a name past any table":    withUint32(whole, header+16, 1<<20+1),
		"a mark of no known place": withUint32(whole, len(whole)-4, 7),
	} {
		if _, _, err := Read(bytes.NewReader(damaged)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// A place that names a name past the end.
	table.city[0] = uint32(len(table.names))
	file.Reset()
	table.Write(&file, "2026-10")
	if _, _, err := Read(&file); err != errDamaged {
		t.Errorf("a place of no known name: %v", err)
	}
}

func withUint32(b []byte, at int, v uint32) []byte {
	b = bytes.Clone(b)
	b[at], b[at+1], b[at+2], b[at+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	return b
}

// A table of IPv4 rows alone places no IPv6 address.
func TestATableOfOneKindOfAddressPlacesNoneOfTheOther(t *testing.T) {
	table, err := Parse(strings.NewReader(fixture[:strings.Index(fixture, "2001:")]))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := table.Lookup(netip.MustParseAddr("2001:4860::1")); ok {
		t.Errorf("placed at %+v", got)
	}
	if got, ok := table.Lookup(netip.MustParseAddr("1.0.0.1")); !ok || got != brisbane {
		t.Errorf("1.0.0.1 placed at %+v, %v", got, ok)
	}
}
