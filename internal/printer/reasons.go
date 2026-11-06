package printer

import (
	"fmt"
	"regexp"
	"strings"
)

// Issue is one problem, in plain language, with what to do about it.
type Issue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // error, warning or info
	Text     string `json:"text"`
	Fix      string `json:"fix"`
	Job      int    `json:"job,omitempty"`
}

type reason struct{ severity, text, fix string }

// reasons maps IPP printer-state-reasons and job-state-reasons to explanations.
// %[1]s is the computer's name, %[2]s is who to ask.
var reasons = map[string]reason{
	"media-empty":                {"error", "The printer is out of paper.", "Load paper in the rear tray, then press the printer's Black or Color button."},
	"media-needed":               {"error", "The printer needs paper.", "Load paper in the rear tray, then press the printer's Black or Color button."},
	"media-jam":                  {"error", "Paper is jammed.", "Gently pull the stuck sheet out, then press the Black or Color button."},
	"cover-open":                 {"error", "A cover on the printer is open.", "Close it."},
	"door-open":                  {"error", "A door on the printer is open.", "Close it."},
	"marker-supply-low":          {"warning", "One of the inks is low.", "Check the ink tanks on the front."},
	"marker-supply-empty":        {"error", "One of the inks is empty.", "Refill the empty tank."},
	"offline":                    {"error", "The printer isn't responding.", "Check that the printer is switched on and its USB cable is plugged into %[1]s."},
	"connecting-to-device":       {"warning", "Trying to reach the printer.", "If this lasts more than a minute, check the printer is on and plugged in."},
	"paused":                     {"error", "Printing is paused on the computer.", "Ask %[2]s to resume the printer queue."},
	"job-canceled-by-user":       {"info", "Someone canceled this job.", ""},
	"job-canceled-by-operator":   {"info", "This job was canceled.", ""},
	"document-format-error":      {"error", "The printer couldn't read this file.", "Try saving it as a PDF and printing that."},
	"document-unprintable-error": {"error", "This file can't be printed.", "Try saving it as a PDF and printing that."},
	"aborted-by-system":          {"error", "The computer stopped this job because of an error.", "Check the Status tab for details, then try again."},
	"job-hold-until-specified":   {"info", "This job is on hold.", ""},
}

var ignoredReasons = map[string]bool{
	"none": true, "job-completed-successfully": true, "job-printing": true, "job-incoming": true,
	"processing-to-stop-point": true, "job-queued": true, "job-data-insufficient": true,
	"cups-waiting-for-job-completed": true, "job-queued-for-marker": true,
}

var suffix = regexp.MustCompile(`-(report|warning|error)$`)

// cleanReasons drops the -report/-warning/-error suffixes and the reasons that mean "all fine".
func cleanReasons(raw []string) []string {
	var out []string
	for _, r := range raw {
		base := suffix.ReplaceAllString(r, "")
		if ignoredReasons[base] || ignoredReasons[r] {
			continue
		}
		out = append(out, base)
	}
	return out
}

// Explain turns state reasons into issues.
func Explain(rs []string, computer, admin string) []Issue {
	out := []Issue{}
	for _, r := range rs {
		e, ok := reasons[r]
		if !ok {
			out = append(out, Issue{Code: r, Severity: "warning", Text: fmt.Sprintf("The printer reported: %s.", r)})
			continue
		}
		fix := e.fix
		if strings.Contains(fix, "%[") {
			fix = fmt.Sprintf(fix, computer, admin)
		}
		out = append(out, Issue{Code: r, Severity: e.severity, Text: e.text, Fix: fix})
	}
	return out
}
