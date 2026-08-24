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
| v0.1.0 | ASCII85 decoder: the `z` zero-group shorthand was silently stripped (4 bytes lost per group; corrupt output, `unexpected EOF` from chained FlateDecode, panic from `Page.Content()`), and the `~>` end-of-data marker never signalled EOF ([upstream issue #83](https://github.com/ledongthuc/pdf/issues/83), [PR #84](https://github.com/ledongthuc/pdf/pull/84)) |

Fixes are offered back to upstream as pull requests. A triage of upstream's open bug-fix PRs (hangs, OOMs, truncated-input handling) is in progress; confirmed fixes will be absorbed here with their original authorship preserved.

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
