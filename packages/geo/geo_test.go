package geo

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestTheBuiltInTablePlacesWellKnownAddresses(t *testing.T) {
	for addr, want := range map[string]string{
		"8.8.8.8":                   "US", // Google
		"193.0.6.139":               "NL", // the RIPE NCC
		"2001:4860:4860::8888":      "US",
		"2001:67c:2e8:22::c100:68b": "NL",
		"::ffff:8.8.8.8":            "US", // an IPv4 address written as IPv6
		// Addresses with no place in the world.
		"127.0.0.1": "", "10.1.2.3": "", "192.168.1.10": "", "169.254.0.5": "", "::1": "", "fe80::1": "", "fd00::1": "", "0.0.0.0": "", "224.0.0.1": "",
	} {
		if got := Country(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Country(%s) = %q, want %q", addr, got, want)
		}
	}
	if got := Country(netip.Addr{}); got != "" {
		t.Errorf("no address at all is in %q", got)
	}
	if v4, v6 := builtin().Size(); v4 < 100000 || v6 < 50000 {
		t.Errorf("the built-in table has %d and %d boundaries", v4, v6)
	}
}

const registry = `2|test|20261007|6|19700101|20261007|+0000
test|*|ipv4|*|4|summary
test|FR|ipv4|5.0.0.0|256|20200101|allocated|a
test|FR|ipv4|5.0.1.0|256|20200101|assigned|a
test|DE|ipv4|5.0.2.0|512|20200101|allocated|b
test|JP|ipv4|5.0.9.0|256|20200101|allocated|c
test|ZZ|ipv4|5.1.0.0|256|20200101|allocated|d
test||ipv4|5.2.0.0|256||available|
test|US|ipv4|5.3.0.0|256|20200101|reserved|e
test|US|ipv4|not-an-address|256|20200101|allocated|e
test|US|ipv4|5.4.0.0|many|20200101|allocated|e
test|US|ipv4|5.5.0.0|0|20200101|allocated|e
test|US|ipv4|255.255.255.0|256|20200101|allocated|e
test|BR|ipv6|2a00:1::|32|20200101|allocated|f
test|BR|ipv6|2a00:2::|96|20200101|allocated|f
test|AR|ipv6|ffff:ffff:ffff:ffff::|64|20200101|allocated|g
test|SE|ipv4|2a00:3::|32|20200101|allocated|h
`

func TestATableIsCompiledFromWhatRegistriesPublish(t *testing.T) {
	// A second registry hands out a block that begins where one of the
	// first's does: the later one stands.
	table, err := Compile(strings.NewReader(registry), strings.NewReader("other|IT|ipv4|5.0.9.0|256|20200101|allocated|z\n"))
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{
		"5.0.0.0": "FR", "5.0.1.255": "FR", "5.0.2.0": "DE", "5.0.3.255": "DE",
		"5.0.4.0": "", "5.0.8.255": "", "5.0.9.7": "IT", "5.0.10.0": "",
		"4.255.255.255": "", "5.1.0.1": "", "5.2.0.1": "", "5.3.0.1": "", "5.5.0.0": "",
		"255.255.255.255": "US",
		"2a00:1::1":       "BR", "2a00:1:ffff:ffff:ffff::": "BR", "2a00:2::1": "", "2a00:0::1": "",
	} {
		if got := table.Country(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Country(%s) = %q, want %q", addr, got, want)
		}
	}
	// Two blocks of one country side by side are one stretch.
	if v4, v6 := table.Size(); v4 != 6 || v6 != 3 {
		t.Errorf("the table has %d and %d boundaries", v4, v6)
	}

	// Written and read back, it says the same.
	var written bytes.Buffer
	if err := table.Write(&written); err != nil {
		t.Fatal(err)
	}
	back, err := Read(bytes.NewReader(written.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Country(netip.MustParseAddr("5.0.2.9")); got != "DE" {
		t.Errorf("read back, 5.0.2.9 is in %q", got)
	}
	if got := back.Country(netip.MustParseAddr("2a00:1::9")); got != "BR" {
		t.Errorf("read back, 2a00:1::9 is in %q", got)
	}
}

// failing is a file that cannot be read, or written.
type failing struct{}

var errDisk = errors.New("the disk has failed")

func (failing) Read([]byte) (int, error)  { return 0, errDisk }
func (failing) Write([]byte) (int, error) { return 0, errDisk }

func TestWhatCannotBeCompiledWrittenOrRead(t *testing.T) {
	if _, err := Compile(strings.NewReader("nothing here\n")); err == nil || !strings.Contains(err.Error(), "name no address blocks") {
		t.Errorf("files with no blocks: %v", err)
	}
	if _, err := Compile(failing{}); !errors.Is(err, errDisk) {
		t.Errorf("a file that cannot be read: %v", err)
	}
	var crowded strings.Builder
	for i := range 26 * 26 {
		crowded.WriteString("test|" + string(rune('A'+i/26)) + string(rune('A'+i%26)) + "|ipv4|6." + string(rune('0'+i/100)) + "." + string(rune('0'+i/10%10)) + string(rune('0'+i%10)) + ".0|256|20200101|allocated|x\n")
	}
	if _, err := Compile(strings.NewReader(crowded.String())); err == nil || !strings.Contains(err.Error(), "more than there are") {
		t.Errorf("more countries than a table holds: %v", err)
	}

	table, err := Compile(strings.NewReader(registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Write(failing{}); !errors.Is(err, errDisk) {
		t.Errorf("writing to a disk that fails: %v", err)
	}
	var good bytes.Buffer
	table.Write(&good)
	unpacked, _ := gzip.NewReader(bytes.NewReader(good.Bytes()))
	var raw bytes.Buffer
	raw.ReadFrom(unpacked)
	pack := func(b []byte) *bytes.Reader {
		var packed bytes.Buffer
		w := gzip.NewWriter(&packed)
		w.Write(b)
		w.Close()
		return bytes.NewReader(packed.Bytes())
	}
	whole := raw.Bytes()
	// A country numbered beyond those the table names.
	misnumbered := append([]byte(nil), whole...)
	misnumbered[len(magic)+1+2*int(whole[len(magic)])+4+4] = 200
	for name, damaged := range map[string]*bytes.Reader{
		"not packed":                  bytes.NewReader([]byte("plain")),
		"something else":              pack([]byte("PNG..")),
		"too short to be anything":    pack(nil),
		"cut off in the countries":    pack(whole[:len(magic)+2]),
		"cut off before the IPv4":     pack(whole[:len(magic)+1+2*int(whole[len(magic)])+2]),
		"cut off in the IPv4":         pack(whole[:len(magic)+1+2*int(whole[len(magic)])+9]),
		"cut off in the IPv6":         pack(whole[:len(whole)-3]),
		"with more after the end":     pack(append(append([]byte(nil), whole...), 0)),
		"a country that is not there": pack(misnumbered),
		"packed and cut short":        bytes.NewReader(good.Bytes()[:good.Len()-6]),
	} {
		if _, err := Read(damaged); !errors.Is(err, errDamaged) {
			t.Errorf("a table %s: %v", name, err)
		}
	}
}
