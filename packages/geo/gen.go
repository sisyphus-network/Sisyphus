//go:build ignore

// This makes countries.bin.gz from the regional Internet registries' files.
// Run it from this directory, with the files' names as arguments, or use
// scripts/update-geo.sh, which fetches them first.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sisyphus-network/Sisyphus/packages/geo"
)

func main() {
	var sources []io.Reader
	for _, name := range os.Args[1:] {
		f, err := os.Open(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		sources = append(sources, f)
	}
	table, err := geo.Compile(sources...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := os.Create("countries.bin.gz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := table.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out.Close()
	v4, v6 := table.Size()
	fmt.Printf("countries.bin.gz: %d IPv4 and %d IPv6 boundaries\n", v4, v6)
}
