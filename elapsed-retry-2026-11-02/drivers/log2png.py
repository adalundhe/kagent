"""Render a plain-text log as a terminal-styled PNG with headless Chrome.

Usage: python3 -I log2png.py <log> <png> [width]
"""
import html
import subprocess
import sys
import tempfile
from pathlib import Path

log, png = Path(sys.argv[1]), Path(sys.argv[2])
width = int(sys.argv[3]) if len(sys.argv) > 3 else 1900
lines = log.read_text().splitlines()


def style(line):
    escaped = html.escape(line)
    if line.startswith("$ "):
        return f'<span class="cmd">{escaped}</span>'
    if line.startswith("#"):
        return f'<span class="note">{escaped}</span>'
    if "grpc=14" in line or " ERROR " in line or "WARN" in line or "FAIL" in line or "refused" in line or "WORKING" in line:
        return f'<span class="bad">{escaped}</span>'
    if "grpc=0" in line or "COMPLETED" in line or "SUCCEEDED" in line or "PASS" in line or " OK " in line:
        return f'<span class="good">{escaped}</span>'
    return escaped


body = "\n".join(style(line) for line in lines)
page = f"""<!doctype html><meta charset="utf-8"><style>
body {{ margin: 0; background: #11151c; }}
pre {{ margin: 0; padding: 18px 22px; color: #d6dde6; font: 13px/1.45 Menlo, monospace; white-space: pre-wrap; word-break: break-all; width: {width - 44}px; }}
.cmd {{ color: #8ab4ff; font-weight: bold; }} .note {{ color: #8b949e; }} .bad {{ color: #ff8f8f; }} .good {{ color: #7ee2a8; }}
</style><pre>{body}</pre>"""
height = 40 + int(len(lines) * 13 * 1.45) + sum(len(line) // int((width - 44) / 7.8) for line in lines) * 19
with tempfile.TemporaryDirectory() as tmp:
    page_path = Path(tmp) / "log.html"
    page_path.write_text(page)
    subprocess.run([
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "--headless=new", "--disable-gpu", "--hide-scrollbars",
        f"--window-size={width},{height}", f"--screenshot={png}", page_path.as_uri(),
    ], check=True, capture_output=True)
print(png)
