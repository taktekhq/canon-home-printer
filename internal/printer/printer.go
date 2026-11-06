// Package printer talks to CUPS and explains what the printer is doing in plain language.
//
// It reads state over IPP and submits or cancels jobs with the lp and cancel commands, which
// talk to CUPS over its local socket as the service's own user.
package printer

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ipp "github.com/phin1x/go-ipp"
)

// Printer is one CUPS queue plus the USB device behind it.
type Printer struct {
	Queue      string
	MaintQueue string
	USBVendor  string
	USBProduct string
	Computer   string // e.g. "the Toshiba laptop"
	Admin      string // who to ask
	CUPSLog    string
	SysUSB     string // /sys/bus/usb/devices (overridable for tests)

	client *ipp.IPPClient

	mu       sync.Mutex
	progress map[int]progressMark
}

type progressMark struct {
	message string
	since   time.Time
}

// StallAfter is how long a printing job can show the same progress before we call it stalled.
const StallAfter = 90 * time.Second

// New makes a Printer that reads CUPS at host:port.
func New(host string, port int) *Printer {
	return &Printer{
		client:   ipp.NewIPPClient(host, port, "", "", false),
		SysUSB:   "/sys/bus/usb/devices",
		progress: map[int]progressMark{},
	}
}

var jobStates = map[int]string{3: "queued", 4: "held", 5: "printing", 6: "stopped", 7: "canceled", 8: "failed", 9: "done"}
var printerStates = map[int]string{3: "idle", 4: "printing", 5: "stopped"}

var jobAttrs = []string{
	"job-id", "job-name", "job-originating-user-name", "job-state", "job-state-reasons",
	"job-printer-state-message", "job-media-sheets-completed", "job-impressions-completed",
	"time-at-creation", "time-at-processing", "time-at-completed", "job-printer-uri",
}

var printerAttrs = []string{
	"printer-state", "printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs",
	"printer-make-and-model", "printer-state-change-time",
}

func first(a ipp.Attributes, name string) any {
	if v := a[name]; len(v) > 0 {
		return v[0].Value
	}
	return nil
}

func attrInt(a ipp.Attributes, name string) int {
	if n, ok := first(a, name).(int); ok {
		return n
	}
	return 0
}

func attrStr(a ipp.Attributes, name string) string {
	if s, ok := first(a, name).(string); ok {
		return s
	}
	return ""
}

func attrStrs(a ipp.Attributes, name string) []string {
	var out []string
	for _, v := range a[name] {
		if s, ok := v.Value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func attrBool(a ipp.Attributes, name string, def bool) bool {
	if b, ok := first(a, name).(bool); ok {
		return b
	}
	return def
}

func stamp(unix int) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(int64(unix), 0).Format(time.RFC3339)
}

// USBConnected reports whether a device with our vendor and product id is plugged in.
func (p *Printer) USBConnected() bool {
	dirs, _ := filepath.Glob(filepath.Join(p.SysUSB, "*"))
	for _, d := range dirs {
		v, err1 := os.ReadFile(filepath.Join(d, "idVendor"))
		pr, err2 := os.ReadFile(filepath.Join(d, "idProduct"))
		if err1 == nil && err2 == nil &&
			strings.TrimSpace(string(v)) == p.USBVendor && strings.TrimSpace(string(pr)) == p.USBProduct {
			return true
		}
	}
	return false
}

// Status is the printer's state for the page.
type Status struct {
	Queue        string  `json:"queue"`
	USB          bool    `json:"usb"`
	State        string  `json:"state"`
	Message      string  `json:"message"`
	Issues       []Issue `json:"issues"`
	Accepting    bool    `json:"accepting"`
	Model        string  `json:"model"`
	StateChanged string  `json:"state_changed,omitempty"`
	Detail       string  `json:"detail,omitempty"`
}

// Status asks CUPS how the queue is doing.
func (p *Printer) Status() Status {
	s := Status{Queue: p.Queue, USB: p.USBConnected(), Issues: []Issue{}}
	a, err := p.client.GetPrinterAttributes(p.Queue, printerAttrs)
	if err != nil {
		s.State = "missing"
		s.Detail = err.Error()
		s.Issues = append(s.Issues, Issue{Code: "no-queue", Severity: "error",
			Text: fmt.Sprintf("The print queue %s isn't set up on %s.", p.Queue, p.Computer),
			Fix:  fmt.Sprintf("Ask %s: the printer needs to be added to CUPS again.", p.Admin)})
		return s
	}
	s.State = printerStates[attrInt(a, "printer-state")]
	if s.State == "" {
		s.State = "unknown"
	}
	s.Issues = append(s.Issues, Explain(cleanReasons(attrStrs(a, "printer-state-reasons")), p.Computer, p.Admin)...)
	if !s.USB {
		s.Issues = append([]Issue{{Code: "usb", Severity: "error",
			Text: fmt.Sprintf("%s can't see the printer on USB.", capitalize(p.Computer)),
			Fix: fmt.Sprintf("Make sure the printer is switched on and its USB cable is plugged into %s. "+
				"If it is, unplug the cable and plug it back in.", p.Computer)}}, s.Issues...)
	}
	s.Accepting = attrBool(a, "printer-is-accepting-jobs", true)
	if !s.Accepting {
		s.Issues = append(s.Issues, Issue{Code: "rejecting", Severity: "error",
			Text: "The print queue isn't accepting new jobs.", Fix: fmt.Sprintf("Ask %s to re-enable it.", p.Admin)})
	}
	s.Message = attrStr(a, "printer-state-message")
	s.Model = attrStr(a, "printer-make-and-model")
	s.StateChanged = stamp(attrInt(a, "printer-state-change-time"))
	return s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Job is one print job for the page.
type Job struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	User     string  `json:"user"`
	State    string  `json:"state"`
	Summary  string  `json:"summary"`
	Message  string  `json:"message"`
	Issues   []Issue `json:"issues"`
	Timeline []Mark  `json:"timeline"`
	Active   bool    `json:"active"`
	Created  string  `json:"created"`
	Sheets   int     `json:"sheets"`
	Printer  string  `json:"printer"` // "print" or "maintenance"

	By   string `json:"by"`
	Mine bool   `json:"mine"`
}

// Mark is one point on a job's timeline.
type Mark struct {
	At    string `json:"at"`
	Label string `json:"label"`
}

// stalledFor remembers when a printing job's progress message last changed.
func (p *Printer) stalledFor(id int, message string, state int, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state != 5 {
		delete(p.progress, id)
		return 0
	}
	last, ok := p.progress[id]
	if !ok || last.message != message {
		p.progress[id] = progressMark{message, now}
		return 0
	}
	return now.Sub(last.since)
}

func (p *Printer) describe(id int, a ipp.Attributes, printerState string, now time.Time) Job {
	state := attrInt(a, "job-state")
	message := attrStr(a, "job-printer-state-message")
	issues := Explain(cleanReasons(attrStrs(a, "job-state-reasons")), p.Computer, p.Admin)
	stalled := p.stalledFor(id, message, state, now)

	var summary string
	switch state {
	case 3:
		summary = "Waiting in line"
		if printerState == "stopped" {
			summary = "Waiting: the printer is stopped"
		}
	case 4:
		summary = "On hold"
	case 5:
		switch {
		case strings.HasPrefix(strings.ToLower(message), "printing"):
			summary = message
		case message != "":
			summary = "Printing: " + message
		default:
			summary = "Printing"
		}
		if stalled >= StallAfter {
			mins := int(stalled.Minutes())
			if mins < 1 {
				mins = 1
			}
			issues = append([]Issue{{Code: "stalled", Severity: "error",
				Text: fmt.Sprintf("No progress for %d min. The printer has probably stopped and is waiting.", mins),
				Fix: "Most often it's out of paper: load paper, then press the printer's Black or Color button. " +
					"Also check for a jam or a blinking orange light."}}, issues...)
		}
	case 9:
		summary = "Printed"
	case 7:
		summary = "Canceled"
	case 8:
		summary = "Failed"
	default:
		summary = jobStates[state]
		if summary == "" {
			summary = "Unknown"
		}
	}

	ended := map[int]string{9: "Finished", 7: "Canceled", 8: "Stopped with an error"}[state]
	if ended == "" {
		ended = "Ended"
	}
	timeline := []Mark{}
	for _, m := range []struct{ attr, label string }{
		{"time-at-creation", "Sent"}, {"time-at-processing", "Started printing"}, {"time-at-completed", ended},
	} {
		if t := stamp(attrInt(a, m.attr)); t != "" {
			timeline = append(timeline, Mark{t, m.label})
		}
	}
	sheets := attrInt(a, "job-media-sheets-completed")
	if sheets == 0 {
		sheets = attrInt(a, "job-impressions-completed")
	}
	kind := "print"
	if p.MaintQueue != p.Queue && strings.HasSuffix(attrStr(a, "job-printer-uri"), "/"+p.MaintQueue) {
		kind = "maintenance"
	}
	stateName := jobStates[state]
	if stateName == "" {
		stateName = strconv.Itoa(state)
	}
	return Job{
		ID: id, Name: attrStr(a, "job-name"), User: attrStr(a, "job-originating-user-name"),
		State: stateName, Summary: summary, Message: message, Issues: issues, Timeline: timeline,
		Active: state == 3 || state == 4 || state == 5, Created: stamp(attrInt(a, "time-at-creation")),
		Sheets: sheets, Printer: kind,
	}
}

// Jobs lists jobs on every queue, newest first. which is "all" or "not-completed".
func (p *Printer) Jobs(which string, limit int) []Job {
	st := p.Status()
	raw, err := p.client.GetJobs("", "", which, false, 0, 0, jobAttrs)
	if err != nil {
		return []Job{}
	}
	now := time.Now()
	out := make([]Job, 0, len(raw))
	for id, a := range raw {
		out = append(out, p.describe(id, a, st.State, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Job returns one job, or false.
func (p *Printer) Job(id int) (Job, bool) {
	for _, j := range p.Jobs("all", 0) {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// PrintOptions are the choices on the print form.
type PrintOptions struct {
	Copies int
	Color  bool
	Pages  string // e.g. "1-3,5"; empty = all
	Fit    bool
}

var requestID = regexp.MustCompile(`request id is \S+-(\d+)`)

// LPArgs builds the lp command line for a print job.
func (p *Printer) LPArgs(path, title, user string, o PrintOptions) []string {
	copies := min(max(o.Copies, 1), 50)
	color, mode := "RGB", "color"
	if !o.Color {
		color, mode = "Gray", "monochrome"
	}
	args := []string{"-U", user, "-d", p.Queue, "-t", truncate(title, 200), "-n", strconv.Itoa(copies),
		"-o", "ColorModel=" + color, "-o", "print-color-mode=" + mode}
	if o.Pages != "" {
		args = append(args, "-o", "page-ranges="+o.Pages)
	}
	if o.Fit {
		args = append(args, "-o", "print-scaling=fit")
	}
	return append(args, "--", path)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Submit prints a file and returns the CUPS job id. CUPS keeps its own copy of the file.
func (p *Printer) Submit(path, title, user string, o PrintOptions) (int, error) {
	return runLP(p.LPArgs(path, title, user, o), nil)
}

// SubmitRaw sends raw bytes (maintenance commands) to the maintenance queue.
func (p *Printer) SubmitRaw(data []byte, title, user string) (int, error) {
	return runLP([]string{"-U", user, "-d", p.MaintQueue, "-o", "raw", "-t", title}, data)
}

func runLP(args []string, stdin []byte) (int, error) {
	cmd := exec.Command("lp", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, &CUPSError{msg}
	}
	return ParseRequestID(out.String())
}

// ParseRequestID reads the job id from lp's "request id is Queue-123 (1 file(s))".
func ParseRequestID(out string) (int, error) {
	m := requestID.FindStringSubmatch(out)
	if m == nil {
		return 0, &CUPSError{"lp didn't say which job it created: " + strings.TrimSpace(out)}
	}
	return strconv.Atoi(m[1])
}

// Cancel cancels a job.
func (p *Printer) Cancel(id int) error {
	out, err := exec.Command("cancel", strconv.Itoa(id)).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return &CUPSError{msg}
	}
	return nil
}

// CUPSError is CUPS refusing something.
type CUPSError struct{ Msg string }

func (e *CUPSError) Error() string { return e.Msg }

// IsCUPSError reports whether err came from CUPS.
func IsCUPSError(err error) bool {
	var c *CUPSError
	return errors.As(err, &c)
}

// LogLine is one line of the CUPS error log.
type LogLine struct {
	At    string `json:"at"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

var logLine = regexp.MustCompile(`^([EWI]) \[([^\]]+)\] (.*)$`)
var logNoise = []string{"Bad driver information file", "CreateProfile failed", "Unable to configure printer options",
	"cups-driverd", "Expiring subscriptions", "cups-brf"}

// RecentLog returns errors and warnings from the CUPS log, newest first, without known noise.
func (p *Printer) RecentLog(since time.Duration, limit int) []LogLine {
	f, err := os.Open(p.CUPSLog)
	if err != nil {
		return []LogLine{{Level: "warning", Text: "Can't read the CUPS log: " + errText(err)}}
	}
	defer f.Close()
	return ParseLog(f, time.Now().Add(-since), limit)
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// ParseLog reads a CUPS error_log.
func ParseLog(r interface{ Read([]byte) (int, error) }, cutoff time.Time, limit int) []LogLine {
	levels := map[string]string{"E": "error", "W": "warning", "I": "info"}
	var lines []LogLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		m := logLine.FindStringSubmatch(line)
		if m == nil || containsAny(line, logNoise) {
			continue
		}
		when, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[2])
		if err != nil || when.Before(cutoff) {
			continue
		}
		lines = append(lines, LogLine{At: when.Format(time.RFC3339), Level: levels[m[1]], Text: m[3]})
	}
	out := make([]LogLine, 0, len(lines))
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, lines[i])
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
