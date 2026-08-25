// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Reading of PDF tokens and objects from a raw byte stream.

package pdf

import (
	"errors"
	"fmt"
	"io"
	"strconv"
)

// A token is a PDF token in the input stream, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	keyword, a PDF keyword
//	name, a PDF name without the leading slash
//
// Go has no sum types, so token is an interface and the dynamic type is the
// tag: readers branch with a type switch, and keyword and name are distinct
// string types so that "obj", /obj and (obj) never compare equal. At the end
// of input, when allowEOF is set, readToken returns the value io.EOF.
type token any

// A name is a PDF name, without the leading slash.
type name string

// A keyword is a PDF keyword.
// Delimiter tokens used in higher-level syntax,
// such as "<<", ">>", "[", "]", "{", "}", are also treated as keywords.
type keyword string

// A buffer holds buffered input bytes from the PDF file and lexes them into
// tokens and objects. Everything the package reads from a file goes through
// one: an object at an offset named by the cross-reference table, the
// decoded data of a stream, or the concatenated content streams of a page.
type buffer struct {
	r      io.Reader // source of data
	buf    []byte    // window of data read from r, 4 KB at most
	pos    int       // read index in buf
	offset int64     // position in r just past buf, i.e. of the next read
	tmp    []byte    // scratch space reused while accumulating a token
	unread []token   // tokens read and then pushed back; the last is read first

	// Parsing modes. newBuffer sets the file-object mode: end of input is
	// malformed, "1 0 R" is a reference and a dictionary may be followed
	// by stream data. Interpret switches all three for content streams,
	// where the input simply ends, "1 0 R" is three operands and
	// dictionaries are inline parameters.
	allowEOF    bool // the end of input is expected rather than malformed
	allowObjptr bool // parse "N G R" as an object reference
	allowStream bool // a dictionary may be followed by the stream keyword

	eof   bool // set by reload once the input ended; readByte then returns '\n'
	depth int  // current nesting of arrays and dictionaries, see maxObjectNesting

	// Decryption context, set only when reading an object from the file
	// of an encrypted document. Strings and streams are encrypted with a
	// key derived from the number of the object that holds them, so the
	// number of the object being parsed is tracked while it is read.
	key    []byte // document key; nil when the file is not encrypted
	useAES bool   // AES rather than RC4
	objptr objptr // the object being parsed; zero outside an object definition
}

// newBuffer returns a new buffer reading from r at the given offset. The
// buffer starts in file-object mode; see the parsing mode fields.
func newBuffer(r io.Reader, offset int64) *buffer {
	return &buffer{
		r:           r,
		offset:      offset,
		buf:         make([]byte, 0, 4096),
		allowObjptr: true,
		allowStream: true,
	}
}

// readByte returns the next byte of input. At the end of input it returns
// '\n' rather than an error: a trailing space ends any token harmlessly, so
// the lexer need not check for the end on every byte. Loops that skip
// whitespace must check b.eof instead, or they never end.
func (b *buffer) readByte() byte {
	if b.pos >= len(b.buf) {
		b.reload()
		if b.pos >= len(b.buf) {
			return '\n'
		}
	}
	c := b.buf[b.pos]
	b.pos++
	return c
}

// errorf reports malformed input. The lexer has no error return: it panics
// with an error value, and the methods of Reader and Page that return an
// error recover it (see recoverMalformed). The statements that follow an
// errorf call in the lexer keep the code well-formed should the panic ever
// be replaced; they are not reached.
func (b *buffer) errorf(format string, args ...any) {
	panic(fmt.Errorf(format, args...))
}

// reload refills buf from r and reports whether any data arrived. It is the
// one place that decides whether the end of input is normal — allowEOF is
// set, or the value has no stream at all — or a malformed file, which it
// reports through errorf. A read that ends without either decision leaves
// readByte returning '\n' forever.
//
// Each read stops at the next multiple of the buffer capacity in the file,
// so that the window stays aligned to the file and seekForward can compute
// a position inside it.
func (b *buffer) reload() bool {
	n := cap(b.buf) - int(b.offset%int64(cap(b.buf)))
	n, err := b.r.Read(b.buf[:n])
	if n == 0 && err != nil {
		b.buf = b.buf[:0]
		b.pos = 0
		if errors.Is(err, errStreamNotPresent) {
			b.eof = true
			return false
		}
		if b.allowEOF && err == io.EOF {
			b.eof = true
			return false
		}
		b.errorf("malformed PDF: reading at offset %d: %v", b.offset, err)
		return false
	}
	b.offset += int64(n)
	b.buf = b.buf[:n]
	b.pos = 0
	return true
}

// seekForward moves the read position to offset by reading and discarding
// the input up to it; the source is a plain io.Reader and cannot be
// repositioned. Its one use is an object stream: the index table at the
// start names where each object begins, and the object wanted is reached by
// skipping ahead to that position. Because reload keeps the window aligned
// to the source, the position lands inside the current window and is a
// simple index into it. An offset before the window, or past the end of the
// input, is malformed; the latter leaves the buffer at its end, which
// readObject reports as io.EOF.
func (b *buffer) seekForward(offset int64) {
	if offset < 0 {
		b.errorf("malformed PDF: seek to negative offset %d", offset)
		return
	}
	for b.offset < offset {
		if !b.reload() {
			return
		}
	}
	pos := len(b.buf) - int(b.offset-offset)
	if pos < 0 || pos > len(b.buf) {
		b.errorf("malformed PDF: seek to offset %d outside buffer", offset)
		return
	}
	b.pos = pos
}

// readOffset returns the position in the source of the next byte readByte
// would return. offset names the end of the window, so the position is that
// minus the part of the window not yet consumed. Its one use is recording
// where the data of a stream begins, right after the stream keyword.
func (b *buffer) readOffset() int64 {
	return b.offset - int64(len(b.buf)) + int64(b.pos)
}

// unreadByte steps the read position back one byte, so that a byte read to
// see whether a token has ended is read again as the start of the next one.
// It can only step back within the current window, which is enough: the
// lexer never looks more than one byte ahead. At the end of input there is
// nothing to step back over, and the synthetic '\n' is simply produced again.
func (b *buffer) unreadByte() {
	if b.pos > 0 {
		b.pos--
	}
}

// unreadToken pushes a token back so that the next readToken returns it.
// The parser needs it to look ahead: "1 0 R" and "1 0 obj" start with two
// integers, and only the third token says what they are. Tokens are pushed
// back in reverse order of reading, since the last pushed is read first.
func (b *buffer) unreadToken(t token) {
	b.unread = append(b.unread, t)
}

// readToken returns the next token of input: a pushed-back one if there is
// any, otherwise the next one lexed from the bytes. Whitespace and comments
// between tokens are skipped. The first byte of a token decides what it is:
// '<' opens a hex string or, doubled, a dictionary; '(' a literal string;
// '/' a name; brackets and braces are keywords on their own; anything else
// runs to the next delimiter or space and is a keyword, a number or a
// boolean. At the end of input, with allowEOF set, the value io.EOF is
// returned; without it reload has already reported the file as malformed.
func (b *buffer) readToken() token {
	if n := len(b.unread); n > 0 {
		t := b.unread[n-1]
		b.unread = b.unread[:n-1]
		return t
	}

	// Find first non-space, non-comment byte.
	c := b.readByte()
	for {
		if isSpace(c) {
			if b.eof {
				return io.EOF
			}
			c = b.readByte()
		} else if c == '%' {
			for c != '\r' && c != '\n' {
				c = b.readByte()
			}
		} else {
			break
		}
	}

	switch c {
	case '<':
		if b.readByte() == '<' {
			return keyword("<<")
		}
		b.unreadByte()
		return b.readHexString()

	case '(':
		return b.readLiteralString()

	case '[', ']', '{', '}':
		return keyword(string(c))

	case '/':
		return b.readName()

	case '>':
		if b.readByte() == '>' {
			return keyword(">>")
		}
		b.unreadByte()
		fallthrough

	default:
		if isDelim(c) {
			b.errorf("unexpected delimiter %#q", rune(c))
			return nil
		}
		b.unreadByte()
		return b.readKeyword()
	}
}

// readHexString reads the body of a hex string after its '<': pairs of hex
// digits up to the closing '>', with white space between digits ignored. An
// odd number of digits is valid: the final digit behaves as if followed by 0
// (PDF 32000-1, 7.3.4.3). The end of input ends the string as well; a string
// left open is malformed, but reload has already decided whether that is
// reportable, and the lexer must not spin waiting for a '>' that never comes.
func (b *buffer) readHexString() token {
	tmp := b.tmp[:0]
	for {
		c, ok := b.readHexStringByte()
		if !ok {
			break
		}
		c2, ok := b.readHexStringByte()
		if !ok {
			x := unhex(c) << 4
			if x < 0 {
				b.errorf("malformed hex string %c", c)
				break
			}
			tmp = append(tmp, byte(x))
			break
		}
		x := unhex(c)<<4 | unhex(c2)
		if x < 0 {
			b.errorf("malformed hex string %c %c %s", c, c2, b.buf[b.pos:])
			break
		}
		tmp = append(tmp, byte(x))
	}
	b.tmp = tmp
	return string(tmp)
}

// readHexStringByte returns the next byte of a hex string that is not white
// space, reporting false at the closing '>' or at the end of input.
func (b *buffer) readHexStringByte() (byte, bool) {
	for {
		c := b.readByte()
		if c == '>' || b.eof {
			return 0, false
		}
		if !isSpace(c) {
			return c, true
		}
	}
}

// unhex returns the value of the hex digit b, or -1 if b is not one. The
// result is an int so that a -1 survives being shifted and or-ed into a byte
// value: one sign check on the combined result catches a bad digit in either
// position.
func unhex(b byte) int {
	switch {
	case '0' <= b && b <= '9':
		return int(b) - '0'
	case 'a' <= b && b <= 'f':
		return int(b) - 'a' + 10
	case 'A' <= b && b <= 'F':
		return int(b) - 'A' + 10
	}
	return -1
}

// readLiteralString reads the body of a literal string after its '(': bytes
// up to the matching ')', with unescaped parentheses allowed as long as they
// balance, and backslash escapes decoded (PDF 32000-1, 7.3.4.2). The end of
// input ends the string; reload has already decided whether that is
// reportable.
func (b *buffer) readLiteralString() token {
	tmp := b.tmp[:0]
	depth := 1
	for !b.eof {
		c := b.readByte()
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case '\\':
			tmp = b.appendEscape(tmp)
			continue
		}
		if depth == 0 {
			break
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	return string(tmp)
}

// appendEscape decodes the escape sequence after a backslash in a literal
// string and appends its value to tmp: the C-style letters, an escaped
// parenthesis or backslash, up to three octal digits, or a line ending, which
// stands for nothing and joins the string across lines. A backslash before
// any other character is ignored, as the specification says, and the
// character itself is kept.
func (b *buffer) appendEscape(tmp []byte) []byte {
	c := b.readByte()
	switch c {
	case 'n':
		return append(tmp, '\n')
	case 'r':
		return append(tmp, '\r')
	case 'b':
		return append(tmp, '\b')
	case 't':
		return append(tmp, '\t')
	case 'f':
		return append(tmp, '\f')
	case '\r':
		if b.readByte() != '\n' {
			b.unreadByte()
		}
		return tmp
	case '\n':
		return tmp
	case '0', '1', '2', '3', '4', '5', '6', '7':
		x := int(c - '0')
		for range 2 {
			c = b.readByte()
			if c < '0' || c > '7' {
				b.unreadByte()
				break
			}
			x = x*8 + int(c-'0')
		}
		if x > 255 {
			b.errorf("invalid octal escape \\%03o", x)
		}
		return append(tmp, byte(x))
	}
	return append(tmp, c)
}

// readName reads a name after its '/': every byte up to the next space or
// delimiter, with #xx standing for the byte with that hex code so that a name
// can hold a space or a delimiter (PDF 32000-1, 7.3.5). The result carries no
// slash. A '#' not followed by two hex digits is malformed.
func (b *buffer) readName() token {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		if c == '#' {
			x := unhex(b.readByte())<<4 | unhex(b.readByte())
			if x < 0 {
				b.errorf("malformed name")
			}
			tmp = append(tmp, byte(x))
			continue
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	return name(tmp)
}

// readKeyword reads a run of bytes up to the next space or delimiter and
// classifies it: true and false are booleans, digits with an optional sign
// are an integer, digits with one '.' are a real, and anything else is a
// keyword — obj, R, stream, a content operator such as Tj, or a word the
// file made up, which the caller decides what to do with. PDF numbers have no
// exponent, so 1e5 is a keyword, not a number.
func (b *buffer) readKeyword() token {
	tmp := b.tmp[:0]
	for {
		c := b.readByte()
		if isDelim(c) || isSpace(c) {
			b.unreadByte()
			break
		}
		tmp = append(tmp, c)
	}
	b.tmp = tmp
	s := string(tmp)
	switch {
	case s == "true":
		return true
	case s == "false":
		return false
	case isInteger(s):
		x, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			b.errorf("invalid integer %s", s)
		}
		return x
	case isReal(s):
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			b.errorf("invalid real %s", s)
		}
		return x
	}
	return keyword(string(tmp))
}

// isInteger reports whether s is an optional sign followed by one or more
// decimal digits.
func isInteger(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || '9' < c {
			return false
		}
	}
	return true
}

// isReal reports whether s is an optional sign followed by decimal digits
// with exactly one '.' among them, anywhere: 1., .5 and 1.5 are all reals.
func isReal(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) == 0 {
		return false
	}
	ndot := 0
	for _, c := range s {
		if c == '.' {
			ndot++
			continue
		}
		if c < '0' || '9' < c {
			return false
		}
	}
	return ndot == 1
}

// An object is a PDF syntax object, one of the following Go types:
//
//	bool, a PDF boolean
//	int64, a PDF integer
//	float64, a PDF real
//	string, a PDF string literal
//	name, a PDF name without the leading slash
//	dict, a PDF dictionary
//	array, a PDF array
//	stream, a PDF stream
//	objptr, a PDF object reference
//	objdef, a PDF object definition
//
// An object may also be nil, to represent the PDF null.
type object any

// A dict is a PDF dictionary: names mapped to objects. The values are stored
// as read, so a value can be an objptr that has yet to be resolved.
type dict map[name]object

// An array is a PDF array. As in a dict, its elements may be unresolved
// references.
type array []object

// A stream is a PDF stream: a dictionary followed by raw data. The lexer
// reads the dictionary and records where the data begins, but does not read
// the data; Value.Reader opens the file at offset, and decodes and decrypts
// on demand, when the data is wanted.
type stream struct {
	hdr    dict   // the stream dictionary: /Length, /Filter and the rest
	ptr    objptr // the object holding the stream; its decryption key depends on it
	offset int64  // position of the first data byte in the file
}

// An objptr is a reference to an indirect object, written "N G R" in a
// file: the object number and its generation. Object numbers are bounded by
// maxObjectNumber, so uint32 always holds one; generations are 16-bit in the
// cross-reference table itself.
type objptr struct {
	id  uint32
	gen uint16
}

// An objdef is the definition of an indirect object, written
// "N G obj ... endobj" in a file. It is what reading at a cross-reference
// offset yields; resolve checks that ptr is the object it asked for and
// unwraps obj.
type objdef struct {
	ptr objptr
	obj object
}

// readObject reads one object: a value token as it is, or a dictionary or an
// array built from the tokens that follow, or null. With allowObjptr set it
// also recognises the two forms that start with a pair of integers — "N G R",
// a reference, and "N G obj ... endobj", the definition of object N — by
// reading ahead up to two tokens and pushing them back if neither matches.
// Strings are decrypted here when the document is encrypted, since the key
// depends on the object being read. A closing ">>" or "]" met where a value
// was expected ends the enclosing dictionary or array by reading as null.
func (b *buffer) readObject() object {
	tok := b.readToken()
	if kw, ok := tok.(keyword); ok {
		switch kw {
		case "null":
			return nil
		case "<<":
			return b.readDict()
		case "[":
			return b.readArray()
		case ">>", "]":
			// stop the object - these mark the end of dict/array
			return nil
		}
		b.errorf("unexpected keyword %q parsing object", kw)
		return nil
	}

	if str, ok := tok.(string); ok && b.key != nil && b.objptr.id != 0 {
		tok = decryptString(b.key, b.useAES, b.objptr, str)
	}

	if !b.allowObjptr {
		return tok
	}

	if t1, ok := tok.(int64); ok && int64(uint32(t1)) == t1 {
		tok2 := b.readToken()
		if t2, ok := tok2.(int64); ok && int64(uint16(t2)) == t2 {
			tok3 := b.readToken()
			switch tok3 {
			case keyword("R"):
				return objptr{uint32(t1), uint16(t2)}
			case keyword("obj"):
				old := b.objptr
				b.objptr = objptr{uint32(t1), uint16(t2)}
				obj := b.readObject()
				if _, ok := obj.(stream); !ok {
					tok4 := b.readToken()
					if tok4 != keyword("endobj") {
						b.errorf("missing endobj after indirect object definition")
						b.unreadToken(tok4)
					}
				}
				b.objptr = old
				return objdef{objptr{uint32(t1), uint16(t2)}, obj}
			}
			b.unreadToken(tok3)
		}
		b.unreadToken(tok2)
	}
	return tok
}

// maxObjectNesting bounds arrays and dictionaries nested inside one another.
// readObject recurses once per level, and a run of openers with no closers
// would otherwise exhaust the goroutine stack, which is fatal rather than
// recoverable.
const maxObjectNesting = 512

// enter records entering an array or a dictionary, and reports the input as
// malformed once they nest deeper than maxObjectNesting. Real files nest a
// handful of levels; only a run of openers with no closers gets anywhere near
// the limit, and without it that run would end in a fatal stack overflow.
func (b *buffer) enter() {
	if b.depth++; b.depth > maxObjectNesting {
		b.errorf("malformed PDF: object nesting deeper than %d", maxObjectNesting)
	}
}

// leave records leaving an array or a dictionary; it is deferred by the
// readers so that the count stays right when a panic unwinds through them.
func (b *buffer) leave() {
	b.depth--
}

// readArray reads the elements of an array after its '[' up to the closing
// ']', each through readObject. The end of input also ends the array: a
// truncated file must not keep the lexer appending forever.
func (b *buffer) readArray() object {
	b.enter()
	defer b.leave()
	var x array
	for {
		tok := b.readToken()
		// Break on io.EOF as well (readToken returns io.EOF as a token value
		// once the input is exhausted, and readDict already guards for it):
		// otherwise an array that is never closed, e.g. in a truncated
		// content stream, loops forever appending io.EOF objects and
		// allocates memory without bound.
		if tok == nil || tok == io.EOF || tok == keyword("]") {
			break
		}
		b.unreadToken(tok)
		x = append(x, b.readObject())
	}
	return x
}

// readDict reads the entries of a dictionary after its "<<" up to the closing
// ">>": a name key followed by any object, each through readObject. A key
// that is not a name is malformed. With allowStream set — that is, when
// reading objects from the file rather than a content stream — the token
// after the dictionary is examined too: if it is the stream keyword, the
// dictionary was the header of a stream, whose data starts on the next line,
// and a stream value recording that position is returned instead. The data
// itself is not read here.
func (b *buffer) readDict() object {
	b.enter()
	defer b.leave()
	x := make(dict)
	for {
		tok := b.readToken()
		if tok == nil || tok == keyword(">>") {
			break
		}
		if tok == io.EOF {
			break
		}
		n, ok := tok.(name)
		if !ok {
			b.errorf("unexpected non-name key %T(%v) parsing dictionary", tok, tok)
			continue
		}
		x[n] = b.readObject()
	}

	if !b.allowStream {
		return x
	}

	tok := b.readToken()
	if tok != keyword("stream") {
		b.unreadToken(tok)
		return x
	}

	switch b.readByte() {
	case '\r':
		if b.readByte() != '\n' {
			b.unreadByte()
		}
	case '\n':
		// ok
	default:
		b.errorf("stream keyword not followed by newline")
	}

	return stream{x, b.objptr, b.readOffset()}
}

// isSpace reports whether b is one of the six white-space characters of PDF
// syntax (PDF 32000-1, 7.2.2). NUL is one of them, which is why a stray zero
// byte between tokens is not an error.
func isSpace(b byte) bool {
	switch b {
	case '\x00', '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

// isDelim reports whether b is one of the delimiter characters of PDF syntax
// (PDF 32000-1, 7.2.2). A delimiter ends the token before it without being
// part of it, so a name or keyword runs up to the first space or delimiter.
// Together the two sets are the only way tokens are separated: PDF has no
// other punctuation.
func isDelim(b byte) bool {
	switch b {
	case '<', '>', '(', ')', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}
