package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fake(t *testing.T, script string) *Scanner {
	dir := t.TempDir()
	bin := filepath.Join(dir, "scanimage")
	os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	return &Scanner{Dir: filepath.Join(dir, "scans"), Computer: "the laptop", Admin: "Sam", Command: bin}
}

func wait(t *testing.T, s *Scanner, id, user string) Job {
	for i := 0; i < 100; i++ {
		j, _ := s.Get(id, user)
		if j.State == "done" || j.State == "failed" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("scan never finished")
	return Job{}
}

func TestScanSuccessAndPrivacy(t *testing.T) {
	// The last argument is the output file.
	s := fake(t, `for a; do out=$a; done; echo data > "$out"`)
	j, err := s.Start("mom@example.com", 300, "Color", "pdf")
	if err != nil {
		t.Fatal(err)
	}
	done := wait(t, s, j.ID, "mom@example.com")
	if done.State != "done" || done.URL == nil || done.Size == 0 {
		t.Fatalf("%+v", done)
	}
	if _, ok := s.Get(j.ID, "dad@example.com"); ok {
		t.Fatal("another user can see the scan")
	}
	if len(s.List("mom@example.com")) != 1 || len(s.List("dad@example.com")) != 0 {
		t.Fatal("list not per user")
	}
	if s.Path("mom@example.com", done.File) == "" || s.Path("dad@example.com", done.File) != "" {
		t.Fatal("path not per user")
	}
	for _, bad := range []string{"../x", ".hidden", "a/b", ""} {
		if s.Path("mom@example.com", bad) != "" {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestScanFailureExplained(t *testing.T) {
	s := fake(t, `echo "scanimage: open of device pixma failed: Invalid argument" >&2; exit 1`)
	j, _ := s.Start("mom@example.com", 150, "Gray", "jpeg")
	done := wait(t, s, j.ID, "mom@example.com")
	if done.State != "failed" || !strings.Contains(done.Error, "The laptop can't reach") || done.ExitCode != 1 {
		t.Fatalf("%+v", done)
	}
	if len(s.List("mom@example.com")) != 0 {
		t.Fatal("failed scan left a file")
	}
}

func TestBadSettingsAndBusy(t *testing.T) {
	s := fake(t, `sleep 0.3; for a; do out=$a; done; echo x > "$out"`)
	if _, err := s.Start("a", 1200, "Color", "pdf"); err != ErrBadSettings {
		t.Fatal(err)
	}
	j, err := s.Start("a", 150, "Color", "png")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Busy() {
		t.Fatal("want busy")
	}
	if _, err := s.Start("a", 150, "Color", "png"); err != ErrBusy {
		t.Fatal("want ErrBusy")
	}
	wait(t, s, j.ID, "a")
}
