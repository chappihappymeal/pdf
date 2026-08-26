package pdf

import (
	"bytes"
	"crypto/rc4"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// buildUnterminatedArrayPDF constructs a minimal single-page PDF whose
// content stream ends inside an array that is never closed. Real-world
// truncated or malformed PDFs exhibit the same shape: the tokenizer hits
// end of input while readArray is still collecting elements.
func buildUnterminatedArrayPDF() []byte {
	var buf bytes.Buffer
	offsets := make([]int, 5)

	buf.WriteString("%PDF-1.4\n")

	// The content stream ends inside "[ ... " with no closing "]".
	content := "BT /F1 12 Tf [ (hello) 1 2"
	objs := []string{
		"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents 4 0 R >>\nendobj\n",
		fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content),
	}
	for i, obj := range objs {
		offsets[i+1] = buf.Len()
		buf.WriteString(obj)
	}

	xrefOffset := buf.Len()
	buf.WriteString("xref\n0 5\n")
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	buf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n")
	fmt.Fprintf(&buf, "%d\n%%%%EOF\n", xrefOffset)
	return buf.Bytes()
}

// TestUnterminatedArrayTerminates verifies that text extraction terminates
// on a PDF whose content stream is truncated inside an unterminated array.
// Before readArray handled io.EOF, readToken returned io.EOF as a token
// value, which matched neither nil nor keyword("]"), so readArray appended
// io.EOF objects forever, allocating memory without bound.
func TestUnterminatedArrayTerminates(t *testing.T) {
	data := buildUnterminatedArrayPDF()
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The extracted text is irrelevant; the call just has to return.
		r.GetPlainText()
	}()

	select {
	case <-done:
		// ok: extraction terminated
	case <-time.After(5 * time.Second):
		t.Fatal("GetPlainText did not return within 5s: readArray is looping on io.EOF at end of a truncated content stream")
	}
}

// TestReadObjectStopsAtStrayCloseBracket verifies that readObject treats a
// stray "]" like ">>": as the end of the enclosing structure rather than a
// malformed-PDF panic. Real-world PDFs produce this shape via dictionaries
// with a missing value, e.g. << /A ] >>.
func TestReadObjectStopsAtStrayCloseBracket(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("readObject panicked on stray ]: %v", r)
		}
	}()

	b := newBuffer(strings.NewReader("<< /A ] >>"), 0)
	b.allowEOF = true
	obj := b.readObject()
	d, ok := obj.(dict)
	if !ok {
		t.Fatalf("expected dict, got %T (%v)", obj, obj)
	}
	if _, present := d[name("A")]; !present {
		t.Fatalf("expected key A in dict, got %v", d)
	}
}

// TestLiteralStringUnknownEscape pins PDF 32000-1, 7.3.4.2: a backslash before
// a character that is not an escape is ignored, so (a\qb) reads as "aqb"
// rather than being reported as malformed.
func TestLiteralStringUnknownEscape(t *testing.T) {
	b := newBuffer(strings.NewReader(`(a\qb) (x\yz)`), 0)
	b.allowEOF = true
	for _, want := range []string{"aqb", "xyz"} {
		got := b.readToken()
		if got != want {
			t.Errorf("readToken = %#v, want %q", got, want)
		}
	}
}

// lexer returns a buffer over src in content-stream mode: the end of input
// is expected, and "1 0 R" stays three tokens.
func lexer(src string) *buffer {
	b := newBuffer(strings.NewReader(src), 0)
	b.allowEOF = true
	b.allowObjptr = false
	b.allowStream = false
	return b
}

// fileLexer returns a buffer over src in file-object mode, with the end of
// input allowed so that a fragment can be tested on its own.
func fileLexer(src string) *buffer {
	b := newBuffer(strings.NewReader(src), 0)
	b.allowEOF = true
	b.allowObjptr = true
	b.allowStream = true
	return b
}

// mustPanicWith runs fn and requires it to panic with a message containing
// want; the lexer reports malformed input that way.
func mustPanicWith(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		e := recover()
		if e == nil {
			t.Fatalf("no panic, want one containing %q", want)
		}
		if got := fmt.Sprint(e); !strings.Contains(got, want) {
			t.Errorf("panic %q, want it to contain %q", got, want)
		}
	}()
	fn()
}

// TestLexTokens pins how each kind of token is read: the delimiters, names
// with #xx escapes, the classification of bare words into booleans, integers,
// reals and keywords, and what is skipped between tokens.
func TestLexTokens(t *testing.T) {
	tests := []struct {
		src  string
		want []token
	}{
		{"<< >> [ ] { }", []token{keyword("<<"), keyword(">>"), keyword("["), keyword("]"), keyword("{"), keyword("}")}},
		{"<</A/B>>", []token{keyword("<<"), name("A"), name("B"), keyword(">>")}},
		{"/Name /A#20B /#2F /", []token{name("Name"), name("A B"), name("/"), name("")}},
		{"true false", []token{true, false}},
		{"12 -3 +7 0", []token{int64(12), int64(-3), int64(7), int64(0)}},
		{"1.5 .5 1. -.5 +2.25", []token{1.5, 0.5, 1.0, -0.5, 2.25}},
		{"1e5 + - 12abc obj R", []token{keyword("1e5"), keyword("+"), keyword("-"), keyword("12abc"), keyword("obj"), keyword("R")}},
		{"a % comment to end of line\nb", []token{keyword("a"), keyword("b")}},
		{"a\x00b\tc\fd", []token{keyword("a"), keyword("b"), keyword("c"), keyword("d")}},
		{"<4A6F>(hi)", []token{"Jo", "hi"}},
		{"", []token{io.EOF}},
		{"   % only a comment", []token{io.EOF}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			b := lexer(tt.src)
			for i, want := range tt.want {
				if got := b.readToken(); got != want {
					t.Errorf("token %d = %#v (%T), want %#v (%T)", i, got, got, want, want)
				}
			}
			if got := b.readToken(); got != io.EOF && tt.want[len(tt.want)-1] != io.EOF {
				t.Errorf("trailing token %#v, want io.EOF", got)
			}
		})
	}
}

// TestLiteralStringEscapes pins the decoding of literal strings: every escape
// of PDF 32000-1 7.3.4.2, octal codes of one to three digits, line
// continuations, balanced parentheses and a string cut off by the end of
// input.
func TestLiteralStringEscapes(t *testing.T) {
	tests := []struct{ src, want string }{
		{`(a\nb)`, "a\nb"},
		{`(\r\t\b\f)`, "\r\t\b\f"},
		{`(\(\)\\)`, "()\\"},
		{`(\101)`, "A"},
		{`(\61)`, "1"},
		{`(\7)`, "\x07"},
		{`(\1012)`, "A2"},
		{`(\0053)`, "\x053"},
		{`(\8)`, "8"},
		{"(a\\\r\nb)", "ab"},
		{"(a\\\rb)", "ab"},
		{"(a\\\nb)", "ab"},
		{"(a(b)c)", "a(b)c"},
		{"(a((b))c)", "a((b))c"},
		{"(unterminated", "unterminated"},
		{"()", ""},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := lexer(tt.src).readToken(); got != tt.want {
				t.Errorf("readToken = %#v, want %q", got, tt.want)
			}
		})
	}
}

// TestLexMalformed pins that every check in the lexer fires, and the message
// it reports, since that message is what a caller sees in the error.
func TestLexMalformed(t *testing.T) {
	tests := []struct {
		name, src, want string
		read            func(b *buffer)
	}{
		{"lone >", "a > b", "unexpected delimiter", nil},
		{"stray )", "a ) b", "unexpected delimiter", nil},
		{"hex bad pair", "<4G>", "malformed hex string", nil},
		{"hex bad odd", "<G>", "malformed hex string", nil},
		{"name bad escape", "/A#ZZ", "malformed name", nil},
		{"integer overflow", "99999999999999999999", "invalid integer", nil},
		{"real just a dot", ".", "invalid real", nil},
		{"octal too large", `(\777)`, "invalid octal escape", nil},
		{"dict non-name key", "<< 1 2 >>", "unexpected non-name key", func(b *buffer) { b.readObject() }},
		{"object keyword", "endobj", "unexpected keyword", func(b *buffer) { b.readObject() }},
		{"missing endobj", "1 0 obj 5 6", "missing endobj", func(b *buffer) { b.readObject() }},
		{"stream no newline", "<< >> stream x", "stream keyword not followed by newline", func(b *buffer) { b.readObject() }},
		{"nesting", strings.Repeat("[", maxObjectNesting+1), "object nesting deeper", func(b *buffer) { b.readObject() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fileLexer(tt.src)
			mustPanicWith(t, tt.want, func() {
				for range 4 {
					if tt.read != nil {
						tt.read(b)
					} else {
						b.readToken()
					}
				}
			})
		})
	}
}

// TestReadObjectForms pins the grammar readObject recognises by looking
// ahead: references, definitions, and the push-back when a pair of integers
// turns out to be nothing more.
func TestReadObjectForms(t *testing.T) {
	t.Run("reference", func(t *testing.T) {
		if got := fileLexer("1 0 R").readObject(); got != (objptr{1, 0}) {
			t.Errorf("got %#v", got)
		}
	})
	t.Run("definition", func(t *testing.T) {
		got := fileLexer("1 0 obj 5 endobj").readObject()
		if got != (objdef{objptr{1, 0}, int64(5)}) {
			t.Errorf("got %#v", got)
		}
	})
	t.Run("definition of a stream needs no endobj", func(t *testing.T) {
		got, ok := fileLexer("7 2 obj << /Length 1 >> stream\nx\nendstream").readObject().(objdef)
		if !ok {
			t.Fatalf("got %#v, want objdef", got)
		}
		s, ok := got.obj.(stream)
		if !ok || s.ptr != (objptr{7, 2}) || s.hdr[name("Length")] != int64(1) {
			t.Errorf("got %#v", got.obj)
		}
	})
	t.Run("two integers pushed back", func(t *testing.T) {
		b := fileLexer("1 0 5 1 0 Tj")
		want := []object{int64(1), int64(0), int64(5), int64(1), int64(0)}
		for i, w := range want {
			if got := b.readObject(); got != w {
				t.Errorf("object %d = %#v, want %#v", i, got, w)
			}
		}
	})
	t.Run("number too large for a reference", func(t *testing.T) {
		b := fileLexer("5000000000 0 R")
		if got := b.readObject(); got != int64(5000000000) {
			t.Errorf("got %#v, want the integer itself", got)
		}
	})
	t.Run("generation too large for a reference", func(t *testing.T) {
		b := fileLexer("1 70000 R")
		if got := b.readObject(); got != int64(1) {
			t.Errorf("got %#v, want the integer itself", got)
		}
	})
	t.Run("null", func(t *testing.T) {
		if got := fileLexer("null").readObject(); got != nil {
			t.Errorf("got %#v, want nil", got)
		}
	})
	t.Run("closers read as null", func(t *testing.T) {
		if got := fileLexer("<< /A >> ").readObject(); !reflect.DeepEqual(got, dict{name("A"): nil}) {
			t.Errorf("got %#v", got)
		}
	})
	t.Run("references off in content streams", func(t *testing.T) {
		b := lexer("1 0 R")
		for i, w := range []object{int64(1), int64(0)} {
			if got := b.readObject(); got != w {
				t.Errorf("object %d = %#v, want %#v", i, got, w)
			}
		}
		if got := b.readToken(); got != keyword("R") {
			t.Errorf("got %#v, want the R keyword left as an operator", got)
		}
	})
	t.Run("nested", func(t *testing.T) {
		got := fileLexer("[ 1 << /K [ (s) ] >> /N ]").readObject()
		want := array{int64(1), dict{name("K"): array{"s"}}, name("N")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})
}

// TestReadDictStream pins the boundary between a stream's dictionary and its
// data: the line ending after the stream keyword, the offset recorded for the
// first data byte, and the modes in which no stream is looked for.
func TestReadDictStream(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		offset    int64
	}{
		{"crlf", "<< /Length 3 >> stream\r\nabc", 24},
		{"lf", "<< /Length 3 >> stream\nabc", 23},
		{"cr alone, tolerated", "<< /Length 3 >> stream\rabc", 23},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := fileLexer(tt.src)
			s, ok := b.readObject().(stream)
			if !ok {
				t.Fatal("want a stream")
			}
			if s.offset != tt.offset {
				t.Errorf("data offset = %d, want %d", s.offset, tt.offset)
			}
			if b.readOffset() != tt.offset || b.readByte() != 'a' {
				t.Error("the lexer is not positioned at the first data byte")
			}
		})
	}
	t.Run("no stream keyword", func(t *testing.T) {
		b := fileLexer("<< /A 1 >> 2")
		if got := b.readObject(); !reflect.DeepEqual(got, dict{name("A"): int64(1)}) {
			t.Errorf("got %#v", got)
		}
		if got := b.readObject(); got != int64(2) {
			t.Errorf("next object = %#v, want the pushed-back 2", got)
		}
	})
	t.Run("streams not looked for in content streams", func(t *testing.T) {
		b := lexer("<< /A 1 >> stream")
		if got := b.readObject(); !reflect.DeepEqual(got, dict{name("A"): int64(1)}) {
			t.Errorf("got %#v", got)
		}
		if got := b.readToken(); got != keyword("stream") {
			t.Errorf("next token = %#v, want the stream keyword left in place", got)
		}
	})
	t.Run("cut off by end of input", func(t *testing.T) {
		got := fileLexer("<< /A 1 /B").readObject()
		if !reflect.DeepEqual(got, dict{name("A"): int64(1), name("B"): nil}) {
			t.Errorf("got %#v", got)
		}
	})
}

// TestSeekForwardBeforeWindow pins that an offset behind the read position
// is reported rather than producing a negative index into the window.
func TestSeekForwardBeforeWindow(t *testing.T) {
	b := lexer("0123456789")
	for range 5 {
		b.readByte()
	}
	b.seekForward(7)
	if c := b.readByte(); c != '7' {
		t.Errorf("after seekForward(7) read %q, want '7'", c)
	}
	mustPanicWith(t, "outside buffer", func() {
		b := lexer(strings.Repeat("x", 5000))
		for range 4100 {
			b.readByte()
		}
		b.seekForward(10)
	})
}

// TestReadObjectDecryptsStrings pins the one place strings are decrypted:
// inside an object definition of an encrypted document, with the key derived
// from that object's number. Outside an object nothing is touched.
func TestReadObjectDecryptsStrings(t *testing.T) {
	key := []byte("0123456789abcdef")
	ptr := objptr{4, 0}
	c, _ := rc4.NewCipher(cryptKey(key, false, ptr))
	ct := make([]byte, len("secret"))
	c.XORKeyStream(ct, []byte("secret"))
	src := fmt.Sprintf("4 0 obj <%x> endobj <%x>", ct, ct)

	b := fileLexer(src)
	b.key = key
	def, ok := b.readObject().(objdef)
	if !ok || def.obj != "secret" {
		t.Errorf("inside the object got %#v, want %q", def.obj, "secret")
	}
	if got := b.readObject(); got != string(ct) {
		t.Errorf("outside an object got %#v, want the raw string", got)
	}
}
