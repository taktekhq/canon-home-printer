# homeprint

A small website for the family printer (Canon PIXMA G3010, plugged into the `xps` machine by USB).
Family members open it from anywhere, sign in with Cloudflare Access, and can:

- **Print** PDFs, photos (JPG/PNG) and text files: colour or black & white, copies, page ranges.
- **Scan** from the glass to PDF or JPG. Each person only sees their own scans (kept 30 days).
- **See what's happening**: a live print queue with who sent what, and plain-language problems and fixes
  ("No progress for 2 min… most often it's out of paper: load paper, then press Black or Color").
- **Fix print quality** (maintainers only): nozzle check, cleaning and deep cleaning, using
  [canon-ij-maint](https://github.com/nizarmah/canon-ij-maint).

## How it fits together

```
browser ──HTTPS──> Cloudflare Access (sign-in) ──> Cloudflare Tunnel ──> cloudflared on xps
                                                                   └──> homeprint on 127.0.0.1:8091
                                                                          ├─ CUPS (lp queues)
                                                                          └─ scanimage (SANE)
```

- The app only listens on `127.0.0.1`; nothing is opened on the xps firewall.
- Every request must carry a valid Cloudflare Access JWT (`Cf-Access-Jwt-Assertion`). The app checks
  its signature against the team's keys and the application's AUD tag, and uses the email inside.
- CUPS hides job owners from other users, so the app records who sent each job in its own
  activity log (`~/.local/share/homeprint/events.jsonl`).
- Uploaded files are deleted as soon as CUPS has taken the job.

## Printer setup on xps

| Queue | Driver | Used for |
|---|---|---|
| `Canon_G3010` | Gutenprint | printing |
| `Canon_G3010_Canon` | Canon cnijfilter2 | maintenance commands (raw) |

Scanner: `pixma:04A9183B_92B81B`.

## Run it

Dependencies are Ubuntu packages: `python3-fastapi python3-uvicorn python3-multipart python3-jwt python3-cryptography python3-cups`.

```bash
# local test, no sign-in, only on this machine
HOMEPRINT_DEV_USER=you@example.com python3 -m uvicorn homeprint.app:app --host 127.0.0.1 --port 8092
```

Production:

```bash
mkdir -p ~/.config/homeprint
cp deploy/homeprint.env.example ~/.config/homeprint/homeprint.env   # fill in the Cloudflare values
sudo cp deploy/homeprint.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now homeprint
```

Then point a Cloudflare Tunnel public hostname at `http://127.0.0.1:8091`, and protect that hostname
with a Cloudflare Access application that allows the family's emails.

## API

All under `/api`, all require sign-in: `me`, `status`, `jobs`, `jobs/{id}`, `jobs/{id}/cancel`,
`print` (multipart), `scan`, `scan/{id}`, `scans`, `scans/file/{name}`, `maintenance`, `log`.
`/healthz` is unauthenticated.
