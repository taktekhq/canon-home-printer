// Package events keeps the activity log: who printed, scanned or cleaned what, and when.
//
// CUPS hides job names and owners from other users, so this log is also how the page shows
// who sent each job.
package events

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one line of the log. Fields beyond these are kept as they are.
type Event map[string]any

// Log appends JSON lines to a file.
type Log struct {
	Path string
	mu   sync.Mutex
}

// Add records an action.
func (l *Log) Add(user, action string, details map[string]any) error {
	e := Event{"at": time.Now().Format(time.RFC3339), "user": user, "action": action}
	for k, v := range details {
		e[k] = v
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Recent returns up to limit events, newest first.
func (l *Log) Recent(limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.Path)
	if err != nil {
		return []Event{}
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
		if len(lines) > 2*limit+100 {
			lines = lines[len(lines)-limit:]
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	out := make([]Event, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var e Event
		if json.Unmarshal(lines[i], &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Str returns a string field.
func (e Event) Str(k string) string {
	s, _ := e[k].(string)
	return s
}

// Int returns a number field.
func (e Event) Int(k string) int {
	f, _ := e[k].(float64)
	return int(f)
}

// Time parses the "at" field. It accepts the older format without a time zone, as local time.
func (e Event) Time() time.Time {
	at := e.Str("at")
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t
	}
	t, _ := time.ParseInLocation("2006-01-02T15:04:05", at, time.Local)
	return t
}
