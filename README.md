# pdf

[![Go CI](https://github.com/chappihappymeal/pdf/actions/workflows/ci.yml/badge.svg)](https://github.com/chappihappymeal/pdf/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/chappihappymeal/pdf.svg)](https://pkg.go.dev/github.com/chappihappymeal/pdf)

A Go library for reading PDF files and extracting their text content — plain text, styled text, or text grouped by rows and columns.

This is a maintained continuation of [ledongthuc/pdf](https://github.com/ledongthuc/pdf), which itself continued the archived [rsc/pdf](https://github.com/rsc/pdf) by Russ Cox. It exists because I run this parser in production against a high volume of real-world PDFs (bank statements and receipts from dozens of different generators) and need decoder bugs fixed and merged, with regression tests, at a steady pace.

## Why this fork

- **Bug fixes ship.** Each fix lands with unit tests and synthetic reproduction PDFs in `testdata/` — the upstream repositories have open bug reports and pull requests waiting on review.
- **Tagged releases.** Versioned with semver from `v0.1.0`; upstream has no tags, so all of its importers depend on pseudo-versions.
- **Tested against a real-world corpus.** Changes are validated against a private regression corpus of bank-issued PDFs from many generators (OpenText Exstream, Ghostscript, Adobe tools and others) before release.

### Fixes over upstream

| Version | Fix |
|---------|-----|
| v0.2.0 | `readArray` looped forever appending `io.EOF` on truncated PDFs — unbounded allocation until OOM (absorbed [upstream PR #79](https://github.com/ledongthuc/pdf/pull/79) by @spencerkimball) |
| v0.2.0 | `readObject` panicked on a stray `]`, e.g. a dictionary with a missing value (absorbed the unique part of [upstream PR #64](https://github.com/ledongthuc/pdf/pull/64) by @yama6a) |
| v0.2.0 | A PDF object split across the streams of a `/Contents` array hung text extraction (absorbed [upstream PR #76](https://github.com/ledongthuc/pdf/pull/76) by @rztaylor), and the concatenated streams are now separated with whitespace — raw concatenation glued adjacent tokens together, corrupting coordinates or panicking `Page.Content()` with `bad Td` |
| v0.2.0 | A page whose `/Contents` has no stream data panicked out of `Page.Content()` and text extraction; it now degrades to an empty page (idea from [upstream PR #46](https://github.com/ledongthuc/pdf/pull/46) by @utsav82, reimplemented) |
| v0.2.0 | `readDict` printed `DEBUG: ...` to the caller's stdout on dictionaries with non-name keys |
| v0.1.0 | ASCII85 decoder: the `z` zero-group shorthand was silently stripped (4 bytes lost per group; corrupt output, `unexpected EOF` from chained FlateDecode, panic from `Page.Content()`), and the `~>` end-of-data marker never signalled EOF ([upstream issue #83](https://github.com/ledongthuc/pdf/issues/83), [PR #84](https://github.com/ledongthuc/pdf/pull/84)) |

Fixes are offered back to upstream as pull requests, and upstream's open bug-fix PRs are absorbed here with their original authorship preserved. Still in the queue: [upstream PR #78](https://github.com/ledongthuc/pdf/pull/78) (hardening against malformed and hostile inputs) is under review for the next release.

## Install

```sh
go get github.com/chappihappymeal/pdf
```

## Features

- Get plain text content (without format)
- Get Content (including all font and formatting information)

## Examples

See the `examples/` folder.

### Read plain text

```golang
package main

import (
	"bytes"
	"fmt"

	"github.com/chappihappymeal/pdf"
)

func main() {
	pdf.DebugOn = true

	f, r, err := pdf.Open("./pdf_test.pdf")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	b, err := r.GetPlainText()
	if err != nil {
		panic(err)
	}
	buf.ReadFrom(b)
	content := buf.String()
	fmt.Println(content)
}
```

### Read all text with styles

```golang
package main

import (
	"fmt"

	"github.com/chappihappymeal/pdf"
)

func main() {
	f, r, err := pdf.Open("./pdf_test.pdf")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	sentences, err := r.GetStyledTexts()
	if err != nil {
		panic(err)
	}

	// Print all sentences
	for _, sentence := range sentences {
		fmt.Printf("Font: %s, Font-size: %f, x: %f, y: %f, content: %s \n",
			sentence.Font,
			sentence.FontSize,
			sentence.X,
			sentence.Y,
			sentence.S)
	}
}
```

### Read text grouped by rows

```golang
package main

import (
	"fmt"
	"os"

	"github.com/chappihappymeal/pdf"
)

func main() {
	content, err := readPdf(os.Args[1]) // Read local pdf file
	if err != nil {
		panic(err)
	}
	fmt.Println(content)
}

func readPdf(path string) (string, error) {
	f, r, err := pdf.Open(path)
	defer func() {
		_ = f.Close()
	}()
	if err != nil {
		return "", err
	}
	totalPage := r.NumPage()

	for pageIndex := 1; pageIndex <= totalPage; pageIndex++ {
		p := r.Page(pageIndex)
		if p.V.IsNull() || p.V.Key("Contents").Kind() == pdf.Null {
			continue
		}

		rows, _ := p.GetTextByRow()
		for _, row := range rows {
			println(">>>> row: ", row.Position)
			for _, word := range row.Content {
				fmt.Println(word.S)
			}
		}
	}
	return "", nil
}
```

## Lineage and credits

- [rsc/pdf](https://github.com/rsc/pdf) — the original PDF reader by Russ Cox (archived).
- [ledongthuc/pdf](https://github.com/ledongthuc/pdf) — Thuc Le's fork that kept the library alive and added styled-text extraction; this repository is forked from it.
- Contributors of upstream bug-fix PRs are credited in the commits that absorb their work.

## License

BSD-style, see [LICENSE](LICENSE) — unchanged from the original Go Authors license.
