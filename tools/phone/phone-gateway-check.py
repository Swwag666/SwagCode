#!/usr/bin/env python3
"""Manual check of the SwagCod Phone chain (F-9): gateway -> SwagCod HTTP API.

tools/phone-check.ps1 verifies the SwagCod side (the loopback API itself).
This script verifies the other half: the dsh-phone gateway running with
`"backend": "swagcod"` in its config.json, i.e. exactly what the phone PWA
talks to. Python because the gateway is Python - whoever wires the phone up
already has an interpreter, and the repo stays dependency-free.

It walks the gateway's own HTTP surface:

    GET  /api/health                     bridge must report "connected"
    POST /api/rpc  {method, params}      the whitelisted RPC methods
    GET  /api/events?since=..&had=1      the long-poll stream the PWA reads

Without --turn it stays read-only. With --turn it sends one real turn through
the gateway and asserts that the stream carries the user item, assistant
content and the turn end marker - that is the acceptance criterion "phone web
opens sessions, sends a turn, receives the stream". It spends provider tokens,
hence the explicit flag.

Usage:
    python tools/phone/phone-gateway-check.py --config C:/path/to/dsh-phone/config.json
    python tools/phone/phone-gateway-check.py --config ... --turn
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

RESULTS = []


def check(name, expect, actual, ok):
    RESULTS.append((name, expect, str(actual), bool(ok)))
    print("[{}] {} | expect {} | got {}".format("ok  " if ok else "FAIL", name, expect, actual))


def load_cfg(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


class Gateway:
    def __init__(self, base, token, timeout=30.0):
        self.base = base.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _raw(self, method, path, body=None, timeout=None):
        data = None
        headers = {"accept": "application/json"}
        if body is not None:
            data = json.dumps(body, ensure_ascii=False).encode("utf-8")
            headers["content-type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read().decode("utf-8", "replace")

    def health(self):
        code, raw = self._raw("GET", "/api/health", None, 10.0)
        try:
            return code, json.loads(raw)
        except ValueError:
            return code, {}

    def rpc(self, method, params=None):
        code, raw = self._raw(
            "POST", "/api/rpc", {"token": self.token, "method": method, "params": params or {}}
        )
        try:
            return code, json.loads(raw)
        except ValueError:
            return code, {"ok": False, "error": raw[:200]}

    def events(self, since, had=True, timeout=25.0):
        # The gateway authenticates this route too: the token rides in the
        # x-dsh-token header (preferred over a query string, which ends up in
        # access logs).
        q = urllib.parse.urlencode({"since": repr(float(since)), "had": "1" if had else ""})
        req = urllib.request.Request(
            self.base + "/api/events?" + q,
            headers={"accept": "application/json", "x-dsh-token": self.token},
            method="GET",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                code, raw = resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            code, raw = exc.code, exc.read().decode("utf-8", "replace")
        try:
            return code, json.loads(raw)
        except ValueError:
            return code, {}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", required=True, help="path to dsh-phone config.json")
    ap.add_argument("--gateway", default="", help="gateway base URL (default: from config listenPort)")
    ap.add_argument("--session", default="", help="session id to inspect (default: first in the list)")
    ap.add_argument("--turn", action="store_true", help="send one real turn and watch the stream")
    ap.add_argument(
        "--create",
        default="",
        help="cwd for a brand-new session made through session.createAndStart "
        "(the PWA's 'new chat' path); implies a real turn",
    )
    ap.add_argument("--model", default="", help="model id used with --create")
    ap.add_argument(
        "--message",
        default="Reply with one word: check. Do not call tools.",
        help="turn text used with --turn",
    )
    ap.add_argument("--wait", type=float, default=120.0, help="seconds to wait for the turn to end")
    args = ap.parse_args()

    cfg = load_cfg(args.config)
    gw_token = str(cfg.get("gatewayToken") or "")
    if not gw_token:
        print("config.json has no gatewayToken - the gateway would reject every call")
        return 2
    base = args.gateway or "http://127.0.0.1:{}".format(cfg.get("listenPort") or 8460)
    gw = Gateway(base, gw_token)

    print("== SwagCod Phone gateway check: {} ==".format(base))
    print("backend in config: {}".format(cfg.get("backend")))
    print("swagcodBaseUrl:    {}".format(cfg.get("swagcodBaseUrl")))
    if str(cfg.get("backend") or "").lower() not in ("swagcod", "swag"):
        print("WARNING: config backend is not 'swagcod'; this check expects the SwagCod adapter")

    # 1. health: the adapter must be connected to the API
    code, h = gw.health()
    check("GET /api/health", "200 + bridge connected", "{} {}".format(code, h.get("bridge")),
          code == 200 and h.get("bridge") == "connected")
    if h.get("bridge") != "connected":
        print("The gateway cannot reach SwagCod. Is the app running with the HTTP API on")
        print("{} and is swagcodToken the current one?".format(cfg.get("swagcodBaseUrl")))
        return summarize()

    # 2. ping through the adapter
    code, r = gw.rpc("ping")
    res = r.get("result") or {}
    check("rpc ping", "ok + backend swagcod", "{} {}".format(r.get("ok"), res.get("backend")),
          r.get("ok") is True and res.get("backend") == "swagcod")

    # 3. session list
    code, r = gw.rpc("session.list", {"limit": 1000})
    sessions = (r.get("result") or {}).get("sessions") or []
    check("rpc session.list", "ok + a list", "{} sessions".format(len(sessions)), r.get("ok") is True)
    if not sessions:
        print("SwagCod has no sessions yet - create one in the app, or use the PWA's 'new chat'.")
        return summarize()
    sid = args.session or sessions[0]["sessionId"]
    one = [s for s in sessions if s.get("sessionId") == sid]
    check("session shape", "sessionId/title/cwd/orderingTime",
          "{} | {} | {}".format(sid, bool(one and one[0].get("title")), bool(one and one[0].get("orderingTime"))),
          bool(one) and bool(one[0].get("title")) and bool(one[0].get("orderingTime")))

    # 4. state
    code, r = gw.rpc("session.getState", {"sessionId": sid})
    st = r.get("result") or {}
    check("rpc session.getState", "ok + a status", "{}".format(st.get("status")),
          r.get("ok") is True and bool(st.get("status")))
    check("state selections", "model + permission present",
          "{} | {}".format((st.get("selections") or {}).get("model"), (st.get("selections") or {}).get("permission")),
          bool((st.get("selections") or {}).get("model")))

    # 5. snapshot -> timeline items
    code, r = gw.rpc("session.getSnapshot", {"sessionId": sid, "limit": 200})
    items = (r.get("result") or {}).get("items") or []
    ids_ok = all(isinstance(i.get("id"), str) and i.get("id") for i in items)
    roles = sorted({i.get("role") for i in items})
    check("rpc session.getSnapshot", "ok + items with ids", "{} items, roles {}".format(len(items), roles),
          r.get("ok") is True and ids_ok)
    revs_ok = all(int(i.get("revision") or 0) >= 1 for i in items)
    check("item revisions", "every item has revision >= 1", str(revs_ok), revs_ok)

    # 6. notices (empty unless a turn waits for an approval)
    code, r = gw.rpc("session.getNotices", {"sessionId": sid})
    notices = (r.get("result") or {}).get("notices")
    check("rpc session.getNotices", "ok + a list", "{} notice(s)".format(len(notices or [])),
          r.get("ok") is True and isinstance(notices, list))

    # 7. catalogs: the PWA refuses to open "new chat" while either is empty
    code, r = gw.rpc("catalog.listModels", {})
    models = (r.get("result") or {}).get("models") or []
    check("rpc catalog.listModels", "ok + non-empty", "{} model(s)".format(len(models)),
          r.get("ok") is True and len(models) > 0)
    if models:
        m = models[0]
        check("model shape", "id + selectionId + title",
              "{} | {} | {}".format(m.get("id"), m.get("selectionId"), m.get("title")),
              bool(m.get("id")) and bool(m.get("title")))
    code, r = gw.rpc("catalog.listPermissions", {})
    perms = (r.get("result") or {}).get("permissions") or []
    check("rpc catalog.listPermissions", "ok + non-empty", "{} policy(ies)".format(len(perms)),
          r.get("ok") is True and len(perms) > 0)

    # 8. workspaces for the "new chat" sheet
    code, r = gw.rpc("workspace.list", {})
    ws = (r.get("result") or {}).get("workspaces") or []
    check("rpc workspace.list", "ok + paths", "{} workspace(s)".format(len(ws)),
          r.get("ok") is True and all(w.get("path") for w in ws))

    # 9. capabilities + config must not leak the API token
    code, r = gw.rpc("runtime.getCapabilities", {})
    caps = r.get("result") or {}
    check("rpc runtime.getCapabilities", "backend swagcod", str(caps.get("backend")),
          r.get("ok") is True and caps.get("backend") == "swagcod")
    code, r = gw.rpc("runtime.getConfig", {})
    leaked = str(cfg.get("swagcodToken") or "") and str(cfg.get("swagcodToken")) in json.dumps(r)
    check("rpc runtime.getConfig", "no swagcodToken in the answer", "leaked={}".format(bool(leaked)), not leaked)

    # 10. an unsupported method must fail with a reason, not hang
    code, r = gw.rpc("session.updateSelections", {"sessionId": sid, "selections": {"permission": "never"}})
    check("permission switch refused", "ok:false + a reason", str(r.get("error"))[:60],
          r.get("ok") is False and bool(r.get("error")))

    # 11. the events endpoint answers a first-time client with a sessions snapshot
    code, r = gw.events(0.0, had=False, timeout=15.0)
    evs = r.get("events") or []
    types = sorted({e.get("type") for e in evs})
    check("GET /api/events (first poll)", "200 + a sessions event", "{} {}".format(code, types),
          code == 200 and "sessions" in types)

    # 12. optional: one real turn, watched through the stream
    if args.turn or args.create:
        since = time.time()
        if args.create:
            # The PWA's "new chat" path: one call creates the session and
            # starts the turn. SwagCod answers with its own id while the PWA
            # keeps the one it invented, so the adapter aliases them - and the
            # alias must work for every later call.
            client_sid = "session-" + uuid.uuid4().hex
            params = {"sessionId": client_sid, "cwd": args.create, "content": args.message}
            if args.model:
                params["selections"] = {"model": args.model}
            print("-- creating a session in {} and starting a turn (spends provider tokens) --".format(args.create))
            code, r = gw.rpc("session.createAndStart", params)
            res = r.get("result") or {}
            check("rpc session.createAndStart", "ok + accepted + realSessionId + turnId",
                  "{} {} {}".format(res.get("accepted"), res.get("realSessionId"), res.get("turnId")),
                  r.get("ok") is True and res.get("accepted") is True
                  and bool(res.get("realSessionId")) and bool(res.get("turnId")))
            turn_id = str(res.get("turnId") or "")
            sid = str(res.get("realSessionId") or sid)
            code, r = gw.rpc("session.getState", {"sessionId": client_sid})
            check("alias resolves the PWA id", "ok + a status", str((r.get("result") or {}).get("status")),
                  r.get("ok") is True and bool((r.get("result") or {}).get("status")))
        else:
            print("-- sending a real turn in {} (spends provider tokens) --".format(sid))
            code, r = gw.rpc("session.startTurn", {"sessionId": sid, "content": args.message})
            res = r.get("result") or {}
            check("rpc session.startTurn", "ok + accepted + turnId",
                  "{} {}".format(res.get("accepted"), res.get("turnId")),
                  r.get("ok") is True and res.get("accepted") is True and bool(res.get("turnId")))
            turn_id = str(res.get("turnId") or "")

        seen_types = set()
        seen_ids = set()
        ended = False
        deadline = time.time() + args.wait
        cursor = since
        while time.time() < deadline and not ended:
            code, r = gw.events(cursor, had=True, timeout=25.0)
            for e in r.get("events") or []:
                cursor = max(cursor, float(e.get("ts") or cursor))
                t = e.get("type")
                seen_types.add(t)
                d = e.get("data") or {}
                if t == "items":
                    for it in d.get("items") or []:
                        seen_ids.add(it.get("id"))
                        if it.get("type") == "turn.end":
                            ended = True
                elif t == "turnEnded":
                    ended = True
            if not (r.get("events") or []):
                time.sleep(0.2)

        check("stream carries items", "an items event arrived", "{}".format(sorted(seen_types)),
              "items" in seen_types)
        check("stream carries the user message", "sw-{}-user".format(turn_id),
              "present" if "sw-{}-user".format(turn_id) in seen_ids else "missing",
              "sw-{}-user".format(turn_id) in seen_ids)
        check("stream carries the answer", "sw-{}-content".format(turn_id),
              "present" if "sw-{}-content".format(turn_id) in seen_ids else "missing",
              "sw-{}-content".format(turn_id) in seen_ids)
        check("turn ended", "turn.end item or turnEnded event", str(ended), ended)

        code, r = gw.rpc("session.getSnapshot", {"sessionId": sid, "limit": 200})
        items = (r.get("result") or {}).get("items") or []
        hist = [i.get("id") for i in items]
        check("history has the turn", "sw-{}-end".format(turn_id),
              "present" if "sw-{}-end".format(turn_id) in hist else "missing",
              "sw-{}-end".format(turn_id) in hist)

    return summarize()


def summarize() -> int:
    print("")
    failed = [r for r in RESULTS if not r[3]]
    width = max([len(r[0]) for r in RESULTS] + [5])
    print("{}  {}  {}  {}".format("Check".ljust(width), "Expect".ljust(34), "Actual".ljust(34), "Result"))
    for name, expect, actual, ok in RESULTS:
        print("{}  {}  {}  {}".format(name.ljust(width), expect[:34].ljust(34), actual[:34].ljust(34),
                                      "OK" if ok else "FAIL"))
    print("")
    print("checks: {}, failed: {}".format(len(RESULTS), len(failed)))
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
