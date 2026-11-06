package printer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ipp "github.com/phin1x/go-ipp"
)

func attrs(kv map[string]any) ipp.Attributes {
	a := ipp.Attributes{}
	for k, v := range kv {
		switch x := v.(type) {
		case []string:
			for _, s := range x {
				a[k] = append(a[k], ipp.Attribute{Name: k, Value: s})
			}
		default:
			a[k] = []ipp.Attribute{{Name: k, Value: v}}
		}
	}
	return a
}

func newTest() *Printer {
	p := New("localhost", 631)
	p.Queue, p.MaintQueue, p.Computer, p.Admin = "Canon_G3010", "Canon_G3010", "the laptop", "Sam"
	return p
}

func TestStalledJob(t *testing.T) {
	p := newTest()
	a := attrs(map[string]any{"job-state": 5, "job-printer-state-message": "Printing page 1, 11%", "job-state-reasons": []string{"job-printing"}})
	t0 := time.Now()
	if j := p.describe(7, a, "printing", t0); len(j.Issues) != 0 {
		t.Fatalf("fresh job has issues: %+v", j.Issues)
	}
	j := p.describe(7, a, "printing", t0.Add(2*time.Minute))
	if len(j.Issues) == 0 || j.Issues[0].Code != "stalled" || !strings.Contains(j.Issues[0].Text, "2 min") {
		t.Fatalf("want stalled, got %+v", j.Issues)
	}
	if j.Summary != "Printing page 1, 11%" || !j.Active {
		t.Errorf("summary %q active %v", j.Summary, j.Active)
	}
}

func TestReasons(t *testing.T) {
	got := cleanReasons([]string{"none", "media-empty-error", "offline-report", "job-printing", "weird-thing-warning"})
	if want := []string{"media-empty", "offline", "weird-thing"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	is := Explain(got, "the laptop", "Sam")
	if is[0].Severity != "error" || !strings.Contains(is[1].Fix, "plugged into the laptop") || is[2].Severity != "warning" {
		t.Fatalf("%+v", is)
	}
	if p := Explain([]string{"paused"}, "x", "Sam"); !strings.Contains(p[0].Fix, "Ask Sam") {
		t.Fatal(p[0].Fix)
	}
}

func TestDoneJobTimeline(t *testing.T) {
	p := newTest()
	a := attrs(map[string]any{"job-state": 9, "time-at-creation": 1000, "time-at-processing": 1005, "time-at-completed": 1060, "job-media-sheets-completed": 2})
	j := p.describe(3, a, "idle", time.Now())
	if j.Summary != "Printed" || j.Active || len(j.Timeline) != 3 || j.Timeline[2].Label != "Finished" || j.Sheets != 2 {
		t.Fatalf("%+v", j)
	}
}

func TestLPArgs(t *testing.T) {
	p := newTest()
	got := strings.Join(p.LPArgs("/tmp/f", "doc.pdf", "mom@example.com", PrintOptions{Copies: 99, Color: false, Pages: "1-2", Fit: true}), " ")
	want := "-U mom@example.com -d Canon_G3010 -t doc.pdf -n 50 -o ColorModel=Gray -o print-color-mode=monochrome -o page-ranges=1-2 -o print-scaling=fit -- /tmp/f"
	if got != want {
		t.Fatalf("\ngot  %s\nwant %s", got, want)
	}
}

func TestParseRequestID(t *testing.T) {
	id, err := ParseRequestID("request id is Canon_G3010-123 (1 file(s))\n")
	if err != nil || id != 123 {
		t.Fatal(id, err)
	}
	if _, err := ParseRequestID("nope"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseLog(t *testing.T) {
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(d).Format("02/Jan/2006:15:04:05 -0700") }
	log := "E [" + ts(-48*time.Hour) + "] old\n" +
		"W [" + ts(-time.Hour) + "] cups-driverd noise\n" +
		"E [" + ts(-time.Hour) + "] first\n" +
		"D [" + ts(-time.Minute) + "] debug\n" +
		"W [" + ts(-time.Minute) + "] second\n"
	got := ParseLog(strings.NewReader(log), now.Add(-24*time.Hour), 10)
	if len(got) != 2 || got[0].Text != "second" || got[1].Level != "error" {
		t.Fatalf("%+v", got)
	}
}

func TestUSB(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "1-2")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "idVendor"), []byte("04a9\n"), 0o644)
	os.WriteFile(filepath.Join(d, "idProduct"), []byte("183b\n"), 0o644)
	p := newTest()
	p.SysUSB, p.USBVendor, p.USBProduct = dir, "04a9", "183b"
	if !p.USBConnected() {
		t.Fatal("want connected")
	}
	p.USBProduct = "ffff"
	if p.USBConnected() {
		t.Fatal("want not connected")
	}
}
