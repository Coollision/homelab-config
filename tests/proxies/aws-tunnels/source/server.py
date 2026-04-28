#!/usr/bin/env python3
"""
AWS SSO auth server for aws-tunnels.

GET  /           - status page: shows credential age, tunnel health, login button
POST /login      - kicks off `aws sso login --no-browser`, streams the device-auth
                                     URL + user code in the page so you can open it on any device,
                                     then writes a profile-scoped login signal file when done
POST /restart    - touches .last-login to signal all tunnel pods to restart
                                     (tunnels watch this file's mtime via their health loop)

Credential files live on a shared NFS PVC mounted at /aws-creds:
    /aws-creds/sso/cache/          - written by `aws sso login`
    /aws-creds/.last-login-<profile> - mtime signals matching profile tunnels to reconnect
    /aws-creds/.last-login         - global restart signal for all tunnels
"""

import http.server
import json
import os
import subprocess
import threading
import time
from urllib.parse import parse_qs
from datetime import datetime, timezone
from pathlib import Path
import re
import html

CREDS_DIR = Path("/aws-creds")
SSO_CACHE = CREDS_DIR / "sso" / "cache"
STATUS_DIR = CREDS_DIR / "tunnel-status"
AWS_DEFAULT_PROFILE = os.environ.get("AWS_DEFAULT_PROFILE", "awsprofile001")
AUTH_PROFILES_RAW = os.environ.get("AWS_AUTH_PROFILES", AWS_DEFAULT_PROFILE)
AUTH_PROFILES = [profile.strip() for profile in AUTH_PROFILES_RAW.split(",") if profile.strip()]
if AWS_DEFAULT_PROFILE not in AUTH_PROFILES:
    AUTH_PROFILES.insert(0, AWS_DEFAULT_PROFILE)
PORT = int(os.environ.get("PORT", "8090"))

_login_lock = threading.Lock()
_login_output = []
_login_done = False
_login_ok = False
_login_profile = AWS_DEFAULT_PROFILE


def _profile_key(profile: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "_", profile)


def _login_signal_file(profile: str) -> Path:
    return CREDS_DIR / f".last-login-{_profile_key(profile)}"


def _global_login_signal_file() -> Path:
    return CREDS_DIR / ".last-login"


def _credential_age_str(profile: str):
    signal_file = _login_signal_file(profile)
    try:
        mtime = signal_file.stat().st_mtime
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


def _touch_login_signal(profile: str):
    CREDS_DIR.mkdir(parents=True, exist_ok=True)
    signal_file = _login_signal_file(profile)
    signal_file.touch()
    now = time.time()
    os.utime(signal_file, (now, now))


def _touch_global_restart_signal():
    CREDS_DIR.mkdir(parents=True, exist_ok=True)
    signal_file = _global_login_signal_file()
    signal_file.touch()
    now = time.time()
    os.utime(signal_file, (now, now))


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


def _run_login(profile: str):
    global _login_output, _login_done, _login_ok
    env = os.environ.copy()
    env["HOME"] = "/root"
    env["AWS_CONFIG_FILE"] = "/root/.aws/config"
    cmd = ["aws", "sso", "login", "--profile", profile, "--no-browser"]
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
            _touch_login_signal(profile)
            _login_ok = True
            _login_output.append(f"✅ Login successful for profile '{profile}' — matching tunnels will reconnect automatically.")
        else:
            _login_ok = False
            _login_output.append(f"❌ aws sso login for profile '{profile}' exited with code {proc.returncode}")
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
button,.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;cursor:pointer;padding:10px 16px;border-radius:8px;border:1px solid transparent;font-size:.92rem;font-weight:700;text-decoration:none;transition:background-color .15s ease,border-color .15s ease,transform .15s ease,box-shadow .15s ease}
button:hover,.btn:hover{transform:translateY(-1px)}
button:active,.btn:active{transform:translateY(0)}
.btn-login{background:#238636;color:#fff;box-shadow:0 1px 0 rgba(255,255,255,.08) inset}
.btn-login:hover{background:#2ea043}
.btn-direct{background:#1f6feb;color:#fff;box-shadow:0 1px 0 rgba(255,255,255,.08) inset}
.btn-direct:hover{background:#388bfd}
.btn-restart{background:#21262d;border-color:#30363d;color:#e6edf3}
.btn-restart:hover{background:#30363d}
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
    age = _credential_age_str(AWS_DEFAULT_PROFILE)
    tokens = _token_files()
    tunnel_rows = _tunnel_status_rows()
    token_info = f"{len(tokens)} token file(s) in SSO cache" if tokens else "no token files found"
    default_signal_file = _login_signal_file(AWS_DEFAULT_PROFILE)
    age_class = "ok" if tokens and default_signal_file.exists() and (time.time() - default_signal_file.stat().st_mtime) < 43200 else "warn"

    profile_rows = []
    for profile in AUTH_PROFILES:
        signal_file = _login_signal_file(profile)
        if signal_file.exists():
            age_value = _credential_age_str(profile)
            profile_age_s = time.time() - signal_file.stat().st_mtime
            status_class = "ok" if profile_age_s < 43200 else "warn"
            status_value = "valid-ish" if profile_age_s < 43200 else "stale"
        else:
            age_value = "never"
            status_class = "warn"
            status_value = "missing"
        profile_rows.append((profile, age_value, status_class, status_value))

    profile_rows_html = "".join(
        [
            f"<tr><td>{html.escape(profile)}</td><td>{html.escape(age_value)}</td><td class='{status_class}'>{status_value}</td></tr>"
            for profile, age_value, status_class, status_value in profile_rows
        ]
    )

    profile_status_html = f"""
<div class=\"card\"> 
    <h2>PROFILE STATUS</h2>
    <table style=\"width:100%;border-collapse:collapse;font-size:.9rem\">
        <thead>
            <tr style=\"text-align:left;color:#8b949e\"><th>profile</th><th>last login</th><th>status</th></tr>
        </thead>
        <tbody>{profile_rows_html}</tbody>
    </table>
    <p style=\"color:#8b949e;font-size:.85rem;margin-top:8px\">
        Each login button only refreshes credentials for that matching profile.
    </p>
</div>
    """

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

    if len(AUTH_PROFILES) == 1:
        login_actions_html = f"""
    <form method=\"POST\" action=\"/login\">
        <button class=\"btn-login\" type=\"submit\">🔑 Start new SSO login ({html.escape(AUTH_PROFILES[0])})</button>
    </form>
        """
    else:
        login_actions_html = "".join(
            [
                f"""
    <form method=\"POST\" action=\"/login\">
        <input type=\"hidden\" name=\"profile\" value=\"{html.escape(profile)}\" />
        <button class=\"btn-login\" type=\"submit\">🔑 Login {html.escape(profile)}</button>
    </form>
                """
                for profile in AUTH_PROFILES
            ]
        )

    return _page(
        f"""
<h1>🔐 AWS Tunnel Auth</h1>
<div class=\"card\">
  <h2>CREDENTIAL STATUS</h2>
    <p>Default profile: <strong>{html.escape(AWS_DEFAULT_PROFILE)}</strong></p>
    <p>Available profiles: <strong>{', '.join(html.escape(profile) for profile in AUTH_PROFILES)}</strong></p>
    <p>Default profile last login: <span class=\"{age_class}\">{age}</span></p>
  <p>{token_info}</p>
</div>
{profile_status_html}
{tunnels_html}
<div class=\"card\">
  <h2>ACTIONS</h2>
    {login_actions_html}
  &nbsp;&nbsp;
  <form method=\"POST\" action=\"/restart\">
        <button class=\"btn-restart\" type=\"submit\">♻️ Restart all tunnels (global)</button>
  </form>
  <p style=\"color:#8b949e;font-size:.85rem;margin-top:8px\">
        Login writes a profile-scoped restart signal so only matching profile tunnels reconnect.
        \"Restart all tunnels\" touches <code>.last-login</code> and forces every tunnel to reconnect.
  </p>
</div>
"""
    )


def _status_json():
    tokens = _token_files()
    tunnel_rows = _tunnel_status_rows()
    profile_last_login = {}
    for profile in AUTH_PROFILES:
        signal_file = _login_signal_file(profile)
        if signal_file.exists():
            profile_last_login[profile] = datetime.fromtimestamp(signal_file.stat().st_mtime, tz=timezone.utc).isoformat()
        else:
            profile_last_login[profile] = None

    tunnels = []
    for name, state, detail, mtime, _css in tunnel_rows:
        tunnels.append({"name": name, "state": state, "detail": detail, "updated": mtime})

    payload = {
        "defaultProfile": AWS_DEFAULT_PROFILE,
        "profiles": AUTH_PROFILES,
        "credentials": {
            "tokenFiles": len(tokens),
            "lastLoginUtcByProfile": profile_last_login,
        },
        "login": {
            "profile": _login_profile,
            "inProgress": (not _login_done and len(_login_output) > 0),
            "lastAttemptOk": _login_ok if _login_done else None,
        },
        "tunnels": tunnels,
    }
    return json.dumps(payload, indent=2)


def _login_stream_page():
    global _login_output, _login_done, _login_ok, _login_profile
    lines_html = "\n".join(_login_output) if _login_output else "Starting…"

    # attempt to extract device auth URL and user code from aws sso login output
    def _parse_device_auth(lines):
        urls = []
        code = None
        for ln in lines:
            # collect all http(s) URLs we see in order
            m = re.search(r"(https?://\S+)", ln)
            if m:
                url = m.group(1).rstrip('.,)')
                if url not in urls:
                    urls.append(url)
            # find a likely user code (contains letters/numbers and often a dash)
            cm = re.search(r"([A-Z0-9-]{4,})", ln)
            if cm and not code and ("code" in ln.lower() or "user code" in ln.lower() or "verification" in ln.lower() or '-' in cm.group(1)):
                code = cm.group(1)
        auth_url = urls[0] if urls else None
        # Prefer any URL that already includes user_code=, otherwise fall back to second URL.
        direct_url = next((u for u in urls if "user_code=" in u), None)
        if not direct_url and len(urls) > 1:
            direct_url = urls[1]
        return auth_url, direct_url, code

    auth_url, direct_auth_url, auth_code = _parse_device_auth(_login_output)
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
        safe_direct_url = html.escape(direct_auth_url) if direct_auth_url else ""
        safe_code = html.escape(auth_code) if auth_code else ""
        direct_label = f"Direct login ({safe_code})" if safe_code else "Direct login with code"
        auth_html = f"""
<div class=\"card\"> 
  <h2>AUTHENTICATION</h2>
  <p style=\"color:#8b949e;font-size:.85rem\">Open this URL on any device to complete SSO authentication:</p>
    <p style=\"display:flex;flex-wrap:wrap;gap:8px\">
        <a class=\"btn btn-login\" href=\"{safe_url}\" target=\"_blank\" rel=\"noopener\">Open authentication URL</a>
        {f'<a class=\"btn btn-direct\" href=\"{safe_direct_url}\" target=\"_blank\" rel=\"noopener\">{direct_label}</a>' if safe_direct_url else ''}
    </p>
  {f'<p style=\"margin-top:8px\">Code: <strong>{safe_code}</strong></p>' if safe_code else ''}
</div>
"""

    page = f"""
{refresh}
<h1>🔑 SSO Login</h1>
<p style="color:#8b949e">Profile: <strong>{html.escape(_login_profile)}</strong></p>
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
        global _login_output, _login_done, _login_ok, _login_profile

        if self.path == "/login":
            raw_length = self.headers.get("Content-Length", "0")
            try:
                content_length = int(raw_length)
            except ValueError:
                content_length = 0
            body_raw = self.rfile.read(content_length).decode("utf-8", errors="ignore") if content_length > 0 else ""
            body = parse_qs(body_raw)
            requested_profile = (body.get("profile") or [AWS_DEFAULT_PROFILE])[0]
            selected_profile = requested_profile if requested_profile in AUTH_PROFILES else AWS_DEFAULT_PROFILE

            with _login_lock:
                if not _login_done and _login_output:
                    self.send_response(303)
                    self.send_header("Location", "/login")
                    self.end_headers()
                    return
                _login_output = []
                _login_done = False
                _login_ok = False
                _login_profile = selected_profile
            threading.Thread(target=_run_login, args=(selected_profile,), daemon=True).start()
            self.send_response(303)
            self.send_header("Location", "/login")
            self.end_headers()

        elif self.path == "/restart":
            _touch_global_restart_signal()
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
