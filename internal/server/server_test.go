package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/taktekhq/canon-home-printer/internal/config"
	"github.com/taktekhq/canon-home-printer/internal/events"
	"github.com/taktekhq/canon-home-printer/internal/printer"
	"github.com/taktekhq/canon-home-printer/internal/scanner"
)

type fakePrinter struct {
	jobs      []printer.Job
	submitted []printer.PrintOptions
	raw       [][]byte
	canceled  []int
	nextID    int
}

func (f *fakePrinter) Status() printer.Status {
	return printer.Status{State: "idle", USB: true, Accepting: true}
}
func (f *fakePrinter) Jobs(string, int) []printer.Job {
	return append([]printer.Job(nil), f.jobs...)
}
func (f *fakePrinter) Job(id int) (printer.Job, bool) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return printer.Job{}, false
}
func (f *fakePrinter) Submit(path, title, user string, o printer.PrintOptions) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	f.submitted = append(f.submitted, o)
	f.nextID++
	f.jobs = append(f.jobs, printer.Job{ID: f.nextID, State: "queued", Active: true})
	return f.nextID, nil
}
func (f *fakePrinter) SubmitRaw(data []byte, title, user string) (int, error) {
	f.raw = append(f.raw, data)
	f.nextID++
	return f.nextID, nil
}
func (f *fakePrinter) Cancel(id int) error                            { f.canceled = append(f.canceled, id); return nil }
func (f *fakePrinter) RecentLog(time.Duration, int) []printer.LogLine { return nil }

type fakeVerifier struct{}

func (fakeVerifier) Email(tok string) (string, error) {
	if strings.HasPrefix(tok, "ok:") {
		return tok[3:], nil
	}
	return "", errors.New("bad signature")
}

func setup(t *testing.T) (*Server, *fakePrinter) {
	dir := t.TempDir()
	cfg := &config.Config{PrinterName: "Canon G3010", ComputerName: "the laptop", AdminName: "Sam",
		DataDir: dir, UploadDir: filepath.Join(dir, "up"), ScanDir: filepath.Join(dir, "scans"),
		EventLog: filepath.Join(dir, "e.jsonl"), NamesFile: filepath.Join(dir, "names.json"),
		MaxUpload: 1 << 20, MaintUsers: []string{"admin@x.com"}}
	os.MkdirAll(cfg.UploadDir, 0o700)
	os.WriteFile(cfg.NamesFile, []byte(`{"Mom@X.com": "Mom"}`), 0o600)
	fp := &fakePrinter{}
	s := &Server{Cfg: cfg, Printer: fp, Scanner: &scanner.Scanner{Dir: cfg.ScanDir}, Events: &events.Log{Path: cfg.EventLog},
		Verifier: fakeVerifier{}, Static: fstest.MapFS{"index.html": {Data: []byte("<h1>hi</h1>")}},
		Token: func(r *http.Request) string { return r.Header.Get("Cf-Access-Jwt-Assertion") }}
	return s, fp
}

func do(s *Server, method, path, user string, body *bytes.Buffer, ct string) *httptest.ResponseRecorder {
	if body == nil {
		body = &bytes.Buffer{}
	}
	r := httptest.NewRequest(method, path, body)
	if user != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", "ok:"+user)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func upload(t *testing.T, name string, content []byte, fields map[string]string) (*bytes.Buffer, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(content)
	mw.Close()
	return &b, mw.FormDataContentType()
}

func TestSignInRequired(t *testing.T) {
	s, _ := setup(t)
	for _, p := range []string{"/", "/api/me", "/api/status"} {
		if w := do(s, "GET", p, "", nil, ""); w.Code != 401 {
			t.Errorf("%s without sign-in: %d", p, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", "forged")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("forged token: %d", w.Code)
	}
	if w := do(s, "GET", "/healthz", "", nil, ""); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestNotConfiguredRefusesAll(t *testing.T) {
	s, _ := setup(t)
	s.Verifier = nil
	if w := do(s, "GET", "/api/me", "mom@x.com", nil, ""); w.Code != 500 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestMe(t *testing.T) {
	s, _ := setup(t)
	w := do(s, "GET", "/api/me", "mom@x.com", nil, "")
	var me map[string]any
	json.Unmarshal(w.Body.Bytes(), &me)
	if me["name"] != "Mom" || me["can_maintain"] != false || me["printer"] != "Canon G3010" {
		t.Fatalf("%v", me)
	}
	if s.DisplayName("layla.m01@x.com") != "Layla M01" {
		t.Fatal(s.DisplayName("layla.m01@x.com"))
	}
}

func TestPrintFlow(t *testing.T) {
	s, fp := setup(t)
	b, ct := upload(t, "a.pdf", []byte("%PDF-1.4 hello"), map[string]string{"copies": "2", "color": "false", "pages": "1-2"})
	w := do(s, "POST", "/api/print", "mom@x.com", b, ct)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if o := fp.submitted[0]; o.Copies != 2 || o.Color || o.Pages != "1-2" || !o.Fit {
		t.Fatalf("%+v", o)
	}
	entries, _ := os.ReadDir(s.Cfg.UploadDir)
	if len(entries) != 0 {
		t.Fatal("upload not deleted")
	}
	// The job shows as Mom's, with its file name, to everyone.
	w = do(s, "GET", "/api/jobs", "dad@x.com", nil, "")
	var jobs []printer.Job
	json.Unmarshal(w.Body.Bytes(), &jobs)
	if len(jobs) != 1 || jobs[0].By != "Mom" || jobs[0].Name != "a.pdf" || jobs[0].Mine {
		t.Fatalf("%+v", jobs)
	}
	// Dad can't cancel Mom's job; Mom can.
	if w := do(s, "POST", "/api/jobs/1/cancel", "dad@x.com", nil, ""); w.Code != 403 {
		t.Fatalf("dad cancel: %d", w.Code)
	}
	if w := do(s, "POST", "/api/jobs/1/cancel", "mom@x.com", nil, ""); w.Code != 200 || len(fp.canceled) != 1 {
		t.Fatalf("mom cancel: %d", w.Code)
	}
}

func TestPrintRejects(t *testing.T) {
	s, _ := setup(t)
	cases := []struct {
		name    string
		content []byte
		fields  map[string]string
		code    int
	}{
		{"a.docx", []byte("PK\x03\x04"), nil, 415},
		{"a.pdf", []byte("%PDF"), map[string]string{"pages": "1-2; rm -rf"}, 400},
		{"big.pdf", append([]byte("%PDF"), make([]byte, 2<<20)...), nil, 413},
	}
	for _, c := range cases {
		b, ct := upload(t, c.name, c.content, c.fields)
		if w := do(s, "POST", "/api/print", "mom@x.com", b, ct); w.Code != c.code {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.code, w.Body)
		}
	}
}

func TestSniff(t *testing.T) {
	for in, want := range map[string]string{"%PDF-1.7": "pdf", "\xff\xd8\xff\xe0": "jpeg", "\x89PNG\r\n": "png"} {
		if got := Sniff([]byte(in), "x"); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if Sniff([]byte("hello"), "notes.txt") != "text" || Sniff([]byte("hello"), "notes.doc") != "" {
		t.Error("text")
	}
	if Sniff([]byte("caf\xc3"), "a.txt") != "text" {
		t.Error("cut-off UTF-8 should still be text")
	}
}

func TestMaintenance(t *testing.T) {
	s, fp := setup(t)
	body := func(a string) *bytes.Buffer { return bytes.NewBufferString(`{"action":"` + a + `"}`) }
	if w := do(s, "POST", "/api/maintenance", "mom@x.com", body("clean"), "application/json"); w.Code != 403 {
		t.Fatalf("non-admin: %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		if w := do(s, "POST", "/api/maintenance", "admin@x.com", body("deep_clean"), "application/json"); w.Code != 200 {
			t.Fatalf("deep clean %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := do(s, "POST", "/api/maintenance", "admin@x.com", body("deep_clean"), "application/json"); w.Code != 429 {
		t.Fatalf("third deep clean: %d", w.Code)
	}
	if !bytes.Contains(fp.raw[0], []byte("<ivec:type>deep</ivec:type>")) {
		t.Fatal("not a deep clean")
	}
	if w := do(s, "POST", "/api/maintenance", "admin@x.com", body("flush"), "application/json"); w.Code != 400 {
		t.Fatal("unknown action accepted")
	}
}

func TestScanFileIsPerUser(t *testing.T) {
	s, _ := setup(t)
	d := s.Scanner.UserDir("mom@x.com")
	os.MkdirAll(d, 0o700)
	os.WriteFile(filepath.Join(d, "scan_1.pdf"), []byte("%PDF"), 0o600)
	if w := do(s, "GET", "/api/scans/file/scan_1.pdf", "mom@x.com", nil, ""); w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("mom: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := do(s, "GET", "/api/scans/file/scan_1.pdf", "dad@x.com", nil, ""); w.Code != 404 {
		t.Fatalf("dad: %d", w.Code)
	}
	if w := do(s, "GET", "/api/scans/file/..%2Fx", "mom@x.com", nil, ""); w.Code != 404 {
		t.Fatalf("traversal: %d", w.Code)
	}
}
