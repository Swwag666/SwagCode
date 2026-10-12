#!/usr/bin/env python3
"""Tests for swagcod_backend.py — the SwagCod HTTP API adapter (F-9).

A fake SwagCod API runs on 127.0.0.1 (stdlib http.server, one thread): it
serves the real route set with the real payload shapes and a controllable SSE
stream, so the adapter is exercised end to end without the desktop app.

Run: python test_swagcod_backend.py   (or: python -m unittest test_swagcod_backend -v)
"""

from __future__ import annotations

import asyncio
import json
import queue
import threading
import time
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import swagcod_backend as sb

TOKEN = "test-token"


def wire(kind, data, ts_ms=None):
    """One SwagCod bus/transcript event in wire format B-8."""
    return {
        "seq": 0,
        "ts_ms": ts_ms or int(time.time() * 1000),
        "kind": {"kind": kind, "data": data},
    }


def sample_transcript(turn="t-1", sid="s-1", ok=True):
    return [
        wire("turn_started", {"turn": turn, "session": sid, "parent": None}),
        wire("user", {"turn": turn, "text": "привет"}),
        wire("reasoning", {"turn": turn, "text": "думаю"}),
        wire(
            "tool_call",
            {"turn": turn, "call_id": "c-1", "name": "read", "arguments": {"file_path": "a.txt"}},
        ),
        wire(
            "tool_result",
            {"turn": turn, "call_id": "c-1", "ok": True, "output": "содержимое", "elapsed_ms": 5},
        ),
        wire("content", {"turn": turn, "text": "готово"}),
        wire("turn_ended", {"turn": turn, "session": sid, "ok": ok, "reason": None if ok else "провал"}),
    ]


class FakeSwagCod:
    """The API surface the adapter uses, with hooks for assertions."""

    def __init__(self):
        self.sessions = [
            {
                "id": "s-1",
                "title": "починить тесты",
                "cwd": "D:\\SwagCod",
                "model": "test-model",
                "status": "idle",
                "turns": 1,
                "est_context_tokens": 1200,
                "last_ok": True,
                "last_activity_ms": 1_700_000_000_000,
                "approval_policy": "on_dangerous",
            }
        ]
        self.transcripts = {"s-1": sample_transcript()}
        self.turns = []          # [{session_id, message, model?}]
        self.approvals = []      # [{call_id, decision}]
        self.created = []        # [{cwd, model?}]
        self.stops = []
        self.models_fail = False
        self._turn_n = 0
        self._session_n = 1
        self._sse = []           # one queue per connected SSE client
        self._lock = threading.Lock()

    # ── helpers used by the handler ────────────────────────────────────────
    def new_turn(self, session_id):
        with self._lock:
            self._turn_n += 1
            return f"t-{self._turn_n}"

    def create_session(self, cwd, model):
        with self._lock:
            self._session_n += 1
            sid = f"s-{self._session_n}"
            brief = {
                "id": sid,
                "title": cwd,
                "cwd": cwd,
                "model": model or "test-model",
                "status": "idle",
                "turns": 0,
                "est_context_tokens": 0,
                "last_ok": None,
                "last_activity_ms": int(time.time() * 1000),
                "approval_policy": None,
            }
            self.sessions.insert(0, brief)
            self.transcripts[sid] = []
            return brief

    def sse_subscribe(self):
        q = queue.Queue()
        with self._lock:
            self._sse.append(q)
        return q

    def sse_unsubscribe(self, q):
        with self._lock:
            if q in self._sse:
                self._sse.remove(q)

    def publish(self, event):
        """Push one bus event to every connected SSE client."""
        frame = ("data: " + json.dumps(event, ensure_ascii=False) + "\n\n").encode("utf-8")
        with self._lock:
            clients = list(self._sse)
        for q in clients:
            q.put(frame)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "FakeSwagCod/1.0"

    def log_message(self, *args):
        pass  # silence request logging

    @property
    def api(self) -> FakeSwagCod:
        return self.server.api

    def _send(self, code, obj, ctype="application/json; charset=utf-8"):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.send_header("connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self):
        got = self.headers.get("authorization", "")
        return got == f"Bearer {TOKEN}"

    def _body(self):
        n = int(self.headers.get("content-length") or 0)
        if n <= 0:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except ValueError:
            return {}

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/health":
            return self._send(200, {"ok": True, "service": "swagcod-http-api"})
        if not self._authorized():
            return self._send(401, {"error": "unauthorized"})
        if path == "/v1/diagnostics":
            return self._send(200, {"version": "0.2.0", "uptime_ms": 1234, "sessions": len(self.api.sessions)})
        if path == "/v1/sessions":
            return self._send(200, list(self.api.sessions))
        if path == "/v1/models":
            if self.api.models_fail:
                return self._send(400, {"error": "провайдер: ключ не задан"})
            return self._send(200, {"object": "list", "data": [{"id": "test-model"}, {"id": "big-model"}]})
        if path == "/v1/events":
            return self._sse()
        return self._send(404, {"error": "not found"})

    def do_POST(self):
        path = self.path.split("?")[0]
        if not self._authorized():
            return self._send(401, {"error": "unauthorized"})
        body = self._body()
        if path == "/v1/sessions":
            cwd = str(body.get("cwd") or "")
            if not cwd or not (cwd.startswith("/") or cwd[1:3] == ":\\"):
                return self._send(400, {"error": "cwd должен быть абсолютным путём"})
            self.api.created.append(body)
            return self._send(200, self.api.create_session(cwd, body.get("model")))
        if path == "/v1/sessions/transcript":
            sid = str(body.get("session_id") or "")
            if sid not in self.api.transcripts:
                return self._send(400, {"error": f"сессия {sid} не найдена"})
            return self._send(200, self.api.transcripts[sid])
        if path == "/v1/turns":
            sid = str(body.get("session_id") or "")
            msg = str(body.get("message") or "")
            if not msg.strip():
                return self._send(400, {"error": "сообщение не может быть пустым"})
            if sid not in self.api.transcripts:
                return self._send(400, {"error": f"сессия {sid} не найдена"})
            self.api.turns.append(body)
            return self._send(200, {"turn_id": self.api.new_turn(sid)})
        if path == "/v1/turns/stop":
            self.api.stops.append(body)
            return self._send(400, {"error": f"нет живого хода у сессии {body.get('session_id')}"})
        if path == "/v1/approvals":
            self.api.approvals.append(body)
            return self._send(200, {})
        if path == "/v1/checkpoints":
            return self._send(200, [])
        return self._send(404, {"error": "not found"})

    def _sse(self):
        q = self.api.sse_subscribe()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-store")
        self.send_header("connection", "close")
        self.end_headers()
        try:
            self.wfile.write(b": ok\n\n")
            self.wfile.flush()
            while True:
                try:
                    frame = q.get(timeout=0.2)
                except queue.Empty:
                    continue
                self.wfile.write(frame)
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass
        finally:
            self.api.sse_unsubscribe(q)


def start_server():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    srv.daemon_threads = True
    api = FakeSwagCod()
    srv.api = api
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, api


def run(coro):
    """One fresh event loop per scenario, like `asyncio.run`."""
    return asyncio.run(coro)


class AdapterTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv, cls.api = start_server()
        cls.port = cls.srv.server_address[1]
        cls.base = f"http://127.0.0.1:{cls.port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()

    def setUp(self):
        self.events = []
        self.pushed = threading.Lock()

    def push(self, type_, data):
        with self.pushed:
            self.events.append({"type": type_, "data": data})

    def pushed_of(self, type_):
        with self.pushed:
            return [e["data"] for e in self.events if e["type"] == type_]

    def cfg(self, **over):
        c = {
            "backend": "swagcod",
            "swagcodBaseUrl": self.base,
            "swagcodToken": TOKEN,
            "swagcodSse": True,
            "swagcodTimeoutSecs": 10,
        }
        c.update(over)
        return c

    async def scenario(self, body):
        """One adapter lifetime: connect, run `body(adapter)`, close."""
        adapter = sb.SwagCodBridge(self.cfg(), self.push)
        await adapter.connect()
        try:
            return await body(adapter)
        finally:
            await adapter._close()

    # ── connection ─────────────────────────────────────────────────────────
    def test_connect_verifies_token_and_refuses_without_one(self):
        async def body(a):
            self.assertTrue(a.connected)
            self.assertEqual(a.version, "0.2.0")
            return True

        self.assertTrue(run(self.scenario(body)))

        async def no_token():
            a = sb.SwagCodBridge(self.cfg(swagcodToken=""), self.push)
            with self.assertRaises(sb.SwagCodError):
                await a.connect()
            self.assertFalse(a.connected)

        run(no_token())

        async def bad_token():
            a = sb.SwagCodBridge(self.cfg(swagcodToken="чужой"), self.push)
            with self.assertRaises(sb.SwagCodError) as ctx:
                await a.connect()
            self.assertIn("токен", str(ctx.exception))

        run(bad_token())

    def test_unreachable_api_reports_honestly(self):
        async def body():
            a = sb.SwagCodBridge(self.cfg(swagcodBaseUrl="http://127.0.0.1:1"), self.push)
            with self.assertRaises(sb.SwagCodError) as ctx:
                await a.connect()
            self.assertIn("недоступен", str(ctx.exception))
            self.assertFalse(a.connected)

        run(body())

    # ── sessions ───────────────────────────────────────────────────────────
    def test_session_list_maps_briefs_to_phone_shape(self):
        async def body(a):
            r = await a.request("session.list", {"limit": 1000})
            self.assertIn("sessions", r)
            # other tests create sessions, and the API sorts by activity: pick
            # the known one by id instead of trusting the order
            mine = [s for s in r["sessions"] if s["sessionId"] == "s-1"]
            self.assertEqual(len(mine), 1)
            s = mine[0]
            self.assertEqual(s["title"], "починить тесты")
            self.assertEqual(s["cwd"], "D:\\SwagCod")
            # orderingTime must be parseable by `new Date(...)` in the PWA
            self.assertTrue(s["orderingTime"].startswith("2023-"))
            self.assertEqual(s["metadata"]["model"], "test-model")
            self.assertEqual(s["metadata"]["approvalPolicy"], "on_dangerous")
            self.assertFalse(s["metadata"]["live"])
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_get_state_maps_status_and_selections(self):
        async def body(a):
            st = await a.request("session.getState", {"sessionId": "s-1"})
            self.assertEqual(st["status"], "idle")
            self.assertEqual(st["selections"]["model"], "test-model")
            self.assertEqual(st["selections"]["permission"], "on_dangerous")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_unknown_session_error_text_survives(self):
        async def body(a):
            with self.assertRaises(sb.SwagCodError) as ctx:
                await a.request("session.getSnapshot", {"sessionId": "нет-такой"})
            self.assertIn("не найдена", str(ctx.exception))
            return True

        self.assertTrue(run(self.scenario(body)))

    # ── transcript -> items ────────────────────────────────────────────────
    def test_snapshot_translates_transcript_into_items(self):
        async def body(a):
            r = await a.request("session.getSnapshot", {"sessionId": "s-1", "limit": 200})
            items = r["items"]
            ids = [i["id"] for i in items]
            self.assertEqual(
                ids,
                ["sw-t-1-user", "sw-t-1-reasoning", "sw-c-1", "sw-t-1-content", "sw-t-1-end"],
            )
            by_id = {i["id"]: i for i in items}
            self.assertEqual(by_id["sw-t-1-user"]["role"], "user")
            self.assertEqual(by_id["sw-t-1-user"]["content"]["text"], "привет")
            self.assertEqual(by_id["sw-t-1-reasoning"]["content"]["kind"], "reasoning")
            tool = by_id["sw-c-1"]
            self.assertEqual(tool["type"], "tool")
            self.assertEqual(tool["status"], "done")
            self.assertEqual(tool["content"]["toolName"], "read")
            self.assertEqual(tool["content"]["output"], "содержимое")
            self.assertFalse(tool["content"]["isError"])
            self.assertEqual(tool["content"]["input"], {"file_path": "a.txt"})
            end = by_id["sw-t-1-end"]
            self.assertEqual(end["type"], "turn.end")
            self.assertEqual(end["content"]["reason"]["kind"], "completed")
            # order must be monotonic: the PWA sorts by orderSeq
            seqs = [i["orderSeq"] for i in items]
            self.assertEqual(seqs, sorted(seqs))
            self.assertEqual(len(set(seqs)), len(seqs))
            # limit is honoured from the tail (newest items win)
            r2 = await a.request("session.getSnapshot", {"sessionId": "s-1", "limit": 2})
            self.assertEqual(len(r2["items"]), 2)
            self.assertEqual(r2["items"][-1]["id"], "sw-t-1-end")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_failed_turn_end_marks_error(self):
        self.api.transcripts["s-1"] = sample_transcript(ok=False)
        try:
            async def body(a):
                r = await a.request("session.getSnapshot", {"sessionId": "s-1"})
                end = [i for i in r["items"] if i["type"] == "turn.end"][-1]
                self.assertEqual(end["content"]["reason"]["kind"], "error")
                self.assertEqual(end["content"]["reason"]["message"], "провал")
                return True

            self.assertTrue(run(self.scenario(body)))
        finally:
            self.api.transcripts["s-1"] = sample_transcript()

    def test_unchanged_snapshot_does_not_bump_revision(self):
        async def body(a):
            first_r = await a.request("session.getSnapshot", {"sessionId": "s-1"})
            # pushed_of already unwraps `data`: each element is the payload
            first = {i["id"] for e in self.pushed_of("items") for i in (e.get("items") or [])}
            self.assertTrue(first)
            before = {i["id"]: i["revision"] for i in first_r["items"]}
            with self.pushed:
                self.events.clear()
            r = await a.request("session.getSnapshot", {"sessionId": "s-1"})
            # nothing changed -> nothing pushed -> the PWA does not re-render
            self.assertEqual(self.pushed_of("items"), [])
            after = {i["id"]: i["revision"] for i in r["items"]}
            self.assertEqual(before, after)
            # the transcript folds tool_call + tool_result into one item, so a
            # freshly built tool card legitimately sits at revision 2
            self.assertEqual(after["sw-c-1"], 2)
            self.assertEqual(after["sw-t-1-content"], 1)
            return True

        self.assertTrue(run(self.scenario(body)))

    # ── turns ──────────────────────────────────────────────────────────────
    def test_start_turn_posts_message_and_shows_user_item(self):
        async def body(a):
            r = await a.request("session.startTurn", {"sessionId": "s-1", "content": "почини"})
            self.assertTrue(r["accepted"])
            self.assertTrue(r["turnId"].startswith("t-"))
            self.assertEqual(self.api.turns[-1]["session_id"], "s-1")
            self.assertEqual(self.api.turns[-1]["message"], "почини")
            pushed = [i for e in self.pushed_of("items") for i in e["items"]]
            user = [i for i in pushed if i["id"] == f"sw-{r['turnId']}-user"]
            self.assertEqual(len(user), 1)
            self.assertEqual(user[0]["role"], "user")
            self.assertEqual(user[0]["content"]["text"], "почини")
            # the session is working now: the PWA shows the stop button
            st = await a.request("session.getState", {"sessionId": "s-1"})
            self.assertEqual(st["status"], "working")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_blank_message_is_refused_before_the_api_call(self):
        async def body(a):
            before = len(self.api.turns)
            with self.assertRaises(sb.SwagCodError):
                await a.request("session.startTurn", {"sessionId": "s-1", "content": "   "})
            self.assertEqual(len(self.api.turns), before)
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_model_choice_rides_the_next_turn(self):
        async def body(a):
            r = await a.request("session.updateSelections", {"sessionId": "s-1", "selections": {"model": "big-model"}})
            self.assertEqual(r["applied"], ["model"])
            await a.request("session.startTurn", {"sessionId": "s-1", "content": "привет"})
            self.assertEqual(self.api.turns[-1].get("model"), "big-model")
            st = await a.request("session.getState", {"sessionId": "s-1"})
            self.assertEqual(st["selections"]["model"], "big-model")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_permission_change_is_refused_with_a_reason(self):
        async def body(a):
            with self.assertRaises(sb.SwagCodError) as ctx:
                await a.request(
                    "session.updateSelections", {"sessionId": "s-1", "selections": {"permission": "never"}}
                )
            self.assertIn("десктоп", str(ctx.exception))
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_interrupt_reports_absent_turn_instead_of_failing(self):
        async def body(a):
            r = await a.request("session.interrupt", {"sessionId": "s-1"})
            self.assertFalse(r["stopped"])
            self.assertIn("нет живого хода", r["reason"])
            self.assertEqual(self.api.stops[-1]["session_id"], "s-1")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_create_and_start_aliases_the_client_id(self):
        client_sid = "session-" + uuid.uuid4().hex

        async def body(a):
            r = await a.request(
                "session.createAndStart",
                {"sessionId": client_sid, "cwd": "D:\\SwagCod", "content": "привет", "selections": {"model": "test-model"}},
            )
            self.assertTrue(r["accepted"])
            self.assertEqual(r["sessionId"], client_sid)
            real = r["realSessionId"]
            self.assertTrue(real.startswith("s-"))
            self.assertEqual(self.api.created[-1]["cwd"], "D:\\SwagCod")
            self.assertEqual(self.api.created[-1]["model"], "test-model")
            # later calls with the PWA's own id must resolve to the real one
            st = await a.request("session.getState", {"sessionId": client_sid})
            self.assertIn(st["status"], ("idle", "working"))
            snap = await a.request("session.getSnapshot", {"sessionId": client_sid})
            self.assertIsInstance(snap["items"], list)
            listed = await a.request("session.list", {})
            self.assertIn(real, [s["sessionId"] for s in listed["sessions"]])
            return True

        self.assertTrue(run(self.scenario(body)))

    # ── live stream ────────────────────────────────────────────────────────
    def test_sse_streams_content_with_growing_revision(self):
        async def body(a):
            await asyncio.sleep(0.3)  # let the SSE thread subscribe
            a._on_event(wire("turn_started", {"turn": "t-77", "session": "s-1"}))
            self.api.publish(wire("content", {"turn": "t-77", "text": "При"}))
            self.api.publish(wire("content", {"turn": "t-77", "text": "вет"}))
            await asyncio.sleep(0.6)
            pushes = [i for e in self.pushed_of("items") for i in e["items"] if i["id"] == "sw-t-77-content"]
            self.assertGreaterEqual(len(pushes), 2)
            self.assertEqual(pushes[0]["content"]["text"], "При")
            self.assertEqual(pushes[-1]["content"]["text"], "Привет")
            self.assertGreater(pushes[-1]["revision"], pushes[0]["revision"])
            self.assertEqual(pushes[-1]["role"], "assistant")
            # turn_started marked the session working
            states = self.pushed_of("state")
            self.assertTrue(states)
            self.assertEqual(states[0]["state"]["status"], "working")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_sse_reasoning_is_a_separate_item(self):
        async def body(a):
            await asyncio.sleep(0.3)
            self.api.publish(wire("turn_started", {"turn": "t-77", "session": "s-1"}))
            self.api.publish(wire("reasoning", {"turn": "t-77", "text": "думаю"}))
            self.api.publish(wire("content", {"turn": "t-77", "text": "ответ"}))
            await asyncio.sleep(0.6)
            items = {i["id"]: i for e in self.pushed_of("items") for i in e["items"]}
            self.assertEqual(items["sw-t-77-reasoning"]["content"]["kind"], "reasoning")
            self.assertEqual(items["sw-t-77-reasoning"]["content"]["text"], "думаю")
            self.assertNotIn("kind", items["sw-t-77-content"]["content"])
            self.assertEqual(items["sw-t-77-content"]["content"]["text"], "ответ")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_sse_tool_call_then_result_updates_one_item(self):
        async def body(a):
            await asyncio.sleep(0.3)
            self.api.publish(wire("turn_started", {"turn": "t-78", "session": "s-1"}))
            self.api.publish(wire("tool_call_start", {"turn": "t-78", "call_id": "c-9", "name": "bash"}))
            self.api.publish(
                wire("tool_call", {"turn": "t-78", "call_id": "c-9", "name": "bash", "arguments": {"command": "ls"}})
            )
            self.api.publish(
                wire("tool_result", {"turn": "t-78", "call_id": "c-9", "ok": False, "output": "отказ", "elapsed_ms": 12})
            )
            await asyncio.sleep(0.7)
            pushes = [i for e in self.pushed_of("items") for i in e["items"] if i["id"] == "sw-c-9"]
            self.assertEqual(len(pushes), 3)
            self.assertEqual(pushes[0]["status"], "running")
            self.assertEqual(pushes[1]["content"]["input"], {"command": "ls"})
            self.assertEqual(pushes[-1]["status"], "failed")
            self.assertTrue(pushes[-1]["content"]["isError"])
            self.assertEqual(pushes[-1]["content"]["output"], "отказ")
            self.assertEqual(pushes[-1]["content"]["toolName"], "bash")
            self.assertEqual([p["revision"] for p in pushes], [1, 2, 3])
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_sse_approval_becomes_notice_and_answer_reaches_api(self):
        async def body(a):
            await asyncio.sleep(0.3)
            self.api.publish(wire("turn_started", {"turn": "t-79", "session": "s-1"}))
            self.api.publish(
                wire(
                    "approval_required",
                    {"turn": "t-79", "call_id": "c-10", "tool": "write", "summary": "правка a.txt", "preview": "- старое\n+ новое"},
                )
            )
            await asyncio.sleep(0.6)
            states = self.pushed_of("state")
            self.assertEqual(states[-1]["state"]["status"], "waiting_approval")
            n = await a.request("session.getNotices", {"sessionId": "s-1"})
            self.assertEqual(len(n["notices"]), 1)
            notice = n["notices"][0]
            self.assertEqual(notice["noticeId"], "c-10")
            self.assertEqual(notice["status"], "open")
            self.assertEqual(notice["title"], "Подтвердить: write")
            self.assertIn("правка a.txt", notice["message"])
            self.assertIn("+ новое", notice["message"])
            self.assertEqual([act["actionId"] for act in notice["actions"]], ["approved", "denied"])
            # the session list marks it live/waiting too
            listed = await a.request("session.list", {})
            one = [s for s in listed["sessions"] if s["sessionId"] == "s-1"][0]
            self.assertEqual(one["metadata"]["status"], "waiting_approval")

            r = await a.request(
                "session.respondInteraction", {"sessionId": "s-1", "noticeId": "c-10", "actionId": "approved"}
            )
            self.assertEqual(r["decision"], "approved")
            self.assertEqual(self.api.approvals[-1], {"call_id": "c-10", "decision": "approved"})
            n2 = await a.request("session.getNotices", {"sessionId": "s-1"})
            self.assertEqual(n2["notices"], [])
            # deny maps to the API's `denied`
            await a.request(
                "session.respondInteraction", {"sessionId": "s-1", "noticeId": "c-11", "actionId": "denied"}
            )
            self.assertEqual(self.api.approvals[-1]["decision"], "denied")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_sse_turn_ended_pushes_marker_and_turnended(self):
        async def body(a):
            await asyncio.sleep(0.3)
            self.api.publish(wire("turn_started", {"turn": "t-80", "session": "s-1"}))
            self.api.publish(wire("content", {"turn": "t-80", "text": "готово"}))
            self.api.publish(wire("turn_ended", {"turn": "t-80", "session": "s-1", "ok": False, "reason": "хук отклонил"}))
            await asyncio.sleep(0.7)
            ended = self.pushed_of("turnEnded")
            self.assertEqual(ended[-1]["sessionId"], "s-1")
            items = [i for e in self.pushed_of("items") for i in e["items"] if i["id"] == "sw-t-80-end"]
            self.assertEqual(len(items), 1)
            self.assertEqual(items[0]["type"], "turn.end")
            self.assertEqual(items[0]["content"]["reason"]["kind"], "error")
            self.assertEqual(items[0]["content"]["reason"]["message"], "хук отклонил")
            st = await a.request("session.getState", {"sessionId": "s-1"})
            self.assertEqual(st["status"], "idle")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_sse_error_event_becomes_a_system_item(self):
        async def body(a):
            await asyncio.sleep(0.3)
            self.api.publish(wire("turn_started", {"turn": "t-81", "session": "s-1"}))
            self.api.publish(wire("error", {"turn": "t-81", "message": "провайдер недоступен"}))
            await asyncio.sleep(0.6)
            items = [i for e in self.pushed_of("items") for i in e["items"] if i["role"] == "system" and i["id"] != "sw-t-81-end"]
            self.assertTrue(items)
            self.assertEqual(items[-1]["content"]["text"], "провайдер недоступен")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_snapshot_and_stream_agree_on_ids(self):
        """The ids the live stream invents must match the transcript's, or the
        PWA would render every streamed message twice after a refresh."""
        async def body(a):
            await asyncio.sleep(0.3)
            turn = "t-1"  # the transcript's turn id
            self.api.publish(wire("turn_started", {"turn": turn, "session": "s-1"}))
            self.api.publish(wire("content", {"turn": turn, "text": "готово"}))
            await asyncio.sleep(0.6)
            live = {i["id"] for e in self.pushed_of("items") for i in e["items"]}
            snap = await a.request("session.getSnapshot", {"sessionId": "s-1"})
            ids = {i["id"] for i in snap["items"]}
            self.assertIn("sw-t-1-content", live)
            self.assertIn("sw-t-1-content", ids)
            # no duplicate items appeared from mixing the two paths
            self.assertEqual(len(snap["items"]), len(ids))
            return True

        self.assertTrue(run(self.scenario(body)))

    # ── catalogs ───────────────────────────────────────────────────────────
    def test_model_catalog_comes_from_the_api(self):
        async def body(a):
            r = await a.request("catalog.listModels", {})
            ids = [m["id"] for m in r["models"]]
            self.assertEqual(ids, ["test-model", "big-model"])
            self.assertEqual(r["models"][0]["selectionId"], "test-model")
            self.assertEqual(r["models"][0]["title"], "test-model")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_model_catalog_falls_back_to_session_models(self):
        self.api.models_fail = True
        try:
            async def body(a):
                a._models_at = 0.0
                r = await a.request("catalog.listModels", {})
                self.assertEqual([m["id"] for m in r["models"]], ["test-model"])
                self.assertIn("провайдер", r.get("error", ""))
                return True

            self.assertTrue(run(self.scenario(body)))
        finally:
            self.api.models_fail = False

    def test_permission_catalog_is_static_and_non_empty(self):
        async def body(a):
            r = await a.request("catalog.listPermissions", {})
            ids = [p["id"] for p in r["permissions"]]
            self.assertEqual(ids, ["on_dangerous", "always", "never"])
            for p in r["permissions"]:
                self.assertTrue(p["title"])
            presets = await a.request("catalog.listAgentPresets", {})
            self.assertEqual(presets["presets"][0]["id"], "standard")
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_workspace_list_collects_session_cwds(self):
        async def body(a):
            r = await a.request("workspace.list", {})
            self.assertIn({"path": "D:\\SwagCod", "title": "D:\\SwagCod"}, r["workspaces"])
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_capabilities_and_config_never_leak_the_token(self):
        async def body(a):
            caps = await a.request("runtime.getCapabilities", {})
            self.assertEqual(caps["backend"], "swagcod")
            self.assertTrue(caps["interrupt"])
            self.assertFalse(caps["attachments"])
            cfg = await a.request("runtime.getConfig", {})
            self.assertEqual(cfg["baseUrl"], self.base)
            self.assertNotIn(TOKEN, json.dumps(cfg))
            ping = await a.request("ping", {})
            self.assertEqual(ping["version"], "0.2.0")
            self.assertNotIn(TOKEN, json.dumps(ping))
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_unknown_method_is_refused(self):
        async def body(a):
            with self.assertRaises(sb.SwagCodError) as ctx:
                await a.request("session.updateDraft", {"sessionId": "s-1"})
            self.assertIn("не поддержан", str(ctx.exception))
            return True

        self.assertTrue(run(self.scenario(body)))

    def test_request_before_connect_fails_like_the_dsh_bridge(self):
        async def body():
            a = sb.SwagCodBridge(self.cfg(), self.push)
            with self.assertRaises(RuntimeError) as ctx:
                await a.request("session.list", {})
            self.assertEqual(str(ctx.exception), "bridge_disconnected")

        run(body())


class TranslatorTest(unittest.TestCase):
    """Pure translation, no I/O: the shapes itemrender.js branches on."""

    def test_ids_are_derived_from_swagcod_ids(self):
        items = sb.items_from_wire(sample_transcript())
        self.assertEqual(
            [i["id"] for i in items],
            ["sw-t-1-user", "sw-t-1-reasoning", "sw-c-1", "sw-t-1-content", "sw-t-1-end"],
        )

    def test_tool_result_without_a_call_still_renders(self):
        items = sb.items_from_wire(
            [wire("tool_result", {"turn": "t-2", "call_id": "c-2", "ok": False, "output": "boom", "elapsed_ms": 1})]
        )
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0]["type"], "tool")
        self.assertEqual(items[0]["status"], "failed")
        self.assertTrue(items[0]["content"]["isError"])

    def test_bookkeeping_events_produce_no_items(self):
        items = sb.items_from_wire(
            [
                wire("turn_started", {"turn": "t-3", "session": "s-1"}),
                wire("status", {"message": "провайдер подключён"}),
                wire("file_changed", {"session": "s-1", "paths": ["a.txt"]}),
            ]
        )
        self.assertEqual(items, [])

    def test_garbage_input_is_survivable(self):
        self.assertEqual(sb.items_from_wire(None), [])
        self.assertEqual(sb.items_from_wire([]), [])
        self.assertEqual(sb.items_from_wire([None, 1, "x", {"kind": None}]), [])
        # a tool event without call_id cannot be keyed, so it is dropped
        self.assertEqual(sb.items_from_wire([wire("tool_call", {"turn": "t", "name": "read"})]), [])

    def test_status_map_covers_every_swagcod_status(self):
        for status in ("idle", "ready", "running", "waiting_approval", "failed", "cancelled"):
            self.assertIn(status, sb.STATUS_MAP)
        self.assertEqual(sb.STATUS_MAP["running"], "working")
        self.assertEqual(sb.STATUS_MAP["waiting_approval"], "waiting_approval")

    def test_iso_timestamp_is_parsable(self):
        self.assertEqual(sb._iso(0), "")
        self.assertEqual(sb._iso(None), "")
        iso = sb._iso(1_700_000_000_000)
        self.assertTrue(iso.startswith("2023-11-14T22:13:20"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
