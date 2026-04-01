#!/usr/bin/env python3
"""
AWS SSO auth server for aws-tunnels.

GET  /           - status page: shows credential age, tunnel health, login button
POST /login      - kicks off `aws sso login --no-browser`, streams the device-auth
                                     URL + user code in the page so you can open it on any device,
                                     then writes .last-login on the shared volume when done
POST /restart    - touches .last-login to signal all tunnel pods to restart
                                     (tunnels watch this file's mtime via their health loop)

Credential files live on a shared NFS PVC mounted at /aws-creds:
    /aws-creds/sso/cache/          - written by `aws sso login`
    /aws-creds/.last-login         - mtime signals tunnels to reconnect
"""

import http.server
import json
import os
import subprocess
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
import re
import html

CREDS_DIR = Path("/aws-creds")
SSO_CACHE = CREDS_DIR / "sso" / "cache"
LAST_LOGIN = CREDS_DIR / ".last-login"
STATUS_DIR = CREDS_DIR / "tunnel-status"
AWS_PROFILE = os.environ.get("AWS_PROFILE", "awsprofile001")
PORT = int(os.environ.get("PORT", "8090"))

_login_lock = threading.Lock()
_login_output = []
_login_done = False
_login_ok = False


def _credential_age_str():
    try:
        mtime = LAST_LOGIN.stat().st_mtime
        age_s = int(time.time() - mtime)
        h, rem = divmod(age_s, 3600)
        m, s = divmod(rem, 60)
        ts = datetime.fromtimestamp(mtime, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
        return f"{h}h {m}m {s}s ago ({ts})"
    except FileNotFoundError:
        return "never"


def _token_files():
    try:
        return sorted(SSO_CACHE.glob("*.json"), key=lambda p: p.stat().st_mtime, reverse=True)
    except Exception:
        return []


def _touch_last_login():
    CREDS_DIR.mkdir(parents=True, exist_ok=True)
    LAST_LOGIN.touch()
    now = time.time()
    os.utime(LAST_LOGIN, (now, now))


def _tunnel_status_rows():
    STATUS_DIR.mkdir(parents=True, exist_ok=True)
    rows = []
    for state_file in sorted(STATUS_DIR.glob("*.state")):
        try:
            name = state_file.stem
            state = state_file.read_text().strip() or "unknown"
            error_file = STATUS_DIR / f"{name}.error"
            detail = error_file.read_text().strip() if error_file.exists() else ""
            mtime = datetime.fromtimestamp(state_file.stat().st_mtime, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
            css = "ok" if state == "running" else ("warn" if state in ["auth_required", "reconnecting", "starting"] else "err")
            rows.append((name, state, detail, mtime, css))
        except FileNotFoundError:
            # Tunnel sidecar may rotate status files while we render; skip this row.
            continue
    return rows


def _run_login():
    global _login_output, _login_done, _login_ok
    env = os.environ.copy()
    env["HOME"] = "/root"
    env["AWS_CONFIG_FILE"] = "/root/.aws/config"
    cmd = ["aws", "sso", "login", "--profile", AWS_PROFILE, "--no-browser"]
    try:
        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            env=env,
            bufsize=1,
        )
        for line in proc.stdout:
            _login_output.append(line.rstrip())
        proc.wait()
        if proc.returncode == 0:
            _touch_last_login()
            _login_ok = True
            _login_output.append("✅ Login successful — tunnels will reconnect automatically.")
        else:
            _login_ok = False
            _login_output.append(f"❌ aws sso login exited with code {proc.returncode}")
    except Exception as exc:
        _login_ok = False
        _login_output.append(f"❌ Exception: {exc}")
    finally:
        _login_done = True


CSS = """
body{font-family:system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 20px;background:#0d1117;color:#e6edf3}
h1{color:#58a6ff}h2{color:#8b949e;font-size:1rem;margin-top:2rem}
.card{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px 20px;margin:12px 0}
.ok{color:#3fb950}.warn{color:#d29922}.err{color:#f85149}
button{cursor:pointer;padding:8px 18px;border-radius:6px;border:none;font-size:.95rem;font-weight:600}
.btn-login{background:#238636;color:#fff}.btn-login:hover{background:#2ea043}
.btn-restart{background:#1f6feb;color:#fff}.btn-restart:hover{background:#388bfd}
pre{background:#0d1117;padding:12px;border-radius:6px;overflow-x:auto;font-size:.85rem;white-space:pre-wrap}
form{display:inline}
"""


def _page(body: str) -> str:
    return f"""<!DOCTYPE html>
<html lang=\"en\"><head><meta charset=\"utf-8\"> 
<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"> 
<title>AWS Tunnel Auth</title>
<style>{CSS}</style></head>
<body>{body}</body></html>"""


def _status_page():
    age = _credential_age_str()
    tokens = _token_files()
    tunnel_rows = _tunnel_status_rows()
    token_info = f"{len(tokens)} token file(s) in SSO cache" if tokens else "no token files found"
    age_class = "ok" if tokens and LAST_LOGIN.exists() and (time.time() - LAST_LOGIN.stat().st_mtime) < 43200 else "warn"

    if tunnel_rows:
        rows_html = "".join(
            [
                f"<tr><td>{name}</td><td class='{css}'>{state}</td><td>{detail or '-'}</td><td>{mtime}</td></tr>"
                for name, state, detail, mtime, css in tunnel_rows
            ]
        )
        tunnels_html = f"""
    <div class=\"card\"> 
        <h2>TUNNEL STATUS</h2>
        <table style=\"width:100%;border-collapse:collapse;font-size:.9rem\">
            <thead>
                <tr style=\"text-align:left;color:#8b949e\"><th>tunnel</th><th>state</th><th>detail</th><th>updated</th></tr>
            </thead>
            <tbody>{rows_html}</tbody>
        </table>
        <p style=\"color:#8b949e;font-size:.85rem;margin-top:8px\">
            State <code>auth_required</code> means the tunnel is paused waiting for new credentials.
            It will not keep retrying until a new login signal is written.
        </p>
    </div>
        """
    else:
        tunnels_html = """
    <div class=\"card\"> 
        <h2>TUNNEL STATUS</h2>
        <p class=\"warn\">No tunnel status files yet.</p>
    </div>
        """

    return _page(
        f"""
<h1>🔐 AWS Tunnel Auth</h1>
<div class=\"card\">
  <h2>CREDENTIAL STATUS</h2>
  <p>Last login: <span class=\"{age_class}\">{age}</span></p>
  <p>{token_info}</p>
</div>
{tunnels_html}
<div class=\"card\">
  <h2>ACTIONS</h2>
  <form method=\"POST\" action=\"/login\">
    <button class=\"btn-login\" type=\"submit\">🔑 Start new SSO login</button>
  </form>
  &nbsp;&nbsp;
  <form method=\"POST\" action=\"/restart\">
    <button class=\"btn-restart\" type=\"submit\">♻️ Restart all tunnels now</button>
  </form>
  <p style=\"color:#8b949e;font-size:.85rem;margin-top:8px\">
    \"Restart tunnels\" touches <code>.last-login</code> — all tunnel pods detect the
    mtime change and reconnect within ~30 s without needing new credentials.
    Use this if a tunnel dropped but credentials are still valid.
  </p>
</div>
"""
    )


def _status_json():
    tokens = _token_files()
    tunnel_rows = _tunnel_status_rows()
    last_login_ts = None
    if LAST_LOGIN.exists():
        last_login_ts = datetime.fromtimestamp(LAST_LOGIN.stat().st_mtime, tz=timezone.utc).isoformat()

    tunnels = []
    for name, state, detail, mtime, _css in tunnel_rows:
        tunnels.append({"name": name, "state": state, "detail": detail, "updated": mtime})

    payload = {
        "profile": AWS_PROFILE,
        "credentials": {
            "tokenFiles": len(tokens),
            "lastLoginUtc": last_login_ts,
        },
        "login": {
            "inProgress": (not _login_done and len(_login_output) > 0),
            "lastAttemptOk": _login_ok if _login_done else None,
        },
        "tunnels": tunnels,
    }
    return json.dumps(payload, indent=2)


def _login_stream_page():
    global _login_output, _login_done, _login_ok
    lines_html = "\n".join(_login_output) if _login_output else "Starting…"

    # attempt to extract device auth URL and user code from aws sso login output
    def _parse_device_auth(lines):
        url = None
        code = None
        for ln in lines:
            # find first http(s) URL
            m = re.search(r"(https?://\S+)", ln)
            if m and not url:
                url = m.group(1).rstrip('.,)')
            # find a likely user code (contains letters/numbers and often a dash)
            cm = re.search(r"([A-Z0-9-]{4,})", ln)
            if cm and not code and ("code" in ln.lower() or "user code" in ln.lower() or "verification" in ln.lower() or '-' in cm.group(1)):
                code = cm.group(1)
            if url and code:
                break
        return url, code

    auth_url, auth_code = _parse_device_auth(_login_output)
    if not _login_done:
        refresh = '<meta http-equiv="refresh" content="3">'
        status_html = '<p class="warn">⏳ Login in progress… page auto-refreshes every 3 s</p>'
    elif _login_ok:
        refresh = ""
        status_html = '<p class="ok">✅ Done — <a href="/">back to status</a></p>'
    else:
        refresh = ""
        status_html = '<p class="err">❌ Login failed — <a href="/">try again</a></p>'

    # If we detected an auth URL/code, render a button and code snippet above the raw output
    auth_html = ""
    if auth_url:
        safe_url = html.escape(auth_url)
        safe_code = html.escape(auth_code) if auth_code else ""
        auth_html = f"""
<div class=\"card\"> 
  <h2>AUTHENTICATION</h2>
  <p style=\"color:#8b949e;font-size:.85rem\">Open this URL on any device to complete SSO authentication:</p>
  <p><a class=\"btn-login\" href=\"{safe_url}\" target=\"_blank\" rel=\"noopener\">Open authentication URL</a></p>
  {f'<p style=\"margin-top:8px\">Code: <strong>{safe_code}</strong></p>' if safe_code else ''}
</div>
"""

    page = f"""
{refresh}
<h1>🔑 SSO Login</h1>
{auth_html}
<div class=\"card\">
  <h2>OUTPUT</h2>
  <p style=\"color:#8b949e;font-size:.85rem\">
    Open the URL below in any browser on any device to authenticate.
    This page refreshes automatically until login completes.
  </p>
  {status_html}
  <pre>{lines_html}</pre>
</div>
"""

    return _page(page)


class Handler(http.server.BaseHTTPRequestHandler):
    def log_request(self, code="-", size="-"):
        # Keep access logs focused on failures to reduce probe/noise churn.
        try:
            status = int(code)
        except (TypeError, ValueError):
            status = 0
        if status >= 400 or status == 0:
            print(f"[auth-server] {self.command} {self.path} -> {code} ({size})")

    def log_message(self, fmt, *args):
        # BaseHTTPRequestHandler uses this for internal diagnostics.
        print(f"[auth-server] {fmt % args}")

    def _send(self, code, body, content_type="text/html; charset=utf-8"):
        encoded = body.encode()
        try:
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", len(encoded))
            self.end_headers()
            self.wfile.write(encoded)
        except (BrokenPipeError, ConnectionResetError):
            # Common when health checks or clients close sockets before reading.
            return

    def do_GET(self):
        try:
            if self.path == "/healthz":
                self._send(200, "ok", "text/plain")
            elif self.path == "/status.json":
                self._send(200, _status_json(), "application/json; charset=utf-8")
            elif self.path == "/":
                self._send(200, _status_page())
            elif self.path == "/login":
                self._send(200, _login_stream_page())
            else:
                self._send(404, "not found", "text/plain")
        except Exception as exc:
            print(f"[auth-server] GET {self.path} failed: {exc}")
            self._send(500, "internal error", "text/plain")

    def do_POST(self):
        global _login_output, _login_done, _login_ok

        if self.path == "/login":
            with _login_lock:
                if not _login_done and _login_output:
                    self.send_response(303)
                    self.send_header("Location", "/login")
                    self.end_headers()
                    return
                _login_output = []
                _login_done = False
                _login_ok = False
            threading.Thread(target=_run_login, daemon=True).start()
            self.send_response(303)
            self.send_header("Location", "/login")
            self.end_headers()

        elif self.path == "/restart":
            _touch_last_login()
            self.send_response(303)
            self.send_header("Location", "/")
            self.end_headers()

        else:
            self._send(404, "not found", "text/plain")


if __name__ == "__main__":
    CREDS_DIR.mkdir(parents=True, exist_ok=True)
    SSO_CACHE.mkdir(parents=True, exist_ok=True)
    STATUS_DIR.mkdir(parents=True, exist_ok=True)
    print(f"[auth-server] Listening on :{PORT}")
    server = http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()
