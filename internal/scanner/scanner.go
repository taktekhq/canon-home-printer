// Package scanner runs scans with scanimage, one at a time, in the background.
package scanner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Formats maps scan formats to content types.
var Formats = map[string]string{"pdf": "application/pdf", "jpeg": "image/jpeg", "png": "image/png"}

var resolutions = map[int]bool{150: true, 300: true, 600: true}
var modes = map[string]bool{"Color": true, "Gray": true}

// ErrBadSettings means an unsupported resolution, mode or format.
var ErrBadSettings = errors.New("unsupported scan settings")

// ErrBusy means another scan is running.
var ErrBusy = errors.New("Another scan is in progress. Wait for it to finish.")

// Scanner runs scanimage.
type Scanner struct {
	Device   string // SANE device; empty = scanimage's default
	Dir      string // one folder per user inside
	Computer string
	Admin    string
	Command  string        // scanimage, overridable for tests
	Timeout  time.Duration // per scan

	busy sync.Mutex
	mu   sync.Mutex
	jobs map[string]*Job
}

// Job is one scan.
type Job struct {
	ID         string `json:"id"`
	user       string
	Resolution int     `json:"resolution"`
	Mode       string  `json:"mode"`
	Format     string  `json:"format"`
	File       string  `json:"file"`
	State      string  `json:"state"` // queued, scanning, done, failed
	Created    float64 `json:"created"`
	Started    float64 `json:"started,omitempty"`
	Finished   float64 `json:"finished,omitempty"`
	Seconds    float64 `json:"seconds,omitempty"`
	Size       int64   `json:"size,omitempty"`
	Error      string  `json:"error,omitempty"`
	Fix        string  `json:"fix,omitempty"`
	Detail     string  `json:"detail,omitempty"`
	ExitCode   int     `json:"exit_code,omitempty"`
	URL        *string `json:"url"`
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// UserDir is the folder for one user's scans. The name is a hash so emails don't appear on disk.
func (s *Scanner) UserDir(email string) string {
	sum := sha256.Sum256([]byte(email))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])[:16])
}

// Busy reports whether a scan is running.
func (s *Scanner) Busy() bool {
	if s.busy.TryLock() {
		s.busy.Unlock()
		return false
	}
	return true
}

// Start begins a scan in the background.
func (s *Scanner) Start(user string, resolution int, mode, format string) (Job, error) {
	if !resolutions[resolution] || !modes[mode] || Formats[format] == "" {
		return Job{}, ErrBadSettings
	}
	if !s.busy.TryLock() {
		return Job{}, ErrBusy
	}
	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}
	id := make([]byte, 6)
	rand.Read(id)
	now := time.Now()
	j := &Job{ID: hex.EncodeToString(id), user: user, Resolution: resolution, Mode: mode, Format: format,
		File: "scan_" + now.Format("2006-01-02_15-04-05") + "." + ext, State: "queued", Created: unix(now)}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*Job{}
	}
	s.jobs[j.ID] = j
	pub := *j
	s.mu.Unlock()
	go func() {
		defer s.busy.Unlock()
		s.run(j)
	}()
	return pub, nil
}

func (s *Scanner) run(j *Job) {
	dir := s.UserDir(j.user)
	os.MkdirAll(dir, 0o700)
	out := filepath.Join(dir, j.File)
	args := []string{"--mode", j.Mode, "--resolution", strconv.Itoa(j.Resolution), "--format=" + j.Format, "-o", out}
	if s.Device != "" {
		args = append([]string{"-d", s.Device}, args...)
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	command := s.Command
	if command == "" {
		command = "scanimage"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.update(j, func(j *Job) { j.State, j.Started = "scanning", unix(time.Now()) })
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	errText := strings.TrimSpace(stderr.String())
	if ctx.Err() == context.DeadlineExceeded {
		code, errText = -9, fmt.Sprintf("timed out after %s", timeout)
	} else if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if errText == "" {
			errText = err.Error()
		}
	}
	st, statErr := os.Stat(out)
	s.update(j, func(j *Job) {
		j.Finished = unix(time.Now())
		j.Seconds = float64(int((j.Finished-j.Started)*10+0.5)) / 10
		if code == 0 && statErr == nil && st.Size() > 0 {
			j.State, j.Size = "done", st.Size()
			return
		}
		j.State = "failed"
		j.Error, j.Fix = s.Explain(errText, code)
		if len(errText) > 800 {
			errText = errText[len(errText)-800:]
		}
		j.Detail, j.ExitCode = errText, code
		os.Remove(out)
	})
}

func (s *Scanner) update(j *Job, f func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(j)
}

// Explain turns scanimage's error output into plain language.
func (s *Scanner) Explain(stderr string, code int) (string, string) {
	e := strings.ToLower(stderr)
	switch {
	case strings.Contains(e, "busy"):
		return "The scanner is busy.", "Wait for the printer to finish what it's doing, then try again."
	case strings.Contains(e, "invalid argument"), strings.Contains(e, "no such device"),
		strings.Contains(e, "failed to open"), strings.Contains(e, "not found"):
		return fmt.Sprintf("%s can't reach the scanner.", capitalize(s.Computer)),
			fmt.Sprintf("Check that the printer is on and its USB cable is plugged into %s, then try again.", s.Computer)
	case strings.Contains(e, "cover"):
		return "The scanner lid seems open or blocked.", "Close the lid and try again."
	case code == -9 || strings.Contains(e, "timed out"):
		return "The scan took too long and was stopped.", "Try again, or pick a lower resolution."
	}
	return "The scan failed.", fmt.Sprintf("Try again. If it keeps failing, the details below help %s fix it.", s.Admin)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Get returns a scan if it belongs to user.
func (s *Scanner) Get(id, user string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.user != user {
		return Job{}, false
	}
	pub := *j
	if pub.State == "done" {
		u := "/api/scans/file/" + pub.File
		pub.URL = &u
	}
	return pub, true
}

// File is one saved scan.
type File struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Created string `json:"created"`
	URL     string `json:"url"`
}

// List returns a user's scans, newest first.
func (s *Scanner) List(user string) []File {
	entries, _ := os.ReadDir(s.UserDir(user))
	type f struct {
		File
		mod time.Time
	}
	var fs []f
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		fs = append(fs, f{File{e.Name(), info.Size(), info.ModTime().Format(time.RFC3339), "/api/scans/file/" + e.Name()}, info.ModTime()})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.After(fs[j].mod) })
	out := make([]File, 0, len(fs))
	for _, x := range fs {
		out = append(out, x.File)
	}
	return out
}

// Path returns the path of one of a user's scans, or "" if there is no such file.
func (s *Scanner) Path(user, name string) string {
	if name == "" || strings.HasPrefix(name, ".") || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return ""
	}
	p := filepath.Join(s.UserDir(user), name)
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return p
}

// Cleanup deletes scans older than keep.
func (s *Scanner) Cleanup(keep time.Duration) {
	cutoff := time.Now().Add(-keep)
	filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(p)
		}
		return nil
	})
}
