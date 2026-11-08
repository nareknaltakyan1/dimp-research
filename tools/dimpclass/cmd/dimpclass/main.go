// Command dimpclass classifies an update between two versions of a Go package
// or module and recommends a zero-downtime mechanism (S8).
//
//	dimpclass [-format md|json|both] [-o out] [-xform file.go] OLD_DIR NEW_DIR
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nareknaltakyan1/dimp-research/tools/dimpclass"
)

func main() {
	format := flag.String("format", "md", "output format: md, json or both (both writes OUT.json and OUT.md; needs -o)")
	out := flag.String("o", "", "output file (default stdout)")
	xform := flag.String("xform", "", "also write the state-transformer skeleton (if any) to this file")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dimpclass [flags] OLD_DIR NEW_DIR\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	r, err := dimpclass.Analyze(flag.Arg(0), flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dimpclass:", err)
		os.Exit(1)
	}
	write := func(name string, data []byte) {
		if name == "" {
			os.Stdout.Write(data)
			return
		}
		if err := os.WriteFile(name, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "dimpclass:", err)
			os.Exit(1)
		}
	}
	js, err := r.JSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dimpclass:", err)
		os.Exit(1)
	}
	switch *format {
	case "json":
		write(*out, js)
	case "md":
		write(*out, []byte(r.Markdown()))
	case "both":
		if *out == "" {
			fmt.Fprintln(os.Stderr, "dimpclass: -format both needs -o PREFIX")
			os.Exit(2)
		}
		write(*out+".json", js)
		write(*out+".md", []byte(r.Markdown()))
	default:
		fmt.Fprintln(os.Stderr, "dimpclass: unknown -format", *format)
		os.Exit(2)
	}
	if *xform != "" && r.StateXform != "" {
		write(*xform, []byte(r.StateXform))
	}
}
