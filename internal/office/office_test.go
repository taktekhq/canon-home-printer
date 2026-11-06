package office

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtAndLooks(t *testing.T) {
	if Ext("Report.DOCX") != "docx" || Ext("a.pdf") != "" || Ext("noext") != "" {
		t.Fatal("Ext")
	}
	zip := []byte("PK\x03\x04rest")
	ole := []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1rest")
	cases := []struct {
		head []byte
		ext  string
		want bool
	}{
		{zip, "docx", true}, {zip, "doc", false}, {ole, "doc", true}, {ole, "docx", false},
		{[]byte(`{\rtf1`), "rtf", true}, {[]byte("%PDF"), "docx", false},
	}
	for _, c := range cases {
		if Looks(c.head, c.ext) != c.want {
			t.Errorf("%q %s", c.head[:4], c.ext)
		}
	}
}

// fakeSoffice writes "<outdir>/document.pdf" like LibreOffice does.
func fakeSoffice(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "soffice")
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

func TestToPDF(t *testing.T) {
	src := filepath.Join(t.TempDir(), "upload")
	os.WriteFile(src, []byte("PK\x03\x04"), 0o600)
	c := &Converter{Command: fakeSoffice(t, `while [ $# -gt 1 ]; do [ "$1" = --outdir ] && out=$2; shift; done; echo "%PDF-1.7" > "$out/document.pdf"`)}
	pdf, cleanup, err := c.ToPDF(src, "docx")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(pdf)
	if string(b) != "%PDF-1.7\n" {
		t.Fatalf("%q", b)
	}
	cleanup()
	if _, err := os.Stat(pdf); !os.IsNotExist(err) {
		t.Fatal("cleanup left the PDF")
	}
}

func TestToPDFFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "upload")
	os.WriteFile(src, []byte("x"), 0o600)
	c := &Converter{Command: fakeSoffice(t, `echo "Error: source file could not be loaded"; exit 1`)}
	if _, _, err := c.ToPDF(src, "docx"); err == nil {
		t.Fatal("want error")
	}
	c = &Converter{Command: "/nonexistent/soffice"}
	if _, _, err := c.ToPDF(src, "docx"); err != ErrUnavailable {
		t.Fatalf("got %v", err)
	}
}
