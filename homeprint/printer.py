"""Talk to CUPS and explain what the printer is doing in plain language."""

import glob
import os
import re
import threading
import time
from datetime import datetime, timedelta

import cups

from . import config

JOB_ATTRS = [
    "job-id", "job-name", "job-originating-user-name", "job-state", "job-state-reasons",
    "job-printer-state-message", "job-media-sheets-completed", "job-impressions-completed",
    "time-at-creation", "time-at-processing", "time-at-completed", "job-k-octets", "copies",
    "job-printer-uri",
]

JOB_STATES = {3: "queued", 4: "held", 5: "printing", 6: "stopped", 7: "canceled", 8: "failed", 9: "done"}
PRINTER_STATES = {3: "idle", 4: "printing", 5: "stopped"}

# printer-state-reasons / job-state-reasons -> (severity, plain-language explanation, what to do)
REASONS = {
    "media-empty": ("error", "The printer is out of paper.",
                    "Load paper in the rear tray, then press the printer's Black or Color button."),
    "media-needed": ("error", "The printer needs paper.",
                     "Load paper in the rear tray, then press the printer's Black or Color button."),
    "media-jam": ("error", "Paper is jammed.",
                  "Gently pull the stuck sheet out, then press the Black or Color button."),
    "cover-open": ("error", "A cover on the printer is open.", "Close it."),
    "door-open": ("error", "A door on the printer is open.", "Close it."),
    "marker-supply-low": ("warning", "One of the inks is low.", "Check the ink tanks on the front."),
    "marker-supply-empty": ("error", "One of the inks is empty.", "Refill the empty tank."),
    "offline-report": ("error", "The printer isn't responding.",
                       "Check that the printer is switched on and its USB cable is plugged into the XPS."),
    "connecting-to-device": ("warning", "Trying to reach the printer.",
                             "If this lasts more than a minute, check the printer is on and plugged in."),
    "paused": ("error", "Printing is paused on the computer.", "Ask Nizar to resume the printer queue."),
    "job-canceled-by-user": ("info", "Someone canceled this job.", ""),
    "job-canceled-by-operator": ("info", "This job was canceled.", ""),
    "document-format-error": ("error", "The printer couldn't read this file.",
                              "Try saving it as a PDF and printing that."),
    "document-unprintable-error": ("error", "This file can't be printed.",
                                   "Try saving it as a PDF and printing that."),
    "aborted-by-system": ("error", "The computer stopped this job because of an error.",
                          "Check the Status tab for details, then try again."),
    "job-hold-until-specified": ("info", "This job is on hold.", ""),
}
IGNORED_REASONS = {"none", "job-completed-successfully", "job-printing", "job-incoming",
                   "processing-to-stop-point", "job-queued", "job-data-insufficient",
                   "cups-waiting-for-job-completed", "job-queued-for-marker"}

STALL_SECONDS = 90

_lock = threading.Lock()
_progress = {}  # job_id -> (last message, time it last changed)


def _conn():
    return cups.Connection()


def _reason_list(value):
    if not value:
        return []
    if isinstance(value, str):
        value = [value]
    out = []
    for r in value:
        base = re.sub(r"-(report|warning|error)$", "", r)
        if base in IGNORED_REASONS or r in IGNORED_REASONS:
            continue
        out.append(base)
    return out


def explain_reasons(reasons):
    """Turn IPP state reasons into [{severity, text, fix}]."""
    out = []
    for r in reasons:
        if r in REASONS:
            sev, text, fix = REASONS[r]
        else:
            sev, text, fix = "warning", f"The printer reported: {r}.", ""
        out.append({"code": r, "severity": sev, "text": text, "fix": fix})
    return out


def usb_connected():
    """True if a printer with our USB vendor/product id is plugged in."""
    for d in glob.glob("/sys/bus/usb/devices/*"):
        try:
            with open(os.path.join(d, "idVendor")) as v, open(os.path.join(d, "idProduct")) as p:
                if v.read().strip() == config.USB_VENDOR and p.read().strip() == config.USB_PRODUCT:
                    return True
        except OSError:
            continue
    return False


def _job_progress(job_id, message, state):
    """Remember when a printing job's progress message last changed, to spot stalls."""
    now = time.time()
    with _lock:
        if state != 5:
            _progress.pop(job_id, None)
            return 0
        last = _progress.get(job_id)
        if not last or last[0] != message:
            _progress[job_id] = (message, now)
            return 0
        return now - last[1]


def _fmt_time(ts):
    return datetime.fromtimestamp(ts).isoformat(timespec="seconds") if ts else None


def describe_job(job_id, attrs, printer_state=None):
    state = attrs.get("job-state", 0)
    message = attrs.get("job-printer-state-message", "") or ""
    reasons = _reason_list(attrs.get("job-state-reasons"))
    issues = explain_reasons(reasons)
    stalled_for = _job_progress(job_id, message, state)

    if state == 3:
        summary = "Waiting in line"
        if printer_state and printer_state.get("state") == "stopped":
            summary = "Waiting: the printer is stopped"
    elif state == 4:
        summary = "On hold"
    elif state == 5:
        summary = message if message.lower().startswith("printing") else "Printing" + (f": {message}" if message else "")
        if stalled_for >= STALL_SECONDS:
            issues.insert(0, {
                "code": "stalled",
                "severity": "error",
                "text": f"No progress for {int(stalled_for // 60) or 1} min. The printer has probably stopped and is waiting.",
                "fix": "Most often it's out of paper: load paper, then press the printer's Black or Color button. "
                       "Also check for a jam or a blinking orange light.",
            })
    elif state == 9:
        summary = "Printed"
    elif state == 7:
        summary = "Canceled"
    elif state == 8:
        summary = "Failed"
    else:
        summary = JOB_STATES.get(state, "Unknown")

    timeline = []
    for key, label in (("time-at-creation", "Sent"), ("time-at-processing", "Started printing"),
                       ("time-at-completed", {9: "Finished", 7: "Canceled", 8: "Stopped with an error"}.get(state, "Ended"))):
        ts = attrs.get(key)
        if ts:
            timeline.append({"at": _fmt_time(ts), "label": label})

    return {
        "id": job_id,
        "name": attrs.get("job-name", ""),
        "user": attrs.get("job-originating-user-name", ""),
        "state": JOB_STATES.get(state, str(state)),
        "summary": summary,
        "message": message,
        "issues": issues,
        "timeline": timeline,
        "active": state in (3, 4, 5),
        "created": _fmt_time(attrs.get("time-at-creation")),
        "sheets": attrs.get("job-media-sheets-completed") or attrs.get("job-impressions-completed") or 0,
        "printer": "maintenance" if str(attrs.get("job-printer-uri", "")).endswith("/" + config.MAINT_QUEUE) else "print",
    }


def printer_status():
    c = _conn()
    out = {"queue": config.QUEUE, "usb": usb_connected()}
    try:
        a = c.getPrinterAttributes(config.QUEUE)
    except cups.IPPError as e:
        out.update(state="missing", issues=[{"code": "no-queue", "severity": "error",
                   "text": f"The print queue {config.QUEUE} doesn't exist on the XPS.",
                   "fix": "Ask Nizar: the printer needs to be added to CUPS again."}], detail=str(e))
        return out
    state = PRINTER_STATES.get(a.get("printer-state"), "unknown")
    reasons = _reason_list(a.get("printer-state-reasons"))
    issues = explain_reasons(reasons)
    if not out["usb"]:
        issues.insert(0, {"code": "usb", "severity": "error",
                          "text": "The XPS can't see the printer on USB.",
                          "fix": "Make sure the printer is switched on and its USB cable is plugged into the XPS. "
                                 "If it is, unplug the cable and plug it back in."})
    if not a.get("printer-is-accepting-jobs", True):
        issues.append({"code": "rejecting", "severity": "error",
                       "text": "The print queue isn't accepting new jobs.", "fix": "Ask Nizar to re-enable it."})
    out.update(
        state=state,
        message=a.get("printer-state-message", ""),
        issues=issues,
        accepting=bool(a.get("printer-is-accepting-jobs", True)),
        model=a.get("printer-make-and-model", ""),
        state_changed=_fmt_time(a.get("printer-state-change-time")),
    )
    return out


def jobs(which="all", limit=60, user=None):
    pstatus = printer_status()
    try:
        # One call returns jobs for every queue, including the maintenance one.
        raw = _conn().getJobs(which_jobs=which, my_jobs=False, requested_attributes=JOB_ATTRS)
    except cups.IPPError:
        raw = {}
    out = [describe_job(jid, attrs, pstatus) for jid, attrs in raw.items()]
    if user:
        out = [j for j in out if j["user"] == user]
    out.sort(key=lambda j: j["id"], reverse=True)
    return out[:limit]


def submit(path, title, user, copies=1, color=True, pages=None, fit=True):
    """Print a file. Returns the CUPS job id."""
    options = {"copies": str(max(1, min(int(copies), 50)))}
    options["ColorModel"] = "RGB" if color else "Gray"
    options["print-color-mode"] = "color" if color else "monochrome"
    if pages:
        options["page-ranges"] = pages
    if fit:
        options["print-scaling"] = "fit"
    cups.setUser(user)
    try:
        return _conn().printFile(config.QUEUE, path, title[:200], options)
    finally:
        cups.setUser(config.SERVICE_USER)


def submit_raw(data, title, user, queue):
    """Send raw bytes (maintenance commands) to a queue."""
    import tempfile
    with tempfile.NamedTemporaryFile(prefix="homeprint-", suffix=".bin", delete=False) as f:
        f.write(data)
        path = f.name
    cups.setUser(user)
    try:
        return _conn().printFile(queue, path, title, {"raw": "true"})
    finally:
        cups.setUser(config.SERVICE_USER)
        try:
            os.unlink(path)
        except OSError:
            pass


def cancel(job_id):
    _conn().cancelJob(int(job_id))


def get_job(job_id):
    for j in jobs(limit=100):
        if j["id"] == int(job_id):
            return j
    return None


_LOG_RE = re.compile(r"^([EWI]) \[([^\]]+)\] (.*)$")
_NOISE = ("Bad driver information file", "CreateProfile failed", "Unable to configure printer options",
          "cups-driverd", "Expiring subscriptions", "cups-brf")


def recent_log(hours=24, limit=40):
    """Errors and warnings from the CUPS log, newest first, without known noise."""
    cutoff = datetime.now().astimezone() - timedelta(hours=hours)
    lines = []
    try:
        with open(config.CUPS_ERROR_LOG, errors="replace") as f:
            for line in f:
                m = _LOG_RE.match(line.rstrip("\n"))
                if not m or any(n in line for n in _NOISE):
                    continue
                level, ts, msg = m.groups()
                try:
                    when = datetime.strptime(ts, "%d/%b/%Y:%H:%M:%S %z")
                except ValueError:
                    continue
                if when < cutoff:
                    continue
                lines.append({"at": when.isoformat(timespec="seconds"),
                              "level": {"E": "error", "W": "warning", "I": "info"}[level], "text": msg})
    except OSError as e:
        return [{"at": None, "level": "warning", "text": f"Can't read the CUPS log: {e.strerror}"}]
    return lines[::-1][:limit]
