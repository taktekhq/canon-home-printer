"""homeprint: a small web app to print, scan and see what the family printer is doing."""

import json
import re
import shutil
import threading
import time
import uuid
from datetime import datetime
from pathlib import Path

import cups
from fastapi import Depends, FastAPI, File, Form, HTTPException, Request, UploadFile
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel

from . import auth, canon_ij_maint, config, printer, scanner

app = FastAPI(title="homeprint", docs_url=None, redoc_url=None, openapi_url=None)
STATIC = Path(__file__).parent / "static"
_event_lock = threading.Lock()

PAGE_RANGE = re.compile(r"^\s*\d+(\s*-\s*\d+)?(\s*,\s*\d+(\s*-\s*\d+)?)*\s*$")
ACCEPTED = "PDF, JPG, PNG or plain text"


def log_event(user, action, **details):
    rec = {"at": datetime.now().isoformat(timespec="seconds"), "user": user, "action": action, **details}
    with _event_lock, open(config.EVENT_LOG, "a") as f:
        f.write(json.dumps(rec) + "\n")


def recent_events(limit=50):
    try:
        with open(config.EVENT_LOG) as f:
            lines = f.readlines()[-limit:]
    except OSError:
        return []
    out = []
    for line in reversed(lines):
        try:
            out.append(json.loads(line))
        except ValueError:
            pass
    return out


def _sniff(path, filename):
    with open(path, "rb") as f:
        head = f.read(2048)
    if head.startswith(b"%PDF"):
        return "pdf"
    if head.startswith(b"\xff\xd8\xff"):
        return "jpeg"
    if head.startswith(b"\x89PNG"):
        return "png"
    if filename.lower().endswith(".txt"):
        try:
            head.decode("utf-8")
            return "text"
        except UnicodeDecodeError:
            pass
    return None


User = Depends(auth.current_user)


@app.exception_handler(cups.IPPError)
async def _cups_error(request: Request, exc: cups.IPPError):
    code, msg = exc.args if len(exc.args) == 2 else (0, str(exc))
    return JSONResponse(status_code=502, content={"detail": f"The print system (CUPS) refused: {msg}", "cups_code": code})


@app.get("/api/me")
def me(user: str = User):
    return {"email": user, "name": auth.display_name(user), "can_maintain": auth.can_maintain(user),
            "printer": config.PRINTER_NAME, "max_upload_mb": config.MAX_UPLOAD_MB}


@app.get("/api/status")
def status(user: str = User):
    p = printer.printer_status()
    active = [j for j in printer.jobs(which="not-completed") if j["active"]]
    try:
        free_gb = shutil.disk_usage(config.DATA_DIR).free / 1e9
    except OSError:
        free_gb = None
    checks = [
        {"name": "Printer plugged in", "ok": p.get("usb", False),
         "text": "The XPS sees the printer on USB." if p.get("usb") else "The XPS can't see the printer on USB."},
        {"name": "Print queue", "ok": p.get("state") not in ("missing", "stopped") and p.get("accepting", False),
         "text": {"idle": "Ready.", "printing": "Printing now.", "stopped": "Stopped.", "missing": "Missing."}.get(p.get("state"), p.get("state", ""))},
        {"name": "Scanner", "ok": True, "text": "Scanning now." if scanner.busy() else "Ready."},
        {"name": "Disk space on the XPS", "ok": free_gb is None or free_gb > 2,
         "text": f"{free_gb:.0f} GB free." if free_gb is not None else "Unknown."},
    ]
    issues = list(p.get("issues", []))
    for j in active:
        for i in j["issues"]:
            issues.append({**i, "job": j["id"]})
    return {"printer": p, "active_jobs": active, "checks": checks, "issues": issues,
            "scanner_busy": scanner.busy(), "checked_at": datetime.now().isoformat(timespec="seconds")}


MAINT_TITLES = {"nozzle_check": "Nozzle check", "clean": "Cleaning", "deep_clean": "Deep cleaning"}


def _job_owners():
    """CUPS hides job names and owners from other users, so we keep our own record."""
    owners = {}
    for e in recent_events(2000):
        if e.get("job") and e.get("action") in ("print", "maintenance") and e["job"] not in owners:
            owners[e["job"]] = (e.get("user", ""), e.get("file") or MAINT_TITLES.get(e.get("what"), "Maintenance"))
    return owners


def _enrich(jobs, user):
    owners = _job_owners()
    for j in jobs:
        owner, name = owners.get(j["id"], (j["user"], j["name"]))
        j["user"], j["name"] = owner, name or j["name"] or "Sent from another computer"
        j["by"] = auth.display_name(owner) if owner else "Someone else"
        j["mine"] = owner == user
    return jobs


@app.get("/api/jobs")
def list_jobs(mine: bool = False, user: str = User):
    jobs = _enrich(printer.jobs(limit=60), user)
    if mine:
        jobs = [j for j in jobs if j["mine"]]
    return jobs[:40]


def _find_job(job_id, user):
    j = printer.get_job(job_id)
    return _enrich([j], user)[0] if j else None


@app.get("/api/jobs/{job_id}")
def job(job_id: int, user: str = User):
    j = _find_job(job_id, user)
    if not j:
        raise HTTPException(404, "No such print job.")
    return j


@app.post("/api/jobs/{job_id}/cancel")
def cancel(job_id: int, user: str = User):
    j = _find_job(job_id, user)
    if not j:
        raise HTTPException(404, "No such print job.")
    if j["user"] != user and not auth.can_maintain(user):
        raise HTTPException(403, "You can only cancel your own jobs.")
    if not j["active"]:
        raise HTTPException(409, "That job has already finished.")
    printer.cancel(job_id)
    log_event(user, "cancel", job=job_id)
    return {"ok": True}


@app.post("/api/print")
async def print_file(file: UploadFile = File(...), copies: int = Form(1), color: bool = Form(True),
                     pages: str = Form(""), fit: bool = Form(True), user: str = User):
    if pages and not PAGE_RANGE.match(pages):
        raise HTTPException(400, 'Pages should look like "1-3" or "1,4,6-8".')
    dest = config.UPLOAD_DIR / f"{uuid.uuid4().hex}"
    size, limit = 0, config.MAX_UPLOAD_MB * 1024 * 1024
    with open(dest, "wb") as out:
        while chunk := await file.read(1024 * 1024):
            size += len(chunk)
            if size > limit:
                out.close()
                dest.unlink(missing_ok=True)
                raise HTTPException(413, f"That file is bigger than {config.MAX_UPLOAD_MB} MB.")
            out.write(chunk)
    kind = _sniff(dest, file.filename or "")
    if not kind:
        dest.unlink(missing_ok=True)
        raise HTTPException(415, f"This kind of file can't be printed directly. Use {ACCEPTED}. "
                                 "For Word or other documents, save or export them as PDF first.")
    title = file.filename or "document"
    try:
        # CUPS keeps its own copy of the file once the job is submitted.
        job_id = printer.submit(str(dest), title, user, copies=copies, color=color,
                                pages=pages.replace(" ", "") or None, fit=fit)
    finally:
        dest.unlink(missing_ok=True)
    log_event(user, "print", job=job_id, file=title, kind=kind, size=size, copies=copies, color=color, pages=pages)
    return {"job": job_id}


class ScanRequest(BaseModel):
    resolution: int = 300
    mode: str = "Color"
    format: str = "pdf"


@app.post("/api/scan")
def scan(req: ScanRequest, user: str = User):
    # The G3010 can't scan while it's printing or cleaning; the scan would just fail.
    if any(j["active"] and j["state"] == "printing" for j in printer.jobs(which="not-completed")):
        raise HTTPException(409, "The printer is busy printing right now. Scan once it's finished.")
    try:
        job = scanner.start(user, req.resolution, req.mode, req.format)
    except ValueError:
        raise HTTPException(400, "Unsupported scan settings.")
    except RuntimeError as e:
        raise HTTPException(409, str(e))
    log_event(user, "scan", resolution=req.resolution, mode=req.mode, format=req.format, scan=job["id"])
    return job


@app.get("/api/scan/{scan_id}")
def scan_status(scan_id: str, user: str = User):
    j = scanner.get(scan_id, user)
    if not j:
        raise HTTPException(404, "No such scan.")
    return j


@app.get("/api/scans")
def scans(user: str = User):
    return scanner.list_files(user)


@app.get("/api/scans/file/{name}")
def scan_file(name: str, download: bool = False, user: str = User):
    p = scanner.file_path(user, name)
    if not p:
        raise HTTPException(404, "No such scan.")
    media = scanner.FORMATS.get({"jpg": "jpeg"}.get(p.suffix[1:], p.suffix[1:]), "application/octet-stream")
    return FileResponse(p, media_type=media, filename=name if download else None)


@app.delete("/api/scans/file/{name}")
def delete_scan(name: str, user: str = User):
    p = scanner.file_path(user, name)
    if not p:
        raise HTTPException(404, "No such scan.")
    p.unlink()
    log_event(user, "delete-scan", file=name)
    return {"ok": True}


class MaintenanceRequest(BaseModel):
    action: str


MAINTENANCE = {
    "nozzle_check": ("Nozzle check", lambda: canon_ij_maint.nozzle_check()),
    "clean": ("Cleaning", lambda: canon_ij_maint.cleaning("regular", "all")),
    "deep_clean": ("Deep cleaning", lambda: canon_ij_maint.cleaning("deep", "all")),
}


@app.post("/api/maintenance")
def maintenance(req: MaintenanceRequest, user: str = User):
    if not auth.can_maintain(user):
        raise HTTPException(403, "Only Nizar can run printer maintenance.")
    if req.action not in MAINTENANCE:
        raise HTTPException(400, "Unknown maintenance action.")
    if req.action == "deep_clean":
        day_ago = time.time() - 86400
        recent = [e for e in recent_events(200) if e.get("action") == "maintenance" and e.get("what") == "deep_clean"
                  and datetime.fromisoformat(e["at"]).timestamp() > day_ago]
        if len(recent) >= 2:
            raise HTTPException(429, "Two deep cleanings already ran in the last 24 hours. "
                                     "More won't help and wastes ink: let it rest, or the print head may need replacing.")
    title, build = MAINTENANCE[req.action]
    job_id = printer.submit_raw(build(), title, user, config.MAINT_QUEUE)
    log_event(user, "maintenance", what=req.action, job=job_id)
    return {"job": job_id, "title": title}


@app.get("/api/log")
def debug_log(user: str = User):
    events = recent_events(60)
    for e in events:
        e["by"] = auth.display_name(e.get("user", ""))
    return {"cups": printer.recent_log(), "events": events}


@app.get("/healthz")
def healthz():
    return {"ok": True}


def _cleanup_loop():
    while True:
        cutoff = time.time() - config.UPLOAD_KEEP_DAYS * 86400
        for p in config.UPLOAD_DIR.iterdir():
            try:
                if p.stat().st_mtime < cutoff:
                    p.unlink()
            except OSError:
                pass
        scanner.cleanup()
        time.sleep(3600)


@app.on_event("startup")
def _start():
    threading.Thread(target=_cleanup_loop, daemon=True).start()


app.mount("/", StaticFiles(directory=STATIC, html=True), name="static")
