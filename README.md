# canon-home-printer

A small website for the **Canon PIXMA printer at home**, so the whole family can use it from
anywhere, signed in with their own email. Built for a Canon PIXMA G3010 (MegaTank) plugged by
USB into an always-on Linux laptop; it should work with any CUPS printer, and the maintenance
buttons with most recent PIXMA models.

What the family can do:

- **Print** PDFs, Word, Excel and PowerPoint files, photos (JPG/PNG) and text: colour or grey,
  copies, page ranges. Office files are converted to PDF with LibreOffice first.
- **Scan** from the glass to PDF or JPG. Each person only sees their own scans (kept 30 days).
- **See what's happening**: a live queue with who sent what, and problems in plain language with
  the fix ("No progress for 2 min. Most often it's out of paper: load paper, then press Black or Color").
- **Fix print quality** (maintainers only): nozzle check, cleaning and deep cleaning, through
  [canon-ij-maint](https://github.com/taktekhq/canon-ij-maint). At most two deep cleanings a day.

Website: <https://taktek.io/canon-home-printer/>

One Go binary with the page built in. It needs CUPS, `lp`, and `scanimage` (SANE). LibreOffice is optional:
with `soffice` installed, Word and other office files are accepted; without it they're refused with a hint to save as PDF.

## How it fits together

```
phone or laptop ──HTTPS──> Cloudflare Access (sign-in by email code)
                              └──> Cloudflare Tunnel ──> cloudflared on the home laptop
                                                           └──> canon-home-printer on 127.0.0.1:8091
                                                                  ├─ CUPS (lp, IPP on localhost:631)
                                                                  └─ scanimage (SANE, USB)
```

- Nothing is opened on the home router or firewall; `cloudflared` dials out.
- The app listens on `127.0.0.1` only, and still checks every request's Cloudflare Access token
  (`Cf-Access-Jwt-Assertion`): signature against the team's keys, the application's AUD tag, the
  issuer and expiry. Without both settings it refuses everything. `/healthz` is open.
- CUPS hides job owners from other users, so the app keeps its own activity log
  (`events.jsonl` in the data folder, `/var/lib/canon-home-printer` with the service file) to show who sent each job.
- Uploads are deleted as soon as CUPS has the job.

## Set it up

1. **Printer.** Install `cups printer-driver-gutenprint sane-utils` (and for Word files
   `libreoffice-writer-nogui libreoffice-calc-nogui libreoffice-impress-nogui fonts-crosextra-carlito
   fonts-crosextra-caladea fonts-noto-core`), plug the printer in, and add a
   queue (Gutenprint has the G3010):

   ```bash
   sudo lpadmin -p Canon_G3010 -E -v "$(lpinfo -v | grep -o 'usb://Canon/[^ ]*')" \
     -m "$(lpinfo -m | grep -i 'g3010' | head -1 | cut -d' ' -f1)" -o PageSize=A4
   scanimage -L   # the scanner name for CHP_SCANNER
   ```

   For stronger colour and deeper black on plain paper (tested on the G3010; prints take about twice
   as long):

   ```bash
   sudo lpadmin -p Canon_G3010 -o StpColorCorrection-default=Bright -o Resolution-default=600dpi
   ```

   Ubuntu desktop may auto-create a second queue with a wrong driver when the printer is plugged
   in; delete it.

2. **App.** Build or download the binary and install the service:

   ```bash
   go build -o canon-home-printer ./cmd/canon-home-printer
   sudo install canon-home-printer /usr/local/bin/
   sudo install -m 640 deploy/canon-home-printer.env.example /etc/canon-home-printer.env  # then edit
   sudo cp deploy/canon-home-printer.service /etc/systemd/system/   # set User=
   sudo systemctl daemon-reload && sudo systemctl enable --now canon-home-printer
   ```

   The service user needs to be in the `lp`, `lpadmin` and `scanner` groups.

3. **Cloudflare.** On a domain in your Cloudflare account:
   - create a Cloudflare Tunnel with a public hostname (say `print.example.com`) pointing at
     `http://127.0.0.1:8091`, and run `cloudflared` as a service on the laptop;
   - create a Zero Trust Access application for that hostname, with an Allow policy listing the
     family's emails (the One-time PIN login works with any email);
   - put the team domain (`yourteam.cloudflareaccess.com`) and the application's AUD tag in the
     env file and restart the service.

## Try it locally

```bash
CHP_DEV_USER=you@example.com CHP_ADDR=127.0.0.1:8092 go run ./cmd/canon-home-printer
```

`CHP_DEV_USER` trusts that email for every request and only works while Cloudflare Access isn't
configured. Never use it on a machine anyone else can reach.

## Settings

All environment variables; see [`deploy/canon-home-printer.env.example`](deploy/canon-home-printer.env.example).
`CHP_ADMIN_NAME` and `CHP_COMPUTER_NAME` are the words the page uses in fixes ("Ask Alex",
"plugged into the laptop").

## API

All under `/api`, all need sign-in: `me`, `status`, `jobs`, `jobs/{id}`, `jobs/{id}/cancel`,
`print` (multipart), `scan`, `scan/{id}`, `scans`, `scans/file/{name}`, `maintenance`, `log`.

## Develop

```bash
go test ./...
```

## License

MIT. Not affiliated with or endorsed by Canon Inc. "Canon" and "PIXMA" are trademarks of
Canon Inc., used only to say which printer this is for.
