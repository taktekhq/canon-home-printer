// Package office turns Word, Excel, PowerPoint and OpenDocument files into PDF with LibreOffice.
package office

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Extensions lists the file types LibreOffice converts for us.
var Extensions = map[string]bool{
	"doc": true, "docx": true, "odt": true, "rtf": true,
	"xls": true, "xlsx": true, "ods": true,
	"ppt": true, "pptx": true, "odp": true,
}

// Ext returns the lowercased extension of a file name if it is an office document we convert, else "".
func Ext(filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if Extensions[ext] {
		return ext
	}
	return ""
}

// Looks reports whether the first bytes fit an office file with this extension:
// a ZIP for the XML formats, an OLE container for the old binary ones, "{\rtf" for RTF.
func Looks(head []byte, ext string) bool {
	switch ext {
	case "docx", "xlsx", "pptx", "odt", "ods", "odp":
		return bytes.HasPrefix(head, []byte("PK\x03\x04"))
	case "doc", "xls", "ppt":
		return bytes.HasPrefix(head, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"))
	case "rtf":
		return bytes.HasPrefix(head, []byte(`{\rtf`))
	}
	return false
}

// Converter runs LibreOffice, one document at a time.
type Converter struct {
	Command string        // soffice; empty = "soffice"
	Timeout time.Duration // per document; 0 = 2 minutes
	TempDir string        // where to work; empty = os.TempDir()

	mu sync.Mutex
}

// ErrUnavailable means LibreOffice isn't installed.
var ErrUnavailable = errors.New("LibreOffice isn't installed")

// Available reports whether the converter command exists.
func (c *Converter) Available() bool {
	_, err := exec.LookPath(c.command())
	return err == nil
}

func (c *Converter) command() string {
	if c.Command != "" {
		return c.Command
	}
	return "soffice"
}

// ToPDF converts src (whose real type is ext) to PDF. The caller must call cleanup when done with pdf.
func (c *Converter) ToPDF(src, ext string) (pdf string, cleanup func(), err error) {
	if !c.Available() {
		return "", func() {}, ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dir, err := os.MkdirTemp(c.TempDir, "office-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	in := filepath.Join(dir, "document."+ext)
	if err := copyFile(src, in); err != nil {
		cleanup()
		return "", func() {}, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// A fresh profile per run: LibreOffice refuses to start twice on one profile, and a
	// crashed run can leave a lock behind.
	cmd := exec.CommandContext(ctx, c.command(),
		"-env:UserInstallation=file://"+filepath.Join(dir, "profile"),
		"--headless", "--norestore", "--nolockcheck",
		"--convert-to", "pdf", "--outdir", dir, in)
	cmd.Env = append(os.Environ(), "HOME="+dir)
	out, runErr := cmd.CombinedOutput()
	pdf = filepath.Join(dir, "document.pdf")
	if st, err := os.Stat(pdf); err != nil || st.Size() == 0 {
		cleanup()
		if ctx.Err() == context.DeadlineExceeded {
			return "", func() {}, fmt.Errorf("conversion took longer than %s", timeout)
		}
		msg := strings.TrimSpace(string(out))
		if runErr != nil && msg == "" {
			msg = runErr.Error()
		}
		return "", func() {}, fmt.Errorf("conversion failed: %s", msg)
	}
	return pdf, cleanup, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
