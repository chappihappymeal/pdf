package pdf

import (
	"bytes"
	"fmt"
	"testing"
)

// TestMissingContentStreamDoesNotPanic verifies that a page whose /Contents
// points at an object with no stream data degrades to an empty page instead
// of panicking out of Page.Content and text extraction. Value.Reader responds
// to reads on such objects with errStreamNotPresent, which the lexer treats
// as end of input.
func TestMissingContentStreamDoesNotPanic(t *testing.T) {
	pdfData := missingContentsPDF()
	reader, err := NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on a page without a content stream: %v", r)
		}
	}()

	content := reader.Page(1).Content()
	if len(content.Text) != 0 {
		t.Fatalf("expected no text, got %v", content.Text)
	}

	text, err := reader.Page(1).GetPlainText(nil)
	if err != nil {
		t.Fatalf("GetPlainText: %v", err)
	}
	if text != "" {
		t.Fatalf("expected empty text, got %q", text)
	}
}

func missingContentsPDF() []byte {
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	writeObject := func(number int, body string) {
		offsets[number] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> /Contents 4 0 R >>")
	writeObject(4, "<< /Length 0 >>")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for number := 1; number <= 4; number++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return pdf.Bytes()
}
