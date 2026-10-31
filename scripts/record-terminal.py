#!/usr/bin/env python3
"""Record actual PTY output as an asciicast v2 artifact, without input events."""
import errno
import fcntl
import json
import os
import pty
import select
import struct
import subprocess
import sys
import termios
import time

destination, *command = sys.argv[1:]
if not command:
    raise SystemExit("usage: record-terminal.py OUTPUT.cast COMMAND [ARGS...]")
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 110, 0, 0))
started = time.monotonic()
with open(destination, "x", encoding="utf-8") as recording:
    recording.write(json.dumps({"version": 2, "width": 110, "height": 32,
                                "timestamp": int(time.time()), "env": {"TERM": "xterm-256color"}}) + "\n")
    process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=slave, stderr=slave)
    os.close(slave)
    try:
        while True:
            if not select.select([master], [], [], 1)[0]:
                if process.poll() is None:
                    continue
            try:
                data = os.read(master, 65536)
            except OSError as error:
                if error.errno == errno.EIO:
                    break
                raise
            if not data:
                break
            output = data.decode("utf-8", errors="replace")
            recording.write(json.dumps([round(time.monotonic() - started, 6), "o", output]) + "\n")
            recording.flush()
            sys.stdout.write(output)
            sys.stdout.flush()
    finally:
        os.close(master)
        if process.poll() is None:
            process.terminate()
        status = process.wait()
raise SystemExit(status)
