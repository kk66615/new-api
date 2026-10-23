#!/usr/bin/env python3
"""Fork check, run before deploying a build that merged upstream changes.

    python3 bin/fork-checks/empty_stream_e2e.py new-api:<tag>

Needs root on a Docker host that has the image (the s8 build host). It starts a
throwaway new-api container from that image (own SQLite under /root/es-stage,
host network on 127.0.0.1:3901, no production database or Redis) and two fake
upstreams on 127.0.0.1:3902:
  /a/...  answers 200 text/event-stream and closes without a byte (+ X-Codex-Turn-State)
  /b/...  answers normally (chat stream, chat JSON, Responses stream)
and checks that an empty upstream stream is retried on another channel, is
charged exactly once (only on the channel that answered), never leaks the
failed upstream's headers, and is not charged at all when every channel is
empty, with one error log and one empty_stream evidence row per empty attempt.
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

GW_PORT, UP_PORT = 3901, 3902
GW = f"http://127.0.0.1:{GW_PORT}"
DATA = "/root/es-stage"
CONTAINER = "es-stage"
MODE = {"a": "empty", "b": "ok"}
HITS = []
RESULTS = []


def check(name, ok, detail=""):
    RESULTS.append((name, bool(ok)))
    print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""), flush=True)


class Upstream(http.server.BaseHTTPRequestHandler):  # HTTP/1.0: the body ends when the socket closes
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        ch = self.path.split("/")[1]
        api = self.path[len(ch) + 1:]
        stream = bool(body.get("stream"))
        HITS.append((ch, api, stream))
        if MODE[ch] == "empty":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("X-Codex-Turn-State", "stale-" + ch)
            self.send_header("X-Request-Id", f"up-{ch}-{len(HITS)}")
            self.end_headers()
            return
        if api.endswith("/chat/completions") and not stream:
            data = json.dumps({"id": "chatcmpl-b", "object": "chat.completion", "created": 1, "model": "es-model",
                               "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello from B"}, "finish_reason": "stop"}],
                               "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if api.endswith("/chat/completions"):
            events = [
                {"id": "chatcmpl-b", "object": "chat.completion.chunk", "created": 1, "model": "es-model", "choices": [{"index": 0, "delta": {"content": "hello from B"}}]},
                {"id": "chatcmpl-b", "object": "chat.completion.chunk", "created": 1, "model": "es-model", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                 "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}},
            ]
            lines = ["data: " + json.dumps(e) for e in events] + ["data: [DONE]"]
        else:
            resp = {"id": "resp_b", "object": "response", "created_at": 1, "model": "es-model", "status": "in_progress", "output": []}
            done = dict(resp, status="completed",
                        output=[{"type": "message", "id": "msg_b", "role": "assistant", "status": "completed",
                                 "content": [{"type": "output_text", "text": "hello from B", "annotations": []}]}],
                        usage={"input_tokens": 10, "output_tokens": 5, "total_tokens": 15})
            events = [{"type": "response.created", "response": resp},
                      {"type": "response.output_text.delta", "item_id": "msg_b", "output_index": 0, "content_index": 0, "delta": "hello from B"},
                      {"type": "response.completed", "response": done}]
            lines = [f"event: {e['type']}\ndata: {json.dumps(e)}" for e in events]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for line in lines:
            self.wfile.write((line + "\n\n").encode())
            self.wfile.flush()


opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def call(method, path, payload=None, headers=None, raw=False):
    req = urllib.request.Request(GW + path, method=method, data=None if payload is None else json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json", **(headers or {})})
    try:
        r = opener.open(req, timeout=60)
        status, hdrs, body = r.status, r.headers, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status, hdrs, body = e.code, e.headers, e.read().decode("utf-8", "replace")
    if raw:
        return status, hdrs, body
    return json.loads(body)


def query(sql, *args):
    con = sqlite3.connect(f"file:{DATA}/es.db?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute(sql, args)]
    finally:
        con.close()


def port_in_use(port):
    with socket.socket() as s:
        return s.connect_ex(("127.0.0.1", port)) == 0


def setup_gateway(image):
    os.makedirs(DATA)
    os.chmod(DATA, 0o700)
    subprocess.run(["docker", "run", "-d", "--name", CONTAINER, "--network", "host",
                    "-e", f"PORT={GW_PORT}", "-e", "SQLITE_PATH=/data/es.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)",
                    "-e", "ERROR_LOG_ENABLED=true", "-e", "SESSION_SECRET=" + secrets.token_hex(16), "-e", "TZ=Asia/Shanghai",
                    "-v", f"{DATA}:/data", image], check=True, capture_output=True)
    for _ in range(60):
        try:
            if call("GET", "/api/status").get("success"):
                break
        except Exception:
            pass
        time.sleep(1)
    password = "Es-" + secrets.token_hex(8) + "-Aa1"
    r = call("POST", "/api/setup", {"username": "esroot", "password": password, "confirmPassword": password, "SelfUseModeEnabled": True, "DemoSiteEnabled": False})
    check("setup", r.get("success"), r.get("message", ""))
    r = call("POST", "/api/user/login", {"username": "esroot", "password": password})
    check("login", r.get("success"), r.get("message", ""))
    admin = {"New-Api-User": "1", "Authorization": "Bearer " + r["data"]["access_token"]}
    r = call("PUT", "/api/option/", {"key": "RetryTimes", "value": "2"}, admin)
    check("option RetryTimes=2", r.get("success"), r.get("message", ""))
    for name, path, priority in (("A-empty", "a", 10), ("B-ok", "b", 0)):
        r = call("POST", "/api/channel/", {"mode": "single", "channel": {"type": 1, "name": name, "key": "sk-fake-" + path,
                                                                         "base_url": f"http://127.0.0.1:{UP_PORT}/{path}", "models": "es-model",
                                                                         "group": "default", "priority": priority, "weight": 1, "status": 1}}, admin)
        check("add channel " + name, r.get("success"), r.get("message", ""))
    r = call("POST", "/api/token/", {"name": "es", "remain_quota": 0, "unlimited_quota": True, "expired_time": -1}, admin)
    check("add token", r.get("success"), r.get("message", ""))
    items = call("GET", "/api/token/?p=1&size=10", None, admin)["data"]["items"]
    key = call("POST", f"/api/token/{items[0]['id']}/key", None, admin)["data"]["key"]
    return {"Authorization": "Bearer " + (key if key.startswith("sk-") else "sk-" + key)}


def run_scenarios(auth):
    channels = {r["name"]: r["id"] for r in query("select id, name from channels")}
    chat = {"model": "es-model", "messages": [{"role": "user", "content": "hi"}]}
    scenarios = [
        ("chat stream: A empty -> B", "/v1/chat/completions", dict(chat, stream=True), "ok"),
        ("responses stream: A empty -> B", "/v1/responses", {"model": "es-model", "stream": True, "input": "hi"}, "ok"),
        ("non-stream client, A answers empty SSE -> B JSON", "/v1/chat/completions", dict(chat, stream=False), "ok"),
        ("all channels empty -> 502, no charge", "/v1/chat/completions", dict(chat, stream=True), "empty"),
    ]
    for name, path, payload, b_mode in scenarios:
        MODE["b"] = b_mode
        HITS.clear()
        before = query("select quota from users where id=1")[0]["quota"]
        status, hdrs, body = call("POST", path, payload, auth, raw=True)
        request_id = hdrs.get("X-Oneapi-Request-Id", "")
        time.sleep(6)  # consume/error logs and the evidence writer flush asynchronously
        after = query("select quota from users where id=1")[0]["quota"]
        logs = query("select type, channel_id, quota, content from logs where request_id=? order by id", request_id)
        evidence = query("select channel_id, note, response_headers from upstream_evidences where request_id=? order by id", request_id)
        consume = [l for l in logs if l["type"] == 2]
        errors = [l for l in logs if l["type"] == 5]
        print(f"\n== {name}: status={status} hits={HITS} quota_delta={before - after}", flush=True)
        print("   logs:", [(l["type"], l["channel_id"], l["quota"], (l["content"] or "")[:50]) for l in logs])
        print("   evidence:", [(e["channel_id"], e["note"]) for e in evidence])
        a_errors = [l for l in errors if l["channel_id"] == channels["A-empty"]]
        a_evidence = [e for e in evidence if e["channel_id"] == channels["A-empty"] and "empty_stream" in (e["note"] or "")]
        check(name + ": upstream A tried first", HITS and HITS[0][0] == "a")
        check(name + ": empty attempt on A logged as error", len(a_errors) == 1)
        check(name + ": evidence row for A with its headers", len(a_evidence) == 1 and "up-a-" in (a_evidence[0]["response_headers"] or ""))
        check(name + ": stale Codex turn header not sent", "stale-a" not in str(hdrs.get_all("X-Codex-Turn-State") or ""))
        if b_mode == "ok":
            check(name + ": client got B's answer", status == 200 and "hello from B" in body, f"status={status}")
            check(name + ": B hit with the client's stream mode", len(HITS) >= 2 and HITS[1][0] == "b" and HITS[1][2] == payload["stream"])
            check(name + ": exactly one consume log, on B", len(consume) == 1 and consume[0]["channel_id"] == channels["B-ok"])
            check(name + ": wallet charged once (= consume log)", consume and before - after == consume[0]["quota"] and consume[0]["quota"] > 0,
                  f"delta={before - after} log={consume[0]['quota'] if consume else None}")
        else:
            check(name + ": client got 502 empty_response", status == 502 and "empty_response" in body, f"status={status}")
            check(name + ": no consume log", not consume)
            check(name + ": wallet unchanged", before == after, f"delta={before - after}")
            check(name + ": every attempt logged as error", len(errors) == len(HITS), f"errors={len(errors)} hits={len(HITS)}")


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
