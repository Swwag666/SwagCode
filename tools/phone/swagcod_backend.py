#!/usr/bin/env python3
"""SwagCod backend for the dsh-phone gateway.

The gateway talks to exactly one backend interface (see `class Bridge` in
gateway.py): `.connected`, `.on_notify`, `async connect()`,
`async request(method, params, timeout)`, `async _close()`. This module
implements that same interface on top of the SwagCod loopback HTTP API
(SwagCod README, "HTTP API (E-7)" + the F-9 additions):

    GET  /health                   liveness, no token
    GET  /v1/diagnostics           version/uptime/active turns, token
    GET  /v1/sessions              SessionBrief[]
    GET  /v1/models                model catalog of the active provider
    POST /v1/sessions              {cwd, model?}          -> SessionBrief
    POST /v1/sessions/transcript   {session_id}           -> wire events (B-8)
    POST /v1/turns                 {session_id, message, model?} -> {turn_id}
    POST /v1/turns/stop            {session_id}
    POST /v1/approvals             {call_id, decision}
    GET  /v1/events                SSE stream of every bus event

Two jobs:
  1. translate the phone's RPC methods into those routes;
  2. translate SwagCod bus events into the gateway's own event types
     (`items`, `state`, `turnEnded`, `sessions`) so the PWA gets a live
     stream instead of waiting for the poll loop.

Wire events -> timeline items is the heart of it. The PWA keys items by
`item.id` and re-renders an item only when its `revision` changes, so ids must
be stable between the transcript path (history) and the SSE path (live
stream), and every content update must bump `revision`. Ids are therefore
derived from SwagCod's own ids:

    sw-<turn>-user        user message
    sw-<turn>-reasoning   model reasoning (separate visual channel)
    sw-<turn>-content     model answer
    sw-<call_id>          one tool call, running -> done/failed
    sw-<turn>-end         turn marker (completed / error)

Stdlib only, like the rest of this project. Blocking HTTP runs in threads
(`asyncio.to_thread`); the SSE stream lives in one daemon thread that pushes
into the event loop with `call_soon_threadsafe`.
"""

from __future__ import annotations

import asyncio
import json
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

# ── statics the PWA needs ──────────────────────────────────────────────────
# Approval policies SwagCod knows (crates/core/src/turn.rs, serde snake_case).
# The PWA refuses to open "new chat" until both catalogs are non-empty, so the
# permission list is served from here: it is a fixed enum, not runtime data.
PERMISSIONS = [
    {"id": "on_dangerous", "selectionId": "on_dangerous", "title": "спрашивать на опасных"},
    {"id": "always", "selectionId": "always", "title": "спрашивать всегда"},
    {"id": "never", "selectionId": "never", "title": "не спрашивать (опасно)"},
]

AGENT_PRESETS = [{"id": "standard", "selectionId": "standard", "title": "стандарт"}]

# SwagCod SessionStatus -> the status strings the PWA understands. "working"
# drives the stop button (isWorking), "waiting_approval" drives the approval
# card; anything else is shown verbatim in the chat meta line.
STATUS_MAP = {
    "idle": "idle",
    "ready": "idle",
    "running": "working",
    "waiting_approval": "waiting_approval",
    "failed": "failed",
    "cancelled": "cancelled",
}

# The model catalog comes from the provider over the network, so cache it: the
# PWA calls catalog.listModels every time the "new chat" sheet opens.
MODELS_TTL_SECS = 60.0
# How long a fetched session list stays fresh. The gateway polls
# session.getState per watched session every second; without this cache each
# poll would re-fetch the whole list.
SESSIONS_TTL_SECS = 0.75
# Tool output can be megabytes; the PWA slices to 4000 chars anyway.
OUTPUT_MAX_CHARS = 20000


class SwagCodError(RuntimeError):
    """The API answered with an error, or was unreachable."""


def _text(value) -> str:
    return value if isinstance(value, str) else ""


def _iso(ms) -> str:
    """SwagCod timestamps are unix ms; the PWA parses `orderingTime` with
    `new Date(...)`, so hand it an ISO-8601 string with an offset."""
    try:
        secs = float(ms or 0) / 1000.0
    except (TypeError, ValueError):
        secs = 0.0
    if secs <= 0:
        return ""
    return datetime.fromtimestamp(secs, tz=timezone.utc).isoformat()


def _item(item_id, role, content, seq, type_="message", status=None, turn=None, revision=1):
    """One PWA timeline item. `revision` is what makes the client re-render."""
    it = {
        "id": item_id,
        "role": role,
        "type": type_,
        "content": content,
        "orderSeq": seq,
        "revision": revision,
        "ts": time.time(),
    }
    if status:
        it["status"] = status
    if turn:
        it["turnId"] = turn
    return it


def _content_json(it) -> str:
    try:
        return json.dumps(it.get("content"), sort_keys=True, ensure_ascii=False)
    except (TypeError, ValueError):
        return str(it.get("content"))


def _same_item(a, b) -> bool:
    """Equal enough that the PWA must not re-render (revision stays put)."""
    return (
        a.get("status") == b.get("status")
        and a.get("type") == b.get("type")
        and a.get("role") == b.get("role")
        and _content_json(a) == _content_json(b)
    )


def items_from_wire(events, start_seq=0):
    """SwagCod wire events (B-8 transcript, or one SSE frame) -> PWA items.

    The transcript emits, per turn: turn_started, user, reasoning, tool_call,
    tool_result, content, turn_ended. The live bus uses the same names plus
    tool_call_start / approval_required / error / status, which this function
    also accepts so both paths produce identical ids.
    """
    out = []
    seq = int(start_seq)
    tools = {}
    for ev in events or []:
        if not isinstance(ev, dict):
            continue
        k = ev.get("kind") or {}
        name = k.get("kind")
        d = k.get("data") or {}
        turn = _text(d.get("turn"))
        if name == "user":
            out.append(_item(f"sw-{turn}-user", "user", {"text": _text(d.get("text"))}, seq, turn=turn))
        elif name == "reasoning":
            out.append(
                _item(
                    f"sw-{turn}-reasoning",
                    "assistant",
                    {"kind": "reasoning", "text": _text(d.get("text"))},
                    seq,
                    turn=turn,
                )
            )
        elif name == "content":
            out.append(_item(f"sw-{turn}-content", "assistant", {"text": _text(d.get("text"))}, seq, turn=turn))
        elif name in ("tool_call_start", "tool_call"):
            cid = _text(d.get("call_id"))
            if not cid:
                continue
            iid = f"sw-{cid}"
            content = {}
            nm = _text(d.get("name"))
            if nm:
                content["toolName"] = nm
            if name == "tool_call" and d.get("arguments") is not None:
                content["input"] = d.get("arguments")
            old = tools.get(iid)
            if old is not None:
                old["content"].update(content)
                old["revision"] = int(old.get("revision") or 1) + 1
            else:
                it = _item(iid, "assistant", content, seq, type_="tool", status="running", turn=turn)
                tools[iid] = it
                out.append(it)
        elif name == "tool_result":
            cid = _text(d.get("call_id"))
            if not cid:
                continue
            iid = f"sw-{cid}"
            ok = bool(d.get("ok"))
            content = {"output": _text(d.get("output"))[:OUTPUT_MAX_CHARS], "isError": not ok}
            old = tools.get(iid)
            if old is not None:
                old["content"].update(content)
                old["status"] = "done" if ok else "failed"
                old["revision"] = int(old.get("revision") or 1) + 1
            else:
                it = _item(
                    iid,
                    "assistant",
                    content,
                    seq,
                    type_="tool",
                    status="done" if ok else "failed",
                    turn=turn,
                )
                tools[iid] = it
                out.append(it)
        elif name == "turn_ended":
            ok = bool(d.get("ok"))
            out.append(
                _item(
                    f"sw-{turn}-end",
                    "system",
                    {
                        "kind": "turn_end",
                        "reason": {"kind": "completed" if ok else "error", "message": _text(d.get("reason"))},
                    },
                    seq,
                    type_="turn.end",
                    turn=turn,
                )
            )
        elif name == "error":
            msg = _text(d.get("message"))
            if msg:
                out.append(
                    _item(f"sw-{turn or 'bus'}-error-{seq}", "system", {"text": msg}, seq, turn=turn or None)
                )
        # turn_started / approval_required / status / file_changed / pty_*
        # carry nothing the PWA renders as a timeline item.
        else:
            continue
        seq += 1
    return out


class SwagCodBridge:
    """Drop-in replacement for `gateway.Bridge` backed by SwagCod's HTTP API."""

    def __init__(self, cfg, push_event=None):
        self.cfg = cfg if isinstance(cfg, dict) else {}
        self.base_url = _text(self.cfg.get("swagcodBaseUrl")).rstrip("/") or "http://127.0.0.1:47479"
        self.token = _text(self.cfg.get("swagcodToken"))
        self.use_sse = bool(self.cfg.get("swagcodSse", True))
        self.timeout = float(self.cfg.get("swagcodTimeoutSecs") or 30.0)
        self.push_event = push_event
        # Present for interface parity with Bridge; the SwagCod API has no
        # JSON-RPC notifications - the SSE thread pushes events itself.
        self.on_notify = None
        self.connected = False
        self.active_endpoint = self.base_url
        self.version = ""

        self._loop = None
        self._stop = threading.Event()
        self._thread = None
        self._gen = 0
        self._lock = threading.RLock()

        self._sessions = {}        # sid -> SessionBrief
        self._sessions_at = 0.0
        self._timeline = {}        # sid -> {item_id: item}
        self._seq = {}             # sid -> next orderSeq
        self._turn_session = {}    # turn -> sid
        self._live = {}            # sid -> turn currently running
        self._pending = {}         # sid -> {call_id: notice}
        self._aliases = {}         # PWA-generated sessionId -> SwagCod sid
        self._model_choice = {}    # sid -> model id picked on the phone
        self._models = []
        self._models_at = 0.0
        self._models_error = ""

        self._handlers = {
            "ping": self._m_ping,
            "session.list": self._m_session_list,
            "session.getState": self._m_session_get_state,
            "session.getSnapshot": self._m_session_get_snapshot,
            "session.getNotices": self._m_session_get_notices,
            "session.startTurn": self._m_session_start_turn,
            "session.createAndStart": self._m_session_create_and_start,
            "session.interrupt": self._m_session_interrupt,
            "session.respondInteraction": self._m_session_respond,
            "session.updateSelections": self._m_session_update_selections,
            "catalog.listModels": self._m_catalog_models,
            "catalog.listPermissions": self._m_catalog_permissions,
            "catalog.listAgentPresets": self._m_catalog_presets,
            "workspace.list": self._m_workspace_list,
            "runtime.getCapabilities": self._m_capabilities,
            "runtime.getConfig": self._m_config,
        }

    # ── plumbing ───────────────────────────────────────────────────────────
    def _push(self, type_, data):
        """Hand one gateway event to the PWA.

        Called from two places: the SSE thread (must hop into the loop) and
        coroutines serving /api/rpc (already in the loop). On the loop thread
        the callback runs inline, so a caller sees its own push the moment the
        request returns instead of one loop tick later.
        """
        cb = self.push_event
        if not cb:
            return
        loop = self._loop
        if loop is not None:
            try:
                if asyncio.get_running_loop() is loop:
                    cb(type_, data)
                    return
            except RuntimeError:
                pass  # no running loop here: we are in the SSE thread
        if loop is None:
            try:
                cb(type_, data)
            except Exception:
                pass
            return
        try:
            loop.call_soon_threadsafe(cb, type_, data)
        except Exception:
            pass

    def _spawn(self, coro):
        """Run a coroutine on the gateway loop from a non-loop thread."""
        loop = self._loop
        if loop is None:
            coro.close()
            return None
        try:
            return asyncio.run_coroutine_threadsafe(coro, loop)
        except Exception:
            coro.close()
            return None

    def _http(self, method, path, body=None, timeout=None):
        """One blocking request. Called through asyncio.to_thread.

        HTTP 4xx is a legitimate answer (the API reports why in
        `{"error": "..."}`), so it becomes SwagCodError with that text. A
        connection-level failure additionally clears `connected`: the gateway
        supervises that flag and reconnects.
        """
        url = self.base_url + path
        headers = {"accept": "application/json"}
        if self.token:
            headers["authorization"] = "Bearer " + self.token
        data = None
        if body is not None:
            data = json.dumps(body, ensure_ascii=False).encode("utf-8")
            headers["content-type"] = "application/json"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as exc:
            payload = ""
            try:
                payload = exc.read().decode("utf-8", "replace")
            except Exception:
                payload = ""
            msg = ""
            try:
                parsed = json.loads(payload)
                if isinstance(parsed, dict):
                    msg = _text(parsed.get("error"))
            except (ValueError, TypeError):
                msg = ""
            if not msg:
                msg = payload[:300] or f"HTTP {exc.code}"
            if exc.code in (401, 403):
                msg = f"токен не подошёл ({exc.code}): {msg}"
            raise SwagCodError(msg)
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            self.connected = False
            raise SwagCodError(f"SwagCod API недоступен ({self.base_url}): {exc}")
        except UnicodeEncodeError as exc:
            # a header value http.client cannot encode (a token pasted with
            # non-ASCII characters): report the config mistake, not a crash
            raise SwagCodError(f"заголовок не отправился (проверь swagcodToken): {exc}")
        if not raw.strip():
            return {}
        try:
            return json.loads(raw)
        except ValueError:
            raise SwagCodError("ответ API не JSON")

    def _sid(self, params) -> str:
        """Resolve a sessionId. `session.createAndStart` gets an id invented by
        the PWA, while SwagCod returns its own (`s-12`); the alias map keeps
        the phone's id working for every later call."""
        raw = _text(params.get("sessionId")) or _text(params.get("session_id"))
        with self._lock:
            return self._aliases.get(raw, raw)

    def _next_seq(self, sid) -> int:
        """Next orderSeq for a session. Caller may or may not hold the lock
        (RLock), so this is safe either way."""
        with self._lock:
            n = int(self._seq.get(sid) or 0)
            self._seq[sid] = n + 1
            return n

    async def _ensure_sessions(self, t=None, max_age=SESSIONS_TTL_SECS):
        with self._lock:
            age = time.time() - self._sessions_at
            if self._sessions and age < max_age:
                return
        await self._refresh_sessions(t=t, push=False)

    async def _refresh_sessions(self, t=None, push=True):
        raw = await asyncio.to_thread(self._http, "GET", "/v1/sessions", None, t or self.timeout)
        briefs = [b for b in (raw if isinstance(raw, list) else []) if isinstance(b, dict) and _text(b.get("id"))]
        briefs.sort(key=lambda b: -(b.get("last_activity_ms") or 0))
        with self._lock:
            self._sessions = {_text(b.get("id")): b for b in briefs}
            self._sessions_at = time.time()
            phone = [self._phone_session(b) for b in briefs]
        if push:
            self._push("sessions", {"sessions": phone})
        return phone

    def _phone_session(self, brief):
        sid = _text(brief.get("id"))
        status = _text(brief.get("status"))
        with self._lock:
            if self._pending.get(sid):
                status = "waiting_approval"
            elif self._live.get(sid):
                status = "running"
        return {
            "sessionId": sid,
            "externalSessionId": sid,
            "title": _text(brief.get("title")) or _text(brief.get("cwd")) or sid,
            "cwd": _text(brief.get("cwd")),
            "orderingTime": _iso(brief.get("last_activity_ms")),
            "metadata": {
                "live": STATUS_MAP.get(status, status) == "working",
                "status": status,
                "model": _text(brief.get("model")),
                "turns": brief.get("turns") or 0,
                "lastOk": brief.get("last_ok"),
                "estContextTokens": brief.get("est_context_tokens") or 0,
                "approvalPolicy": brief.get("approval_policy"),
                "backend": "swagcod",
            },
        }

    def _state(self, sid):
        with self._lock:
            brief = self._sessions.get(sid) or {}
            status = _text(brief.get("status"))
            if self._pending.get(sid):
                status = "waiting_approval"
            elif self._live.get(sid):
                status = "running"
            model = self._model_choice.get(sid) or _text(brief.get("model"))
            permission = brief.get("approval_policy") or "on_dangerous"
        return {
            "status": STATUS_MAP.get(status, status or "idle"),
            "selections": {"model": model, "permission": permission},
            "backend": "swagcod",
        }

    # ── interface parity with Bridge ───────────────────────────────────────
    async def connect(self):
        if not self.token:
            raise SwagCodError(
                "swagcodToken пустой: SwagCod → Настройки → Безопасность → «Копировать токен»"
            )
        # SwagCod's token is hex, so anything outside printable ASCII means the
        # config was pasted wrong (quotes, a Cyrillic typo, a line break).
        # http.client would raise UnicodeEncodeError from putheader instead —
        # an obscure crash for a plain config mistake.
        if not self.token.isascii() or not self.token.isprintable():
            raise SwagCodError("swagcodToken содержит недопустимые символы: нужен hex-токен из SwagCod")
        self._loop = asyncio.get_running_loop()
        self._stop.clear()
        self._gen += 1
        gen = self._gen
        try:
            await asyncio.to_thread(self._http, "GET", "/health", None, 10.0)
            diag = await asyncio.to_thread(self._http, "GET", "/v1/diagnostics", None, 15.0)
        except SwagCodError:
            self.connected = False
            raise
        self.version = _text((diag or {}).get("version"))
        self.connected = True
        if self.use_sse and self._thread is None:
            self._thread = threading.Thread(
                target=self._sse_loop, args=(gen,), name="swagcod-sse", daemon=True
            )
            self._thread.start()
        try:
            await self._refresh_sessions(push=False)
        except SwagCodError:
            pass
        return True

    async def request(self, method, params=None, timeout=30.0):
        if not self.connected:
            raise RuntimeError("bridge_disconnected")
        p = params if isinstance(params, dict) else {}
        handler = self._handlers.get(method)
        if handler is None:
            raise SwagCodError(f"метод {method} не поддержан бэкендом swagcod")
        return await handler(p, float(timeout or self.timeout))

    async def _close(self):
        self.connected = False
        self._stop.set()
        self._gen += 1
        # The SSE thread is a daemon blocked in a socket read; it notices the
        # generation change on its next byte or dies with the process. Joining
        # it here would hang the gateway's reconnect loop when the API is down.
        self._thread = None

    # ── RPC methods ────────────────────────────────────────────────────────
    async def _m_ping(self, p, t):
        diag = await asyncio.to_thread(self._http, "GET", "/v1/diagnostics", None, t)
        diag = diag if isinstance(diag, dict) else {}
        return {"ok": True, "backend": "swagcod", "version": _text(diag.get("version")), "uptime_ms": diag.get("uptime_ms")}

    async def _m_session_list(self, p, t):
        sessions = await self._refresh_sessions(t=t, push=False)
        limit = p.get("limit")
        if isinstance(limit, int) and limit > 0:
            sessions = sessions[:limit]
        return {"sessions": sessions}

    async def _m_session_get_state(self, p, t):
        sid = self._sid(p)
        if not sid:
            raise SwagCodError("нужен sessionId")
        try:
            await self._ensure_sessions(t)
        except SwagCodError:
            pass
        return self._state(sid)

    async def _m_session_get_snapshot(self, p, t):
        sid = self._sid(p)
        if not sid:
            raise SwagCodError("нужен sessionId")
        events = await asyncio.to_thread(
            self._http, "POST", "/v1/sessions/transcript", {"session_id": sid}, t
        )
        fresh = items_from_wire(events if isinstance(events, list) else [])
        changed = self._merge_transcript(sid, fresh)
        if changed:
            self._push("items", {"sessionId": sid, "items": changed})
        with self._lock:
            items = sorted(self._timeline.get(sid, {}).values(), key=lambda i: i.get("orderSeq") or 0)
            items = [dict(i) for i in items]
        limit = p.get("limit")
        if isinstance(limit, int) and limit > 0 and len(items) > limit:
            items = items[-limit:]
        return {"items": items, "sessionId": sid}

    async def _m_session_get_notices(self, p, t):
        sid = self._sid(p)
        with self._lock:
            pend = dict(self._pending.get(sid) or {})
        return {"notices": list(pend.values())}

    async def _m_session_start_turn(self, p, t):
        sid = self._sid(p)
        if not sid:
            raise SwagCodError("нужен sessionId")
        content = _text(p.get("content")) or _text(p.get("message"))
        if not content.strip():
            raise SwagCodError("пустое сообщение")
        body = {"session_id": sid, "message": content}
        with self._lock:
            model = self._model_choice.get(sid)
        if model:
            body["model"] = model
        res = await asyncio.to_thread(self._http, "POST", "/v1/turns", body, t)
        turn = _text((res or {}).get("turn_id"))
        if turn:
            self._add_user_item(sid, turn, content)
        return {"accepted": True, "queued": False, "sessionId": sid, "turnId": turn}

    async def _m_session_create_and_start(self, p, t):
        cwd = _text(p.get("cwd"))
        if not cwd:
            raise SwagCodError("нужен cwd (абсолютный путь папки)")
        content = _text(p.get("content")) or "привет"
        sel = p.get("selections") if isinstance(p.get("selections"), dict) else {}
        model = _text(sel.get("model"))
        body = {"cwd": cwd}
        if model:
            body["model"] = model
        brief = await asyncio.to_thread(self._http, "POST", "/v1/sessions", body, t)
        brief = brief if isinstance(brief, dict) else {}
        real = _text(brief.get("id"))
        if not real:
            raise SwagCodError("SwagCod не вернул id новой сессии")
        client_sid = _text(p.get("sessionId"))
        with self._lock:
            if client_sid and client_sid != real:
                self._aliases[client_sid] = real
            self._sessions[real] = brief
            self._sessions_at = 0.0
            if model:
                self._model_choice[real] = model
        turn_body = {"session_id": real, "message": content}
        if model:
            turn_body["model"] = model
        res = await asyncio.to_thread(self._http, "POST", "/v1/turns", turn_body, t)
        turn = _text((res or {}).get("turn_id"))
        if turn:
            self._add_user_item(real, turn, content)
        self._spawn(self._refresh_sessions(push=True))
        return {
            "accepted": True,
            "queued": False,
            # The PWA keeps working with the id it invented; the alias map in
            # _sid() resolves it to the real one on every later call.
            "sessionId": client_sid or real,
            "realSessionId": real,
            "turnId": turn,
        }

    async def _m_session_interrupt(self, p, t):
        sid = self._sid(p)
        if not sid:
            raise SwagCodError("нужен sessionId")
        try:
            await asyncio.to_thread(self._http, "POST", "/v1/turns/stop", {"session_id": sid}, t)
        except SwagCodError as exc:
            # "нет живого хода" is the normal answer when the turn already
            # finished; the phone only prints "стоп запрошен" either way.
            return {"stopped": False, "sessionId": sid, "reason": str(exc)}
        return {"stopped": True, "sessionId": sid}

    async def _m_session_respond(self, p, t):
        sid = self._sid(p)
        call_id = _text(p.get("noticeId")) or _text(p.get("id"))
        if not call_id:
            raise SwagCodError("нужен noticeId (id вызова инструмента)")
        action = _text(p.get("actionId")).lower()
        decision = "approved" if action in ("approved", "approve", "allow", "yes") else "denied"
        await asyncio.to_thread(
            self._http, "POST", "/v1/approvals", {"call_id": call_id, "decision": decision}, t
        )
        with self._lock:
            pend = self._pending.get(sid)
            if isinstance(pend, dict):
                pend.pop(call_id, None)
        self._push("state", {"sessionId": sid, "state": self._state(sid)})
        return {"answered": True, "decision": decision, "sessionId": sid}

    async def _m_session_update_selections(self, p, t):
        sid = self._sid(p)
        if not sid:
            raise SwagCodError("нужен sessionId")
        sel = p.get("selections") if isinstance(p.get("selections"), dict) else {}
        if "permission" in sel:
            # SwagCod's approval policy is a desktop setting (Settings ->
            # Security) and the loopback API deliberately has no setter: the
            # phone must not be able to widen permissions remotely.
            raise SwagCodError("политика подтверждений меняется в SwagCod на десктопе")
        applied = []
        model = _text(sel.get("model"))
        if model:
            with self._lock:
                self._model_choice[sid] = model
            applied.append("model")
        self._push("state", {"sessionId": sid, "state": self._state(sid)})
        return {"applied": applied, "note": "модель применится к следующему ходу"}

    async def _m_catalog_models(self, p, t):
        with self._lock:
            fresh = (time.time() - self._models_at) < MODELS_TTL_SECS
            if fresh and self._models:
                return {"models": list(self._models)}
        err = ""
        models = []
        try:
            raw = await asyncio.to_thread(self._http, "GET", "/v1/models", None, min(t, 25.0))
            data = raw.get("data") if isinstance(raw, dict) else None
            for m in data if isinstance(data, list) else []:
                mid = _text(m.get("id")) if isinstance(m, dict) else _text(m)
                if mid:
                    title = mid
                    if isinstance(m, dict):
                        title = _text(m.get("title")) or _text(m.get("label")) or mid
                    models.append({"id": mid, "selectionId": mid, "title": title})
        except SwagCodError as exc:
            err = str(exc)
        if not models:
            # Fallback: whatever the existing sessions run. Keeps "new chat"
            # usable when the provider catalog call fails (no key, offline).
            seen = []
            with self._lock:
                for b in self._sessions.values():
                    m = _text(b.get("model"))
                    if m and m not in seen:
                        seen.append(m)
            models = [{"id": m, "selectionId": m, "title": m} for m in seen]
        with self._lock:
            self._models = models
            self._models_at = time.time()
            self._models_error = err
        out = {"models": list(models)}
        if err:
            out["error"] = err
        return out

    async def _m_catalog_permissions(self, p, t):
        return {"permissions": list(PERMISSIONS)}

    async def _m_catalog_presets(self, p, t):
        return {"presets": list(AGENT_PRESETS), "agentPresets": list(AGENT_PRESETS)}

    async def _m_workspace_list(self, p, t):
        try:
            await self._ensure_sessions(t)
        except SwagCodError:
            pass
        paths = []
        with self._lock:
            for b in self._sessions.values():
                cwd = _text(b.get("cwd"))
                if cwd and cwd not in paths:
                    paths.append(cwd)
        return {"workspaces": [{"path": p_, "title": p_} for p_ in paths]}

    async def _m_capabilities(self, p, t):
        return {
            "backend": "swagcod",
            "version": self.version,
            "attachments": False,   # SwagCod's API takes text turns only
            "drafts": True,         # gateway-local
            "interrupt": True,
            "approvals": True,
            "models": True,
            "permissionSwitch": False,
            "revert": False,        # /v1/turns/revert exists but the PWA has no UI for it
        }

    async def _m_config(self, p, t):
        # Never hand the token back: the PWA does not need it and a leaked
        # /api/rpc answer would put a loopback API key in phone logs.
        return {"backend": "swagcod", "baseUrl": self.base_url, "sse": self.use_sse}

    # ── timeline bookkeeping ───────────────────────────────────────────────
    def _merge_transcript(self, sid, fresh):
        """Fold transcript-derived items into the live timeline.

        Returns only the items whose content actually changed, each with a
        bumped revision, so the PWA re-renders exactly those.
        """
        changed = []
        with self._lock:
            tl = self._timeline.setdefault(sid, {})
            for it in fresh:
                old = tl.get(it["id"])
                if old is None:
                    tl[it["id"]] = it
                    changed.append(dict(it))
                    continue
                if _same_item(old, it):
                    # Keep the known revision: an unchanged item must not
                    # re-render (the PWA compares revisions, not content).
                    it["revision"] = old.get("revision")
                    it["orderSeq"] = old.get("orderSeq", it.get("orderSeq"))
                else:
                    it["revision"] = int(old.get("revision") or 1) + 1
                    it["orderSeq"] = old.get("orderSeq", it.get("orderSeq"))
                    changed.append(dict(it))
                tl[it["id"]] = it
            seqs = [int(i.get("orderSeq") or 0) for i in tl.values()]
            self._seq[sid] = (max(seqs) + 1) if seqs else 0
        return changed

    def _add_user_item(self, sid, turn, text):
        """Show the sent message at once - the transcript only learns about it
        when the turn is persisted."""
        iid = f"sw-{turn}-user"
        with self._lock:
            tl = self._timeline.setdefault(sid, {})
            old = tl.get(iid)
            if old is not None:
                return
            it = _item(iid, "user", {"text": text}, self._next_seq(sid), turn=turn)
            tl[iid] = it
            self._live[sid] = turn
            self._turn_session[turn] = sid
            out = dict(it)
        self._push("items", {"sessionId": sid, "items": [out]})
        self._push("state", {"sessionId": sid, "state": self._state(sid)})

    def _append_text(self, sid, turn, iid, text, reasoning):
        if not sid or not text:
            return
        with self._lock:
            tl = self._timeline.setdefault(sid, {})
            it = tl.get(iid)
            if it is None:
                content = {"kind": "reasoning", "text": text} if reasoning else {"text": text}
                it = _item(iid, "assistant", content, self._next_seq(sid), turn=turn)
                tl[iid] = it
            else:
                c = it.get("content")
                if not isinstance(c, dict):
                    c = {"text": _text(c)}
                c["text"] = _text(c.get("text")) + text
                it["content"] = c
                it["revision"] = int(it.get("revision") or 1) + 1
            out = dict(it)
            out["content"] = dict(it["content"]) if isinstance(it.get("content"), dict) else it.get("content")
        self._push("items", {"sessionId": sid, "items": [out]})

    def _tool_update(self, sid, turn, d, result):
        cid = _text(d.get("call_id"))
        if not sid or not cid:
            return
        iid = f"sw-{cid}"
        with self._lock:
            tl = self._timeline.setdefault(sid, {})
            it = tl.get(iid)
            created = it is None
            if created:
                it = _item(
                    iid,
                    "assistant",
                    {"toolName": _text(d.get("name")) or "tool"},
                    self._next_seq(sid),
                    type_="tool",
                    status="running",
                    turn=turn,
                )
                tl[iid] = it
            c = it.get("content")
            if not isinstance(c, dict):
                c = {}
            nm = _text(d.get("name"))
            if nm:
                c["toolName"] = nm
            if not result and d.get("arguments") is not None:
                c["input"] = d.get("arguments")
            if result:
                ok = bool(d.get("ok"))
                c["output"] = _text(d.get("output"))[:OUTPUT_MAX_CHARS]
                c["isError"] = not ok
                c["elapsedMs"] = d.get("elapsed_ms")
                it["status"] = "done" if ok else "failed"
            it["content"] = c
            # A brand-new item starts at revision 1; only a real update bumps.
            # The transcript path folds call+result into one item too, so both
            # paths reach the same numbers for the same item.
            if not created:
                it["revision"] = int(it.get("revision") or 1) + 1
            out = dict(it)
            out["content"] = dict(c)
        self._push("items", {"sessionId": sid, "items": [out]})

    def _add_pending(self, sid, turn, d):
        cid = _text(d.get("call_id"))
        if not sid or not cid:
            return
        tool = _text(d.get("tool")) or "инструмент"
        summary = _text(d.get("summary"))
        preview = _text(d.get("preview"))
        msg = summary
        if preview:
            msg = (msg + "\n\n" if msg else "") + preview[:4000]
        notice = {
            "noticeId": cid,
            "id": cid,
            "status": "open",
            "revision": 1,
            "title": f"Подтвердить: {tool}",
            "message": msg,
            "tool": tool,
            "turnId": turn,
            # The PWA renders one button per action and sends actionId back
            # through session.respondInteraction; these ids are exactly the
            # `decision` values SwagCod's /v1/approvals accepts.
            "actions": [
                {"actionId": "approved", "label": "Разрешить", "style": "primary"},
                {"actionId": "denied", "label": "Запретить"},
            ],
        }
        with self._lock:
            self._pending.setdefault(sid, {})[cid] = notice
            if turn:
                self._turn_session.setdefault(turn, sid)
        self._push("state", {"sessionId": sid, "state": self._state(sid)})

    def _end_turn(self, sid, turn, d):
        ok = bool(d.get("ok"))
        reason = _text(d.get("reason"))
        content = {
            "kind": "turn_end",
            "reason": {"kind": "completed" if ok else "error", "message": reason},
        }
        out = None
        if sid and turn:
            iid = f"sw-{turn}-end"
            with self._lock:
                tl = self._timeline.setdefault(sid, {})
                it = tl.get(iid)
                if it is None:
                    it = _item(iid, "system", content, self._next_seq(sid), type_="turn.end", turn=turn)
                    tl[iid] = it
                else:
                    it["content"] = content
                    it["revision"] = int(it.get("revision") or 1) + 1
                out = dict(it)
                out["content"] = dict(content)
        with self._lock:
            if sid:
                self._live.pop(sid, None)
                self._pending.pop(sid, None)
            if turn:
                self._turn_session.pop(turn, None)
        if out is not None:
            self._push("items", {"sessionId": sid, "items": [out]})
        if sid:
            self._push("state", {"sessionId": sid, "state": self._state(sid)})
            self._push("turnEnded", {"sessionId": sid})
        self._spawn(self._refresh_sessions(push=True))

    # ── SSE ────────────────────────────────────────────────────────────────
    def _on_event(self, ev):
        """One SwagCod bus event -> gateway events. Runs in the SSE thread."""
        if not isinstance(ev, dict):
            return
        k = ev.get("kind")
        if not isinstance(k, dict):
            return
        name = k.get("kind")
        d = k.get("data") if isinstance(k.get("data"), dict) else {}
        turn = _text(d.get("turn"))
        with self._lock:
            sid = _text(d.get("session")) or self._turn_session.get(turn) or ""
        if not sid and name not in ("status", "closed"):
            return
        if name == "turn_started":
            if turn:
                with self._lock:
                    self._turn_session[turn] = sid
                    self._live[sid] = turn
            self._push("state", {"sessionId": sid, "state": self._state(sid)})
            self._spawn(self._refresh_sessions(push=True))
        elif name == "content":
            self._append_text(sid, turn, f"sw-{turn}-content", _text(d.get("text")), False)
        elif name == "reasoning":
            self._append_text(sid, turn, f"sw-{turn}-reasoning", _text(d.get("text")), True)
        elif name in ("tool_call_start", "tool_call"):
            self._tool_update(sid, turn, d, False)
        elif name == "tool_result":
            self._tool_update(sid, turn, d, True)
        elif name == "approval_required":
            self._add_pending(sid, turn, d)
        elif name == "turn_ended":
            self._end_turn(sid, turn, d)
        elif name == "error":
            msg = _text(d.get("message"))
            if msg:
                iid = f"sw-{turn or 'bus'}-error"
                with self._lock:
                    tl = self._timeline.setdefault(sid, {})
                    it = tl.get(iid)
                    if it is None:
                        it = _item(iid, "system", {"text": msg}, self._next_seq(sid), turn=turn or None)
                        tl[iid] = it
                    else:
                        it["content"] = {"text": msg}
                        it["revision"] = int(it.get("revision") or 1) + 1
                    out = dict(it)
                self._push("items", {"sessionId": sid, "items": [out]})
        # status / file_changed / pty_output / pty_exit / closed: nothing the
        # phone renders.

    def _sse_loop(self, gen):
        """Read /v1/events forever, reconnecting. Owns `connected`: when the
        stream dies the gateway's supervisor sees it and reconnects, exactly
        like the DSH bridge's read loop does."""
        url = self.base_url + "/v1/events"
        while not self._stop.is_set() and self._gen == gen:
            try:
                req = urllib.request.Request(
                    url,
                    headers={
                        "authorization": "Bearer " + self.token,
                        "accept": "text/event-stream",
                    },
                )
                with urllib.request.urlopen(req, timeout=None) as resp:
                    for raw in resp:
                        if self._stop.is_set() or self._gen != gen:
                            return
                        line = raw.decode("utf-8", "replace").strip()
                        if not line.startswith("data:"):
                            continue  # comments (": lagged N") and blank lines
                        payload = line[5:].strip()
                        if not payload:
                            continue
                        try:
                            ev = json.loads(payload)
                        except ValueError:
                            continue
                        try:
                            self._on_event(ev)
                        except Exception:
                            pass
            except Exception:
                pass
            if self._stop.is_set() or self._gen != gen:
                return
            self.connected = False
            self._push("bridge", {"status": "disconnected", "error": "SSE оборван"})
            time.sleep(2.0)
