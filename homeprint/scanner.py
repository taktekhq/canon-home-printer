"""Run scans with scanimage, one at a time, in the background."""

import hashlib
import os
import subprocess
import threading
import time
import uuid
from datetime import datetime

from . import config

FORMATS = {"pdf": "application/pdf", "jpeg": "image/jpeg", "png": "image/png"}
RESOLUTIONS = (150, 300, 600)
MODES = ("Color", "Gray")

_lock = threading.Lock()
_jobs = {}  # id -> dict
_busy = threading.Lock()


def _user_dir(email):
    d = config.SCAN_DIR / hashlib.sha256(email.encode()).hexdigest()[:16]
    d.mkdir(parents=True, exist_ok=True)
    return d


def _explain(stderr, code):
    s = stderr.lower()
    if "device busy" in s or "busy" in s:
        return ("The scanner is busy.", "Wait for the printer to finish what it's doing, then try again.")
    if "invalid argument" in s or "no such device" in s or "failed to open" in s or "not found" in s:
        return ("The XPS can't reach the scanner.",
                "Check that the printer is on and its USB cable is plugged into the XPS, then try again.")
    if "cover" in s:
        return ("The scanner lid seems open or blocked.", "Close the lid and try again.")
    if code == -9 or "timed out" in s:
        return ("The scan took too long and was stopped.", "Try again, or pick a lower resolution.")
    return ("The scan failed.", "Try again. If it keeps failing, the details below help Nizar debug it.")


def _run(job):
    out = _user_dir(job["user"]) / job["file"]
    cmd = ["scanimage", "-d", config.SCANNER, "--mode", job["mode"],
           "--resolution", str(job["resolution"]), f"--format={job['format']}", "-o", str(out)]
    job.update(state="scanning", started=time.time(), command=" ".join(cmd))
    with _busy:
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
            code, err = res.returncode, res.stderr.strip()
        except subprocess.TimeoutExpired:
            code, err = -9, "timed out after 300s"
        except OSError as e:
            code, err = -1, str(e)
    job["finished"] = time.time()
    job["seconds"] = round(job["finished"] - job["started"], 1)
    if code == 0 and out.exists() and out.stat().st_size > 0:
        job.update(state="done", size=out.stat().st_size)
    else:
        text, fix = _explain(err, code)
        job.update(state="failed", error=text, fix=fix, detail=err[-800:], exit_code=code)
        try:
            out.unlink()
        except OSError:
            pass


def start(user, resolution=300, mode="Color", fmt="pdf"):
    if resolution not in RESOLUTIONS or mode not in MODES or fmt not in FORMATS:
        raise ValueError("bad scan settings")
    if _busy.locked():
        raise RuntimeError("Another scan is in progress. Wait for it to finish.")
    ext = {"jpeg": "jpg"}.get(fmt, fmt)
    stamp = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
    job = {"id": uuid.uuid4().hex[:12], "user": user, "resolution": resolution, "mode": mode,
           "format": fmt, "file": f"scan_{stamp}.{ext}", "state": "queued", "created": time.time()}
    with _lock:
        _jobs[job["id"]] = job
    threading.Thread(target=_run, args=(job,), daemon=True).start()
    return public(job)


def public(job):
    return {k: v for k, v in job.items() if k not in ("user",)} | {
        "url": f"/api/scans/file/{job['file']}" if job.get("state") == "done" else None}


def get(job_id, user):
    j = _jobs.get(job_id)
    if not j or j["user"] != user:
        return None
    return public(j)


def busy():
    return _busy.locked()


def list_files(user):
    d = _user_dir(user)
    files = []
    for p in sorted(d.iterdir(), key=lambda p: p.stat().st_mtime, reverse=True):
        if p.is_file():
            st = p.stat()
            files.append({"name": p.name, "size": st.st_size,
                          "created": datetime.fromtimestamp(st.st_mtime).isoformat(timespec="seconds"),
                          "url": f"/api/scans/file/{p.name}"})
    return files


def file_path(user, name):
    if "/" in name or name.startswith("."):
        return None
    p = _user_dir(user) / name
    return p if p.is_file() else None


def cleanup():
    cutoff = time.time() - config.SCAN_KEEP_DAYS * 86400
    for root, _, names in os.walk(config.SCAN_DIR):
        for n in names:
            p = os.path.join(root, n)
            try:
                if os.path.getmtime(p) < cutoff:
                    os.unlink(p)
            except OSError:
                pass
