#!/usr/bin/env python3
"""canon-ij-maint: run maintenance on Canon PIXMA inkjets from Linux, without Canon's driver.

Newer Canon PIXMA printers (e.g. the G3010 MegaTank) take maintenance commands as small
"IVEC" XML messages. Canon's Linux driver only exposes a regular cleaning and a nozzle check;
this tool builds the same bytes Canon's driver sends, and also lets you pick a deep cleaning.

Send the bytes to the printer one of three ways:
  --queue NAME     a CUPS queue for the printer (sent with `lp -o raw`)
  --device PATH    the USB printer device, e.g. /dev/usb/lp0 (needs write access)
  --output FILE    just write the bytes to a file ("-" for stdout)

Stdlib only. Python 3.8+.

Not affiliated with or endorsed by Canon Inc. "Canon" and "PIXMA" are trademarks of Canon Inc.
"""

import argparse
import glob
import os
import subprocess
import sys

NS = 'xmlns:ivec="http://www.canon.com/ns/cmd/2008/07/common/"'
JOB_ID = "00000001"

INK_GROUPS = ("all", "black", "color")
CLEAN_TYPES = ("regular", "deep")


def _start_job(job_id):
    return (
        '<?xml version="1.0" encoding="utf-8" ?>'
        f"<cmd {NS}><ivec:contents><ivec:operation>StartJob</ivec:operation>"
        '<ivec:param_set servicetype="maintenance">'
        f"<ivec:jobID>{job_id}</ivec:jobID><ivec:bidi>0</ivec:bidi>"
        "<ivec:jobname/><ivec:username/><ivec:computername/>"
        "<ivec:job_description><![CDATA[1]]></ivec:job_description>"
        "</ivec:param_set></ivec:contents></cmd>"
    )


def _end_job(job_id):
    return (
        '<?xml version="1.0" encoding="utf-8" ?>'
        f"<cmd {NS}><ivec:contents><ivec:operation>EndJob</ivec:operation>"
        '<ivec:param_set servicetype="maintenance">'
        f"<ivec:jobID>{job_id}</ivec:jobID>"
        "</ivec:param_set></ivec:contents></cmd>"
    )


def _operation(name, params, job_id):
    return (
        '<?xml version="1.0" encoding="utf-8"?>'
        f"<cmd {NS}><ivec:contents><ivec:operation>{name}</ivec:operation>"
        f'<ivec:param_set servicetype="maintenance">{params}'
        f"<ivec:jobID>{job_id}</ivec:jobID>"
        "</ivec:param_set></ivec:contents></cmd>"
    )


def wrap(op_xml, job_id=JOB_ID):
    """Frame one operation the way Canon's cmdtocanonij filter does.

    StartJob, then as many NUL bytes as StartJob is long, then the operation, then EndJob.
    """
    start = _start_job(job_id).encode()
    return start + b"\x00" * len(start) + op_xml.encode() + _end_job(job_id).encode()


def cleaning(kind="regular", group="all", job_id=JOB_ID):
    if kind not in CLEAN_TYPES:
        raise ValueError(f"cleaning type must be one of {CLEAN_TYPES}")
    if group not in INK_GROUPS:
        raise ValueError(f"ink group must be one of {INK_GROUPS}")
    params = f"<ivec:inkgroup>{group}</ivec:inkgroup><ivec:type>{kind}</ivec:type>"
    return wrap(_operation("Cleaning", params, job_id), job_id)


def nozzle_check(job_id=JOB_ID):
    return wrap(_operation("TestPrint", "<ivec:type>nozzle_check</ivec:type>", job_id), job_id)


def find_canon_queues():
    """Return CUPS queue names whose device URI points at a Canon printer."""
    try:
        out = subprocess.run(["lpstat", "-v"], capture_output=True, text=True, check=True).stdout
    except (OSError, subprocess.CalledProcessError):
        return []
    queues = []
    for line in out.splitlines():
        # "device for NAME: URI"
        if line.startswith("device for ") and "canon" in line.lower():
            queues.append(line[len("device for "):].split(":", 1)[0])
    return queues


def send(data, queue=None, device=None, output=None, title="Canon maintenance"):
    if output:
        if output == "-":
            sys.stdout.buffer.write(data)
        else:
            with open(output, "wb") as f:
                f.write(data)
        return f"wrote {len(data)} bytes to {output}"
    if device:
        with open(device, "wb") as f:
            f.write(data)
        return f"sent {len(data)} bytes to {device}"
    if queue:
        res = subprocess.run(
            ["lp", "-d", queue, "-o", "raw", "-t", title],
            input=data, capture_output=True, check=False,
        )
        if res.returncode != 0:
            raise RuntimeError(res.stderr.decode().strip() or "lp failed")
        return res.stdout.decode().strip()
    raise ValueError("no destination")


def pick_destination(args):
    if args.output or args.device or args.queue:
        return args
    queues = find_canon_queues()
    if len(queues) == 1:
        args.queue = queues[0]
        print(f"using CUPS queue {args.queue}", file=sys.stderr)
        return args
    devices = sorted(glob.glob("/dev/usb/lp*"))
    if not queues and len(devices) == 1:
        args.device = devices[0]
        print(f"using device {args.device}", file=sys.stderr)
        return args
    found = ", ".join(queues) if queues else "none"
    sys.exit(f"Pick a destination with --queue, --device or --output (Canon queues found: {found}).")


def main(argv=None):
    p = argparse.ArgumentParser(
        prog="canon-ij-maint",
        description="Run cleaning and nozzle checks on Canon PIXMA inkjets without Canon's driver.",
    )
    dest = p.add_argument_group("destination (default: the only Canon CUPS queue)")
    dest.add_argument("--queue", help="CUPS queue name")
    dest.add_argument("--device", help="USB printer device, e.g. /dev/usb/lp0")
    dest.add_argument("--output", help='write bytes to a file instead ("-" = stdout)')

    sub = p.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("clean", help="clean the print heads")
    c.add_argument("--deep", action="store_true", help="deep cleaning (uses much more ink)")
    c.add_argument("--group", choices=INK_GROUPS, default="all",
                   help="which ink group to clean (default: all; the G3010 ignores 'color')")
    sub.add_parser("nozzle-check", help="print the nozzle check pattern")
    sub.add_parser("list", help="list Canon CUPS queues and USB printer devices")

    args = p.parse_args(argv)

    if args.cmd == "list":
        for q in find_canon_queues():
            print(f"queue   {q}")
        for d in sorted(glob.glob("/dev/usb/lp*")):
            print(f"device  {d}")
        return 0

    if args.cmd == "clean":
        data = cleaning("deep" if args.deep else "regular", args.group)
        title = f"{'Deep' if args.deep else 'Regular'} cleaning ({args.group})"
    else:
        data = nozzle_check()
        title = "Nozzle check"

    args = pick_destination(args)
    try:
        print(send(data, args.queue, args.device, args.output, title), file=sys.stderr)
    except (OSError, RuntimeError) as e:
        sys.exit(f"error: {e}")
    if args.cmd == "clean" and not args.output:
        print("The printer should start whirring within a few seconds. "
              "If it stays silent, it ignored the command.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
