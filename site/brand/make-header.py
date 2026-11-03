#!/usr/bin/env python3
"""Write docs/assets/hutch-header.svg using the site's own dither, run in headless Firefox.

usage: make-header.py [out.svg] [seconds into the animation]
"""
import json, os, pathlib, socket, subprocess, sys, tempfile, time

HERE = pathlib.Path(__file__).resolve().parent
PAGE = HERE.parent / 'dist' / 'index.html'
OUT = sys.argv[1] if len(sys.argv) > 1 else str(HERE.parent.parent / 'docs' / 'assets' / 'hutch-header.svg')
T = float(sys.argv[2]) if len(sys.argv) > 2 else 3.0
W, H, PX, PY, PW, PH, CELL, ZOOM = 1280, 560, 824, 16, 440, 528, 6, 1.0
GLYPHS = ' .·:-=+x*#%@'
PORT = 2828 + os.getpid() % 1000


class Marionette:
    def __init__(self, port):
        for _ in range(100):
            try:
                self.sock = socket.create_connection(('127.0.0.1', port))
                break
            except OSError:
                time.sleep(0.1)
        else:
            raise SystemExit('could not reach Firefox')
        self.buf, self.id = b'', 0
        self.read()
        self.call('WebDriver:NewSession', {})

    def read(self):
        while b':' not in self.buf:
            self.buf += self.sock.recv(65536)
        size, _, rest = self.buf.partition(b':')
        size = int(size)
        while len(rest) < size:
            rest += self.sock.recv(65536)
        self.buf = rest[size:]
        return json.loads(rest[:size])

    def call(self, name, params):
        self.id += 1
        body = json.dumps([0, self.id, name, params]).encode()
        self.sock.sendall(str(len(body)).encode() + b':' + body)
        _, _, error, result = self.read()
        if error:
            raise SystemExit(f'{name}: {error}')
        return result


profile = tempfile.mkdtemp()
pathlib.Path(profile, 'user.js').write_text(f'user_pref("marionette.port", {PORT});\n')
firefox = subprocess.Popen(['firefox', '--headless', '--no-remote', '--marionette', '--profile', profile],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    m = Marionette(PORT)
    m.call('WebDriver:Navigate', {'url': PAGE.as_uri()})
    time.sleep(1)
    width, height = PW // CELL * CELL, PH // CELL * CELL
    levels = m.call('WebDriver:ExecuteScript', {
        'script': f'return window.constellation.frame({T}, {width}, {height}, {CELL}, {ZOOM}, true);', 'args': []})['value']
finally:
    firefox.terminate()

x0, y0 = PX + (PW - width) / 2, PY + (PH - height) / 2
rows = {}
for level, cells in enumerate(levels):
    for i in range(0, len(cells), 2):
        rows.setdefault((cells[i + 1], level), []).append(cells[i])
body = []
for (r, level), cs in sorted(rows.items()):
    xs = ' '.join(f'{x0 + (c + 0.5) * CELL:g}' for c in sorted(cs))
    y = y0 + (r + 0.5) * CELL + 3
    body.append(f'<text x="{xs}" y="{y:g}" opacity="{0.35 + 0.65 * level / 11:.2f}">{GLYPHS[level] * len(cs)}</text>')

svg = f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-labelledby="title desc"><title id="title">Hutch — Every coding agent gets its own computer.</title><desc id="desc">Hutch gives every coding agent its own box on a Linux machine you rent or own. A monochrome glyph constellation of connected microVMs.</desc><rect width="{W}" height="{H}" rx="12" fill="#fff"/><rect x="{PX}" y="{PY}" width="{PW}" height="{PH}" rx="10" fill="#111"/><g fill="#111" font-family="monospace"><text x="56" y="70" font-size="28" letter-spacing="-2">hutch ↗</text><text x="56" y="250" font-size="40" letter-spacing="-2">Every coding agent</text><text x="56" y="302" font-size="40" letter-spacing="-2">gets its own computer.</text><text x="56" y="376" font-size="17" fill="#555">Cloned boxes. Persistent terminals. Idle boxes sleep.</text><text x="56" y="498" font-size="14" fill="#555">OPEN SOURCE / APACHE-2.0 / FIRECRACKER + GVISOR</text></g><g fill="#fff" font-family="monospace" font-size="8" text-anchor="middle">{"".join(body)}</g></svg>
'''
pathlib.Path(OUT).write_text(svg)
print(f'{OUT}: {len(svg) // 1024} KB, {len(body)} text runs')
