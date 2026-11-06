// Package server is the website: the JSON API and the page.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	canonij "github.com/nizarmah/canon-ij-maint"

	"github.com/taktekhq/canon-home-printer/internal/config"
	"github.com/taktekhq/canon-home-printer/internal/events"
	"github.com/taktekhq/canon-home-printer/internal/printer"
	"github.com/taktekhq/canon-home-printer/internal/scanner"
)

// Printer is what the server needs from CUPS.
type Printer interface {
	Status() printer.Status
	Jobs(which string, limit int) []printer.Job
	Job(id int) (printer.Job, bool)
	Submit(path, title, user string, o printer.PrintOptions) (int, error)
	SubmitRaw(data []byte, title, user string) (int, error)
	Cancel(id int) error
	RecentLog(since time.Duration, limit int) []printer.LogLine
}

// Verifier turns a request's sign-in token into an email.
type Verifier interface {
	Email(token string) (string, error)
}

// Server holds everything the handlers use.
type Server struct {
	Cfg      *config.Config
	Printer  Printer
	Scanner  *scanner.Scanner
	Events   *events.Log
	Verifier Verifier // nil when sign-in isn't configured
	Static   fs.FS
	Token    func(*http.Request) string
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &httpError{code, fmt.Sprintf(format, a...)}
}

type handler func(w http.ResponseWriter, r *http.Request, user string) (any, error)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// user returns the signed-in email, or an error to send back.
func (s *Server) user(r *http.Request) (string, error) {
	if s.Verifier == nil {
		if s.Cfg.DevUser != "" && s.Cfg.TeamDomain == "" {
			return s.Cfg.DevUser, nil
		}
		return "", fail(500, "Sign-in isn't configured on the server (CF_ACCESS_TEAM_DOMAIN / CF_ACCESS_AUD).")
	}
	tok := s.Token(r)
	if tok == "" {
		return "", fail(401, "Not signed in. Open the site through its normal address to sign in.")
	}
	email, err := s.Verifier.Email(tok)
	if err != nil {
		return "", fail(401, "Sign-in token isn't valid (%s). Reload the page to sign in again.", err)
	}
	return email, nil
}

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		user, err := s.user(r)
		var v any
		if err == nil {
			v, err = h(w, r, user)
		}
		if err != nil {
			var he *httpError
			switch {
			case errors.As(err, &he):
				writeJSON(w, he.code, map[string]string{"detail": he.msg})
			case printer.IsCUPSError(err):
				writeJSON(w, 502, map[string]string{"detail": "The print system (CUPS) refused: " + err.Error()})
			default:
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
				writeJSON(w, 500, map[string]string{"detail": "Something went wrong on the server."})
			}
			return
		}
		if v != nil {
			writeJSON(w, 200, v)
		}
	}
}

// Handler returns the whole site.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/me", s.wrap(s.me))
	mux.HandleFunc("GET /api/status", s.wrap(s.status))
	mux.HandleFunc("GET /api/jobs", s.wrap(s.listJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.wrap(s.job))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.wrap(s.cancel))
	mux.HandleFunc("POST /api/print", s.wrap(s.print))
	mux.HandleFunc("POST /api/scan", s.wrap(s.scan))
	mux.HandleFunc("GET /api/scan/{id}", s.wrap(s.scanStatus))
	mux.HandleFunc("GET /api/scans", s.wrap(s.scans))
	mux.HandleFunc("GET /api/scans/file/{name}", s.wrap(s.scanFile))
	mux.HandleFunc("DELETE /api/scans/file/{name}", s.wrap(s.deleteScan))
	mux.HandleFunc("POST /api/maintenance", s.wrap(s.maintenance))
	mux.HandleFunc("GET /api/log", s.wrap(s.log))
	notFound := s.wrap(func(http.ResponseWriter, *http.Request, string) (any, error) {
		return nil, fail(404, "Not found.")
	})
	static := http.FileServerFS(s.Static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			notFound(w, r)
			return
		}
		// The page itself only shows data the API returns, but keep it behind sign-in too.
		if _, err := s.user(r); err != nil {
			var he *httpError
			errors.As(err, &he)
			http.Error(w, he.msg, he.code)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	return secure(mux)
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.ServeHTTP(w, r)
	})
}

// DisplayName is the friendly name for an email: names.json first, else the part before @.
func (s *Server) DisplayName(email string) string {
	if email == "" {
		return ""
	}
	if b, err := os.ReadFile(s.Cfg.NamesFile); err == nil {
		var names map[string]string
		if json.Unmarshal(b, &names) == nil {
			for k, v := range names {
				if strings.EqualFold(k, email) && v != "" {
					return v
				}
			}
		}
	}
	local, _, _ := strings.Cut(email, "@")
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(local, ".", " "), "_", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// CanMaintain reports whether a user may run cleanings and cancel other people's jobs.
func (s *Server) CanMaintain(email string) bool {
	if len(s.Cfg.MaintUsers) == 0 {
		return true
	}
	for _, m := range s.Cfg.MaintUsers {
		if m == email {
			return true
		}
	}
	return false
}

func (s *Server) me(_ http.ResponseWriter, _ *http.Request, user string) (any, error) {
	return map[string]any{"email": user, "name": s.DisplayName(user), "can_maintain": s.CanMaintain(user),
		"printer": s.Cfg.PrinterName, "max_upload_mb": s.Cfg.MaxUpload >> 20}, nil
}

type check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

func freeGB(dir string) (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return float64(st.Bavail) * float64(st.Bsize) / 1e9, true
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) status(_ http.ResponseWriter, _ *http.Request, user string) (any, error) {
	p := s.Printer.Status()
	active := []printer.Job{}
	for _, j := range s.enrich(s.Printer.Jobs("not-completed", 0), user) {
		if j.Active {
			active = append(active, j)
		}
	}
	comp := capitalize(s.Cfg.ComputerName)
	usbText := comp + " sees the printer on USB."
	if !p.USB {
		usbText = comp + " can't see the printer on USB."
	}
	queueText := map[string]string{"idle": "Ready.", "printing": "Printing now.", "stopped": "Stopped.", "missing": "Missing."}[p.State]
	if queueText == "" {
		queueText = p.State
	}
	scanText := "Ready."
	if s.Scanner.Busy() {
		scanText = "Scanning now."
	}
	gb, ok := freeGB(s.Cfg.DataDir)
	diskText := "Unknown."
	if ok {
		diskText = fmt.Sprintf("%.0f GB free.", gb)
	}
	checks := []check{
		{"Printer plugged in", p.USB, usbText},
		{"Print queue", p.State != "missing" && p.State != "stopped" && p.Accepting, queueText},
		{"Scanner", true, scanText},
		{"Disk space on " + s.Cfg.ComputerName, !ok || gb > 2, diskText},
	}
	issues := append([]printer.Issue{}, p.Issues...)
	for _, j := range active {
		for _, i := range j.Issues {
			i.Job = j.ID
			issues = append(issues, i)
		}
	}
	return map[string]any{"printer": p, "active_jobs": active, "checks": checks, "issues": issues,
		"scanner_busy": s.Scanner.Busy(), "checked_at": time.Now().Format(time.RFC3339)}, nil
}

var maintTitles = map[string]string{"nozzle_check": "Nozzle check", "clean": "Cleaning", "deep_clean": "Deep cleaning"}

type owner struct{ user, name string }

// enrich fills in who sent each job and what it was, from our own log.
func (s *Server) enrich(jobs []printer.Job, user string) []printer.Job {
	owners := map[int]owner{}
	maint := map[int]bool{}
	for _, e := range s.Events.Recent(2000) {
		id := e.Int("job")
		act := e.Str("action")
		if id == 0 || (act != "print" && act != "maintenance") {
			continue
		}
		if _, seen := owners[id]; seen {
			continue
		}
		name := e.Str("file")
		if act == "maintenance" {
			maint[id] = true
			if name = maintTitles[e.Str("what")]; name == "" {
				name = "Maintenance"
			}
		}
		owners[id] = owner{e.Str("user"), name}
	}
	for i := range jobs {
		j := &jobs[i]
		if o, ok := owners[j.ID]; ok {
			j.User, j.Name = o.user, o.name
		}
		if maint[j.ID] {
			j.Printer = "maintenance"
		}
		if j.Name == "" {
			j.Name = "Sent from another computer"
		}
		j.By = "Someone else"
		if j.User != "" {
			j.By = s.DisplayName(j.User)
		}
		j.Mine = j.User == user
	}
	return jobs
}

func (s *Server) listJobs(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	jobs := s.enrich(s.Printer.Jobs("all", 60), user)
	if r.URL.Query().Get("mine") == "true" {
		mine := []printer.Job{}
		for _, j := range jobs {
			if j.Mine {
				mine = append(mine, j)
			}
		}
		jobs = mine
	}
	if len(jobs) > 40 {
		jobs = jobs[:40]
	}
	return jobs, nil
}

func (s *Server) findJob(r *http.Request, user string) (printer.Job, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return printer.Job{}, fail(404, "No such print job.")
	}
	j, ok := s.Printer.Job(id)
	if !ok {
		return printer.Job{}, fail(404, "No such print job.")
	}
	return s.enrich([]printer.Job{j}, user)[0], nil
}

func (s *Server) job(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	return s.findJob(r, user)
}

func (s *Server) cancel(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	j, err := s.findJob(r, user)
	if err != nil {
		return nil, err
	}
	if j.User != user && !s.CanMaintain(user) {
		return nil, fail(403, "You can only cancel your own jobs.")
	}
	if !j.Active {
		return nil, fail(409, "That job has already finished.")
	}
	if err := s.Printer.Cancel(j.ID); err != nil {
		return nil, err
	}
	s.Events.Add(user, "cancel", map[string]any{"job": j.ID})
	return map[string]bool{"ok": true}, nil
}

var pageRange = regexp.MustCompile(`^\s*\d+(\s*-\s*\d+)?(\s*,\s*\d+(\s*-\s*\d+)?)*\s*$`)

// Sniff says what kind of file this is from its first bytes, or "" if we can't print it.
func Sniff(head []byte, filename string) string {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "pdf"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "jpeg"
	case bytes.HasPrefix(head, []byte("\x89PNG")):
		return "png"
	case strings.HasSuffix(strings.ToLower(filename), ".txt") && validUTF8Prefix(head):
		return "text"
	}
	return ""
}

// validUTF8Prefix allows a multi-byte character cut off at the end of the sample.
func validUTF8Prefix(b []byte) bool {
	for i := 0; i < 4 && len(b) > 0; i++ {
		if utf8.Valid(b) {
			return true
		}
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

func formBool(v string, def bool) bool {
	switch strings.ToLower(v) {
	case "true", "1", "on", "yes":
		return true
	case "false", "0", "off", "no":
		return false
	}
	return def
}

func (s *Server) print(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	limit := s.Cfg.MaxUpload
	r.Body = http.MaxBytesReader(nil, r.Body, limit+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fail(400, "Expected a file upload.")
	}
	fields := map[string]string{"copies": "1", "color": "true", "pages": "", "fit": "true"}
	var dest, filename, kind string
	var size int64
	defer func() {
		if dest != "" {
			os.Remove(dest)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, fail(413, "That file is bigger than %d MB.", limit>>20)
			}
			return nil, fail(400, "The upload was cut off. Try again.")
		}
		name := part.FormName()
		if name != "file" {
			if _, ok := fields[name]; ok {
				b, _ := io.ReadAll(io.LimitReader(part, 200))
				fields[name] = string(b)
			}
			part.Close()
			continue
		}
		if dest != "" {
			part.Close()
			continue
		}
		filename = filepath.Base(part.FileName())
		f, err := os.CreateTemp(s.Cfg.UploadDir, "upload-")
		if err != nil {
			return nil, err
		}
		dest = f.Name()
		size, err = io.Copy(f, io.LimitReader(part, limit+1))
		f.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, fail(413, "That file is bigger than %d MB.", limit>>20)
			}
			return nil, fail(400, "The upload was cut off. Try again.")
		}
		if size > limit {
			return nil, fail(413, "That file is bigger than %d MB.", limit>>20)
		}
	}
	if dest == "" {
		return nil, fail(400, "Choose a file to print first.")
	}
	pages := fields["pages"]
	if pages != "" && !pageRange.MatchString(pages) {
		return nil, fail(400, `Pages should look like "1-3" or "1,4,6-8".`)
	}
	f, _ := os.Open(dest)
	head := make([]byte, 2048)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if kind = Sniff(head[:n], filename); kind == "" {
		return nil, fail(415, "This kind of file can't be printed directly. Use PDF, JPG, PNG or plain text. "+
			"For Word or other documents, save or export them as PDF first.")
	}
	copies, _ := strconv.Atoi(strings.TrimSpace(fields["copies"]))
	if copies < 1 {
		copies = 1
	}
	color := formBool(fields["color"], true)
	title := filename
	if title == "" || title == "." {
		title = "document"
	}
	id, err := s.Printer.Submit(dest, title, user, printer.PrintOptions{
		Copies: copies, Color: color, Pages: strings.ReplaceAll(pages, " ", ""), Fit: formBool(fields["fit"], true)})
	if err != nil {
		return nil, err
	}
	s.Events.Add(user, "print", map[string]any{"job": id, "file": title, "kind": kind, "size": size,
		"copies": copies, "color": color, "pages": pages})
	return map[string]int{"job": id}, nil
}

func (s *Server) scan(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	req := struct {
		Resolution int    `json:"resolution"`
		Mode       string `json:"mode"`
		Format     string `json:"format"`
	}{300, "Color", "pdf"}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		return nil, fail(400, "Unsupported scan settings.")
	}
	// The G3010 can't scan while it's printing or cleaning; the scan would just fail.
	for _, j := range s.Printer.Jobs("not-completed", 0) {
		if j.Active && j.State == "printing" {
			return nil, fail(409, "The printer is busy printing right now. Scan once it's finished.")
		}
	}
	job, err := s.Scanner.Start(user, req.Resolution, req.Mode, req.Format)
	switch {
	case errors.Is(err, scanner.ErrBadSettings):
		return nil, fail(400, "Unsupported scan settings.")
	case err != nil:
		return nil, fail(409, "%s", err.Error())
	}
	s.Events.Add(user, "scan", map[string]any{"resolution": req.Resolution, "mode": req.Mode, "format": req.Format, "scan": job.ID})
	return job, nil
}

func (s *Server) scanStatus(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	j, ok := s.Scanner.Get(r.PathValue("id"), user)
	if !ok {
		return nil, fail(404, "No such scan.")
	}
	return j, nil
}

func (s *Server) scans(_ http.ResponseWriter, _ *http.Request, user string) (any, error) {
	return s.Scanner.List(user), nil
}

func (s *Server) scanFile(w http.ResponseWriter, r *http.Request, user string) (any, error) {
	name := r.PathValue("name")
	p := s.Scanner.Path(user, name)
	if p == "" {
		return nil, fail(404, "No such scan.")
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeFile(w, r, p)
	return nil, nil
}

func (s *Server) deleteScan(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	name := r.PathValue("name")
	p := s.Scanner.Path(user, name)
	if p == "" {
		return nil, fail(404, "No such scan.")
	}
	if err := os.Remove(p); err != nil {
		return nil, err
	}
	s.Events.Add(user, "delete-scan", map[string]any{"file": name})
	return map[string]bool{"ok": true}, nil
}

// DeepCleansPerDay is how many deep cleanings may run in 24 hours. More only waste ink.
const DeepCleansPerDay = 2

func (s *Server) maintenance(_ http.ResponseWriter, r *http.Request, user string) (any, error) {
	if !s.CanMaintain(user) {
		return nil, fail(403, "Only %s can run printer maintenance.", s.Cfg.AdminName)
	}
	var req struct {
		Action string `json:"action"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	var data []byte
	switch req.Action {
	case "nozzle_check":
		data = canonij.NozzleCheck()
	case "clean":
		data, _ = canonij.Cleaning(canonij.Regular, canonij.All)
	case "deep_clean":
		n := 0
		for _, e := range s.Events.Recent(500) {
			if e.Str("action") == "maintenance" && e.Str("what") == "deep_clean" && time.Since(e.Time()) < 24*time.Hour {
				n++
			}
		}
		if n >= DeepCleansPerDay {
			return nil, fail(429, "Two deep cleanings already ran in the last 24 hours. "+
				"More won't help and wastes ink: let it rest, or the print head may need replacing.")
		}
		data, _ = canonij.Cleaning(canonij.Deep, canonij.All)
	default:
		return nil, fail(400, "Unknown maintenance action.")
	}
	title := maintTitles[req.Action]
	id, err := s.Printer.SubmitRaw(data, title, user)
	if err != nil {
		return nil, err
	}
	s.Events.Add(user, "maintenance", map[string]any{"what": req.Action, "job": id})
	return map[string]any{"job": id, "title": title}, nil
}

func (s *Server) log(_ http.ResponseWriter, _ *http.Request, _ string) (any, error) {
	evs := s.Events.Recent(60)
	for _, e := range evs {
		e["by"] = s.DisplayName(e.Str("user"))
	}
	return map[string]any{"cups": s.Printer.RecentLog(24*time.Hour, 40), "events": evs}, nil
}

// Cleanup deletes old uploads and scans, every hour, until stop is closed.
func (s *Server) Cleanup(stop <-chan struct{}) {
	for {
		cutoff := time.Now().Add(-time.Duration(s.Cfg.UploadDays) * 24 * time.Hour)
		if entries, err := os.ReadDir(s.Cfg.UploadDir); err == nil {
			for _, e := range entries {
				if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
					os.Remove(filepath.Join(s.Cfg.UploadDir, e.Name()))
				}
			}
		}
		s.Scanner.Cleanup(time.Duration(s.Cfg.ScanDays) * 24 * time.Hour)
		select {
		case <-stop:
			return
		case <-time.After(time.Hour):
		}
	}
}
