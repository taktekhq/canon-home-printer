"""Settings, read from environment variables (see deploy/homeprint.env.example)."""

import os
from pathlib import Path


def _env(name, default=""):
    return os.environ.get(name, default).strip()


QUEUE = _env("HOMEPRINT_QUEUE", "Canon_G3010")
MAINT_QUEUE = _env("HOMEPRINT_MAINT_QUEUE", "Canon_G3010_Canon")
SCANNER = _env("HOMEPRINT_SCANNER", "pixma:04A9183B_92B81B")
USB_VENDOR = _env("HOMEPRINT_USB_VENDOR", "04a9")
USB_PRODUCT = _env("HOMEPRINT_USB_PRODUCT", "183b")
PRINTER_NAME = _env("HOMEPRINT_PRINTER_NAME", "Canon G3010")

DATA_DIR = Path(_env("HOMEPRINT_DATA_DIR", str(Path.home() / ".local/share/homeprint")))
UPLOAD_DIR = DATA_DIR / "uploads"
SCAN_DIR = DATA_DIR / "scans"
EVENT_LOG = DATA_DIR / "events.jsonl"
NAMES_FILE = Path(_env("HOMEPRINT_NAMES", str(DATA_DIR / "names.json")))

CUPS_ERROR_LOG = _env("HOMEPRINT_CUPS_LOG", "/var/log/cups/error_log")
SERVICE_USER = _env("USER", "homeprint")

MAX_UPLOAD_MB = int(_env("HOMEPRINT_MAX_UPLOAD_MB", "50"))
UPLOAD_KEEP_DAYS = int(_env("HOMEPRINT_UPLOAD_KEEP_DAYS", "7"))
SCAN_KEEP_DAYS = int(_env("HOMEPRINT_SCAN_KEEP_DAYS", "30"))

# Cloudflare Access. Both must be set in production; requests without a valid token are refused.
CF_TEAM_DOMAIN = _env("CF_ACCESS_TEAM_DOMAIN")  # e.g. taktek.cloudflareaccess.com
CF_AUD = _env("CF_ACCESS_AUD")                  # the Access application's AUD tag
# Local development only: trust this email instead of Cloudflare.
DEV_USER = _env("HOMEPRINT_DEV_USER")
# Emails allowed to run cleanings (comma-separated). Empty = everyone signed in.
MAINT_USERS = {e.lower() for e in _env("HOMEPRINT_MAINT_USERS").split(",") if e}

for d in (UPLOAD_DIR, SCAN_DIR):
    d.mkdir(parents=True, exist_ok=True)
