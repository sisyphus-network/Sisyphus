package citytable

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// The table is kept on disk in a form of its own, which loads in a
// fraction of the time the CSV takes to read. All numbers are little-
// endian:
//
//	magic "SCITY1\n"
//	edition: a length byte and that many bytes, the month DB-IP published it, 2026-10
//	counts:  uint32 names, places, IPv4 marks, IPv6 marks
//	names:   for each, a uint32 length and that many bytes
//	places:  float32 latitudes, float32 longitudes, uint32 country, region and city name indexes
//	IPv4:    uint32 starts, uint32 places
//	IPv6:    uint64 starts (the first 64 bits), uint32 places
const magic = "SCITY1\n"

// maxCount is more of anything than a table has: DB-IP's has some four
// million of its most numerous. A larger count means a damaged file, and
// is refused before memory is set aside for it.
const maxCount = 1 << 26

// errDamaged is what a file that is not a whole table gives.
var errDamaged = errors.New("the city table's file is damaged; download it again")

// Write writes the table in the node's own form, with the edition it is.
func (t *Table) Write(w io.Writer, edition string) error {
	out := bufio.NewWriterSize(w, 1<<16)
	out.WriteString(magic)
	edition = edition[:min(len(edition), 255)]
	out.WriteByte(byte(len(edition)))
	out.WriteString(edition)
	put := func(values ...uint32) {
		for _, v := range values {
			out.Write(binary.LittleEndian.AppendUint32(nil, v))
		}
	}
	put(uint32(len(t.names)), uint32(len(t.lat)), uint32(len(t.v4.from)), uint32(len(t.v6.from)))
	for _, name := range t.names {
		put(uint32(len(name)))
		out.WriteString(name)
	}
	writeFloats(out, t.lat)
	writeFloats(out, t.lon)
	for _, column := range [][]uint32{t.country, t.region, t.city, t.v4.from, t.v4.place} {
		writeInts(out, column)
	}
	writeInts(out, t.v6.from)
	writeInts(out, t.v6.place)
	return out.Flush()
}

// Read reads a table written by Write, and the edition it is.
func Read(r io.Reader) (*Table, string, error) {
	in := bufio.NewReaderSize(r, 1<<16)
	edition, err := readHeader(in)
	if err != nil {
		return nil, "", err
	}
	var counts [4]uint32
	if err := binary.Read(in, binary.LittleEndian, &counts); err != nil {
		return nil, "", errDamaged
	}
	names, places, v4, v6 := int(counts[0]), int(counts[1]), int(counts[2]), int(counts[3])
	if max(names, places, v4, v6) > maxCount {
		return nil, "", errDamaged
	}
	t := &Table{names: make([]string, names)}
	for i := range t.names {
		var size uint32
		if err := binary.Read(in, binary.LittleEndian, &size); err != nil || size > 1<<20 {
			return nil, "", errDamaged
		}
		name := make([]byte, size)
		if _, err := io.ReadFull(in, name); err != nil {
			return nil, "", errDamaged
		}
		t.names[i] = string(name)
	}
	d := &decoder{r: in}
	t.lat, t.lon = d.floats(places), d.floats(places)
	t.country, t.region, t.city = d.ints32(places), d.ints32(places), d.ints32(places)
	t.v4.from, t.v4.place = d.ints32(v4), d.ints32(v4)
	t.v6.from, t.v6.place = d.ints64(v6), d.ints32(v6)
	if d.err != nil {
		return nil, "", errDamaged
	}
	// An index past the end would be a damaged file, and a panic later.
	for _, column := range [][]uint32{t.country, t.region, t.city} {
		if !below(column, uint32(names)) {
			return nil, "", errDamaged
		}
	}
	if !placed(t.v4.place, uint32(places)) || !placed(t.v6.place, uint32(places)) {
		return nil, "", errDamaged
	}
	return t, edition, nil
}

// readHeader reads the start of a file, up to its edition.
func readHeader(in *bufio.Reader) (string, error) {
	head := make([]byte, len(magic)+1)
	if _, err := io.ReadFull(in, head); err != nil || string(head[:len(magic)]) != magic {
		return "", fmt.Errorf("not a city table in the node's form")
	}
	edition := make([]byte, head[len(magic)])
	if _, err := io.ReadFull(in, edition); err != nil {
		return "", errDamaged
	}
	return string(edition), nil
}

func below(indexes []uint32, n uint32) bool {
	for _, i := range indexes {
		if i >= n {
			return false
		}
	}
	return true
}

func placed(indexes []uint32, n uint32) bool {
	for _, i := range indexes {
		if i >= n && i != nowhere {
			return false
		}
	}
	return true
}

func writeFloats(out *bufio.Writer, values []float32) {
	bits := make([]uint32, len(values))
	for i, v := range values {
		bits[i] = math.Float32bits(v)
	}
	writeInts(out, bits)
}

func writeInts[T uint32 | uint64](out *bufio.Writer, values []T) {
	buf := make([]byte, 0, 1<<16)
	for _, v := range values {
		if len(buf)+8 > cap(buf) {
			out.Write(buf)
			buf = buf[:0]
		}
		switch v := any(v).(type) {
		case uint32:
			buf = binary.LittleEndian.AppendUint32(buf, v)
		case uint64:
			buf = binary.LittleEndian.AppendUint64(buf, v)
		}
	}
	out.Write(buf)
}

// decoder reads arrays of numbers a block at a time, keeping the first
// error.
type decoder struct {
	r   io.Reader
	buf [1 << 16]byte
	err error
}

// read fills out with n numbers of size bytes each, decoded by put.
func (d *decoder) read(n, size int, put func(i int, b []byte)) {
	for i := 0; i < n && d.err == nil; {
		k := min(n-i, len(d.buf)/size)
		if _, d.err = io.ReadFull(d.r, d.buf[:k*size]); d.err != nil {
			return
		}
		for j := range k {
			put(i+j, d.buf[j*size:])
		}
		i += k
	}
}

func (d *decoder) ints32(n int) []uint32 {
	out := make([]uint32, n)
	d.read(n, 4, func(i int, b []byte) { out[i] = binary.LittleEndian.Uint32(b) })
	return out
}

func (d *decoder) ints64(n int) []uint64 {
	out := make([]uint64, n)
	d.read(n, 8, func(i int, b []byte) { out[i] = binary.LittleEndian.Uint64(b) })
	return out
}

func (d *decoder) floats(n int) []float32 {
	out := make([]float32, n)
	d.read(n, 4, func(i int, b []byte) { out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b)) })
	return out
}
