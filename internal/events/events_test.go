package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddRecent(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "e.jsonl")}
	for i := 1; i <= 5; i++ {
		l.Add("a@b.c", "print", map[string]any{"job": i})
	}
	r := l.Recent(3)
	if len(r) != 3 || r[0].Int("job") != 5 || r[2].Int("job") != 3 {
		t.Fatalf("%v", r)
	}
	if time.Since(r[0].Time()) > time.Minute {
		t.Fatal("bad time")
	}
}

func TestOldTimeFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	os.WriteFile(p, []byte(`{"at": "2026-11-06T21:00:00", "user": "x", "action": "scan"}`+"\nnot json\n"), 0o600)
	r := (&Log{Path: p}).Recent(10)
	if len(r) != 1 || r[0].Time().Hour() != 21 {
		t.Fatalf("%v", r)
	}
}
