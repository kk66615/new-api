#!/usr/bin/env python3
"""Fork check for the channel rate and quota limits, run before deploying.

    python3 bin/fork-checks/channel_limits_e2e.py new-api:<tag>

Needs root on a Docker host that has the image (the s8 build host). It starts a
throwaway new-api container from that image (own SQLite under /root/cl-stage,
host network on 127.0.0.1:3911, memory cache on as in production (synced
every 2 s, because creating a channel does not refresh the cache), no
production database or Redis) and a fake OpenAI upstream on 127.0.0.1:3912
that answers every chat request with 30 prompt + 30 completion tokens and
records which channel path it was called on. It checks that:
  - a channel at its RPM limit hands requests to the next channel, and is not
    disabled for it;
  - when every candidate is at its limit the client gets 429
    channel_limit_exceeded and no upstream is called;
  - the rolling minute does not reset at a calendar-minute boundary;
  - a channel past its TPM limit is skipped;
  - the daily budget counts list price (without the group ratio): at group
    ratio 4 a 2500-quota budget still admits three requests of 1000 list
    quota each, and only the fourth moves on; the spent budget survives a
    container restart;
  - the quota reset time zone and time are validated and saved;
  - a token over its RPM limit gets 429 before any channel is used;
  - the usage the channel list shows (GET /api/channel/limit_usage) is the
    limiter's own count and needs an admin login.
Everything it creates is removed at the end. Exit code 0 only if all checks pass.
"""
import http.cookiejar
import http.server
import json
import os
import secrets
import socket
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

GW_PORT, UP_PORT = 3911, 3912
GW = f"http://127.0.0.1:{GW_PORT}"
DATA = "/root/cl-stage"
CONTAINER = "cl-stage"
HITS = []
HITS_LOCK = threading.Lock()
RESULTS = []


def check(name, ok, detail=""):
    RESULTS.append((name, bool(ok)))
    print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""), flush=True)


class Upstream(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        with HITS_LOCK:
            HITS.append(self.path.split("/")[1])
        answer = {
            "id": "chatcmpl-cl", "object": "chat.completion", "created": int(time.time()), "model": body.get("model"),
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 30, "completion_tokens": 30, "total_tokens": 60},
        }
        data = json.dumps(answer).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def call(method, path, payload=None, headers=None, raw=False):
    req = urllib.request.Request(GW + path, method=method, data=None if payload is None else json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json", **(headers or {})})
    try:
        r = opener.open(req, timeout=60)
        status, body = r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status, body = e.code, e.read().decode("utf-8", "replace")
    if raw:
        return status, body
    return json.loads(body)


def query(sql, *args):
    con = sqlite3.connect(f"file:{DATA}/cl.db?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute(sql, args)]
    finally:
        con.close()


def port_in_use(port):
    with socket.socket() as s:
        return s.connect_ex(("127.0.0.1", port)) == 0


def take_hits():
    with HITS_LOCK:
        hits = list(HITS)
        HITS.clear()
    return hits


def wait_ready():
    for _ in range(60):
        try:
            if call("GET", "/api/status").get("success"):
                return
        except Exception:
            pass
        time.sleep(1)


def setup_gateway(image):
    os.makedirs(DATA)
    os.chmod(DATA, 0o700)
    subprocess.run(["docker", "run", "-d", "--name", CONTAINER, "--network", "host",
                    "-e", f"PORT={GW_PORT}", "-e", "SQLITE_PATH=/data/cl.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)",
                    # New channels reach the memory cache only on the periodic sync
                    # (channel creation does not refresh it), so sync every 2 s.
                    "-e", "MEMORY_CACHE_ENABLED=true", "-e", "SYNC_FREQUENCY=2", "-e", "SESSION_SECRET=" + secrets.token_hex(16), "-e", "TZ=Asia/Shanghai",
                    "-v", f"{DATA}:/data", image], check=True, capture_output=True)
    wait_ready()
    password = "Cl-" + secrets.token_hex(8) + "-Aa1"
    r = call("POST", "/api/setup", {"username": "clroot", "password": password, "confirmPassword": password, "SelfUseModeEnabled": True, "DemoSiteEnabled": False})
    check("setup", r.get("success"), r.get("message", ""))
    r = call("POST", "/api/user/login", {"username": "clroot", "password": password})
    check("login", r.get("success"), r.get("message", ""))
    admin = {"New-Api-User": "1", "Authorization": "Bearer " + r["data"]["access_token"]}
    for key, value in (("RetryTimes", "3"),
                       ("GroupRatio", json.dumps({"default": 4})),
                       # 0.002 USD per call = 1000 quota at list price.
                       ("ModelPrice", json.dumps({"cl-budget": 0.002}))):
        r = call("PUT", "/api/option/", {"key": key, "value": value}, admin)
        check(f"option {key}", r.get("success"), r.get("message", ""))
    return admin


def add_channel(admin, name, model, priority, **limits):
    channel = {"type": 1, "name": name, "key": "sk-fake-" + name, "base_url": f"http://127.0.0.1:{UP_PORT}/{name}",
               "models": model, "group": "default", "priority": priority, "weight": 1, "status": 1, **limits}
    r = call("POST", "/api/channel/", {"mode": "single", "channel": channel}, admin)
    check("add channel " + name, r.get("success"), r.get("message", ""))
    time.sleep(3)  # wait for the memory cache sync to pick the channel up
    return r


def add_token(admin, name, **fields):
    r = call("POST", "/api/token/", {"name": name, "remain_quota": 0, "unlimited_quota": True, "expired_time": -1, **fields}, admin)
    check("add token " + name, r.get("success"), r.get("message", ""))
    items = call("GET", "/api/token/?p=1&size=50", None, admin)["data"]["items"]
    token_id = next(t["id"] for t in items if t["name"] == name)
    key = call("POST", f"/api/token/{token_id}/key", None, admin)["data"]["key"]
    return {"Authorization": "Bearer " + (key if key.startswith("sk-") else "sk-" + key)}


def chat(auth, model):
    return call("POST", "/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "hi"}]}, auth, raw=True)


def chat_settled(auth, model):
    """Send one request and give the gateway time to bill it.

    Billing (TPM and the quota windows) runs after the answer has been written
    to the client, so back-to-back requests from one client may be selected
    before the previous one is counted; TPM and quota limits are best effort
    by design (RPM is checked atomically before dispatch).
    """
    result = chat(auth, model)
    time.sleep(1)
    return result


def wait_for_minute_second(target):
    """Sleep until the wall clock second-of-minute reaches target."""
    while int(time.time()) % 60 != target:
        time.sleep(0.2)


def channel_status(name):
    return query("select status from channels where name = ?", name)[0]["status"]


def run_scenarios(admin):
    auth = add_token(admin, "cl")

    # RPM: A (higher priority, 2 per minute) then B.
    add_channel(admin, "rpmA", "cl-rpm", 10, rpm_limit=2)
    add_channel(admin, "rpmB", "cl-rpm", 0)
    take_hits()
    statuses = [chat(auth, "cl-rpm")[0] for _ in range(4)]
    hits = take_hits()
    check("rpm: all four requests succeed", statuses == [200] * 4, str(statuses))
    check("rpm: first two on A, the rest on B", hits == ["rpmA", "rpmA", "rpmB", "rpmB"], str(hits))
    check("rpm: A stays enabled", channel_status("rpmA") == 1)

    # What the channel list shows is the limiter's own count.
    ids = {r["name"]: r["id"] for r in query("select id, name from channels where name in ('rpmA', 'rpmB')")}
    shown = call("GET", f"/api/channel/limit_usage?ids={ids['rpmA']},{ids['rpmB']}", None, admin).get("data") or []
    check("usage: A shows 2 of 2 requests, B (no limit) is left out",
          len(shown) == 1 and shown[0].get("channel_id") == ids["rpmA"] and shown[0].get("rpm") == {"used": 2, "limit": 2}, str(shown))
    status, _ = call("GET", f"/api/channel/limit_usage?ids={ids['rpmA']}", None, None, raw=True)
    check("usage: refused without an admin login", status in (401, 403), str(status))

    # Every candidate at its limit: local 429 and no upstream call.
    add_channel(admin, "fullC", "cl-full", 10, rpm_limit=1)
    add_channel(admin, "fullD", "cl-full", 0, rpm_limit=1)
    take_hits()
    first = [chat(auth, "cl-full")[0] for _ in range(2)]
    status, body = chat(auth, "cl-full")
    hits = take_hits()
    check("all full: two requests use the two channels", first == [200, 200] and sorted(hits) == ["fullC", "fullD"], f"{first} {hits}")
    check("all full: third request is 429 channel_limit_exceeded", status == 429 and "channel_limit_exceeded" in body, f"{status} {body[:160]}")
    check("all full: channels stay enabled", channel_status("fullC") == 1 and channel_status("fullD") == 1)

    # Rolling minute: three requests at :58, more at :01 of the next minute
    # must still be refused by a 3-per-minute channel.
    add_channel(admin, "rollE", "cl-roll", 10, rpm_limit=3)
    add_channel(admin, "rollF", "cl-roll", 0)
    wait_for_minute_second(58)
    take_hits()
    for _ in range(3):
        chat(auth, "cl-roll")
    wait_for_minute_second(1)
    for _ in range(2):
        chat(auth, "cl-roll")
    hits = take_hits()
    check("rolling minute: no reset at the minute boundary", hits == ["rollE"] * 3 + ["rollF"] * 2, str(hits))

    # TPM: each answer settles 60 tokens; a 50-token channel takes one.
    add_channel(admin, "tpmG", "cl-tpm", 10, tpm_limit=50)
    add_channel(admin, "tpmH", "cl-tpm", 0)
    take_hits()
    for _ in range(2):
        chat_settled(auth, "cl-tpm")
    hits = take_hits()
    check("tpm: the second request moves on after 60 tokens", hits == ["tpmG", "tpmH"], str(hits))

    # Daily budget at list price under group ratio 4.
    add_channel(admin, "budgetI", "cl-budget", 10, daily_quota_limit=2500)
    add_channel(admin, "budgetJ", "cl-budget", 0)
    take_hits()
    statuses = [chat_settled(auth, "cl-budget")[0] for _ in range(4)]
    hits = take_hits()
    check("budget: requests succeed", statuses == [200] * 4, str(statuses))
    check("budget: three on I at list price, the fourth on J", hits == ["budgetI"] * 3 + ["budgetJ"], str(hits))
    charged = query("select used_quota from channels where name = 'budgetI'")[0]["used_quota"]
    check("budget: used_quota still records the customer charge", charged == 3 * 4000, f"used_quota={charged}")
    budget_id = query("select id from channels where name = 'budgetI'")[0]["id"]
    shown = (call("GET", f"/api/channel/limit_usage?ids={budget_id}", None, admin).get("data") or [{}])[0].get("daily") or {}
    check("usage: the daily budget shows 3000 of 2500 at list price with its next reset",
          shown.get("used") == 3000 and shown.get("limit") == 2500 and time.time() < shown.get("resets_at", 0) <= time.time() + 90000, str(shown))

    # Without Redis the budget windows are saved, so a restart (a deploy)
    # does not hand the channel its whole daily budget again.
    subprocess.run(["docker", "restart", CONTAINER], check=True, capture_output=True)
    wait_ready()
    time.sleep(3)
    take_hits()
    status, _ = chat_settled(auth, "cl-budget")
    hits = take_hits()
    check("budget: still spent after a restart", status == 200 and hits == ["budgetJ"], f"{status} {hits}")

    # Reset time zone and time: validated and saved.
    r = call("POST", "/api/channel/", {"mode": "single", "channel": {"type": 1, "name": "badZone", "key": "sk-fake-bad", "base_url": f"http://127.0.0.1:{UP_PORT}/badZone",
                                                                     "models": "cl-zone", "group": "default", "status": 1, "quota_reset_timezone": "Mars/Olympus"}}, admin)
    check("reset: an unknown time zone is rejected", not r.get("success"), r.get("message", ""))
    r = call("POST", "/api/channel/", {"mode": "single", "channel": {"type": 1, "name": "badTime", "key": "sk-fake-bad", "base_url": f"http://127.0.0.1:{UP_PORT}/badTime",
                                                                     "models": "cl-zone", "group": "default", "status": 1, "quota_reset_time": "9:30"}}, admin)
    check("reset: a malformed time is rejected", not r.get("success"), r.get("message", ""))
    add_channel(admin, "zoneK", "cl-zone", 0, quota_reset_timezone="America/Los_Angeles", quota_reset_time="09:30")
    channel_id = query("select id from channels where name = 'zoneK'")[0]["id"]
    saved = call("GET", f"/api/channel/{channel_id}", None, admin).get("data", {})
    check("reset: zone and time are saved", saved.get("quota_reset_timezone") == "America/Los_Angeles" and saved.get("quota_reset_time") == "09:30",
          f"{saved.get('quota_reset_timezone')} {saved.get('quota_reset_time')}")
    saved["quota_reset_timezone"], saved["quota_reset_time"] = "", ""
    for read_only in ("key", "status"):  # the update API refuses status changes here
        saved.pop(read_only, None)
    r = call("PUT", "/api/channel/", saved, admin)
    cleared = call("GET", f"/api/channel/{channel_id}", None, admin).get("data", {})
    check("reset: clearing restores server time and midnight", r.get("success") and cleared.get("quota_reset_timezone") == "" and cleared.get("quota_reset_time") == "",
          f"{r.get('message', '')} {cleared.get('quota_reset_timezone')!r} {cleared.get('quota_reset_time')!r}")

    # Token RPM.
    limited = add_token(admin, "cl-token-rpm", rpm_limit=2)
    add_channel(admin, "tokL", "cl-token", 0)
    take_hits()
    statuses = [chat(limited, "cl-token")[0] for _ in range(3)]
    hits = take_hits()
    check("token rpm: third request is 429", statuses == [200, 200, 429], str(statuses))
    check("token rpm: the refused request never reaches an upstream", hits == ["tokL", "tokL"], str(hits))


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    if port_in_use(GW_PORT) or port_in_use(UP_PORT) or os.path.exists(DATA) or \
            subprocess.run(["docker", "inspect", CONTAINER], capture_output=True).returncode == 0:
        print(f"refusing to run: port {GW_PORT}/{UP_PORT} in use, {DATA} exists or container {CONTAINER} exists")
        return 2
    server = http.server.ThreadingHTTPServer(("127.0.0.1", UP_PORT), Upstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        run_scenarios(setup_gateway(sys.argv[1]))
    finally:
        server.shutdown()
        subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)
        subprocess.run(["rm", "-rf", DATA])
    failed = [n for n, ok in RESULTS if not ok]
    print(f"\nSUMMARY: {len(RESULTS) - len(failed)}/{len(RESULTS)} passed" + (f"; FAILED: {failed}" if failed else ""))
    return 1 if failed or not RESULTS else 0


if __name__ == "__main__":
    sys.exit(main())
