/* Хранилище SwagCod на better-sqlite3 — sidecar-процесс Rust-приложения.

Протокол: линейный JSON-RPC поверх stdio. Родитель пишет по одному
запросу на строку, сервер отвечает одной строкой на запрос. stdin закрылся
(родитель умер) — процесс выходит сам, сирот не остаётся.

Запросы:
  {"id":1,"method":"open","path":"...\\swagcod.db"}   — открыть/создать базу
  {"id":2,"method":"ping"}                             — живость
  {"id":3,"method":"query","sql":"SELECT ...","params":[...]}
        → {"id":3,"ok":true,"rows":[[...]]}
  {"id":4,"method":"exec","sql":"INSERT ...","params":[...]}
        → {"id":4,"ok":true,"changes":1,"insertId":7}
  {"id":5,"method":"batch","steps":[{"sql","params"},...]}
        → {"id":5,"ok":true,"changes":N}   — одна транзакция
  {"id":6,"method":"integrity"}                        — PRAGMA integrity_check
  {"id":7,"method":"shutdown"}                         — закрыть базу и выйти

Значения параметров: null | number | string (больших целых и blob в схеме
нет — timestamps в миллисекундах влезают в безопасный диапазон double).
Логи — только в stderr: stdout занят протоколом. */

'use strict';

const readline = require('readline');
const Database = require('better-sqlite3');

/** Схема один в один с crates/core/src/store.rs (SCHEMA + миграции B-3/B-7). */
const SCHEMA = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS sessions(
  id TEXT PRIMARY KEY,
  cwd TEXT NOT NULL,
  model TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  approval_policy TEXT,
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turns(
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  started_ms INTEGER NOT NULL,
  ended_ms INTEGER,
  ok INTEGER NOT NULL,
  failure TEXT,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL,
  tool_calls_json TEXT NOT NULL DEFAULT '[]',
  est_in INTEGER NOT NULL DEFAULT 0,
  est_out INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ord INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  reasoning TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT,
  tool_calls_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS prefs(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approvals(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  tool TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT 'user',
  decided_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks(
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  state TEXT NOT NULL DEFAULT 'queued',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_try_ms INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  every_ms INTEGER,
  created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(state, next_try_ms);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id, started_ms);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, ord);
CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, decided_ms);
`;

/** @type {Database.Database|null} */
let db = null;

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

/* better-sqlite3 считает `?1`-плейсхолдеры именными: позиционные
   аргументы для них — «too many parameters». Биндим массив значений
   объектом {1: v0, 2: v1, ...}. Весь SQL ядра — в `?N`-стиле. */
function bindArgs(params) {
  const list = params || [];
  if (list.length === 0) return [];
  const obj = {};
  list.forEach((v, i) => { obj[i + 1] = v; });
  return [obj];
}

function fail(id, err) {
  send({ id: id ?? null, ok: false, error: String((err && err.message) || err) });
}

function openDb(path) {
  const fs = require('fs');
  const nodePath = require('path');
  if (path !== ':memory:') {
    const dir = nodePath.dirname(path);
    if (dir && dir !== '.') fs.mkdirSync(dir, { recursive: true });
  }
  db = new Database(path);
  db.exec(SCHEMA);
  // Миграции для баз, созданных до B-3/B-7: «duplicate column» — норма.
  try { db.exec("ALTER TABLE sessions ADD COLUMN summary TEXT NOT NULL DEFAULT ''"); } catch { /* уже есть */ }
  try { db.exec('ALTER TABLE sessions ADD COLUMN approval_policy TEXT'); } catch { /* уже есть */ }
}

function handle(req) {
  const id = req.id ?? null;
  switch (req.method) {
    case 'ping':
      send({ id, ok: true, pong: true });
      return;
    case 'open':
      openDb(String(req.path));
      send({ id, ok: true, opened: String(req.path) });
      return;
    case 'integrity': {
      if (!db) throw new Error('база не открыта');
      const row = db.prepare('PRAGMA integrity_check').get();
      send({ id, ok: true, result: row ? row.integrity_check : null });
      return;
    }
    case 'query': {
      if (!db) throw new Error('база не открыта');
      const rows = db.prepare(req.sql).all(...bindArgs(req.params));
      // better-sqlite3 отдаёт объекты; протокол — массивы колонок: компактнее
      // и порядок детерминирован.
      const arrs = rows.map((r) => Object.values(r));
      send({ id, ok: true, rows: arrs });
      return;
    }
    case 'exec': {
      if (!db) throw new Error('база не открыта');
      const info = db.prepare(req.sql).run(...bindArgs(req.params));
      send({ id, ok: true, changes: info.changes, insertId: Number(info.lastInsertRowid) });
      return;
    }
    case 'batch': {
      if (!db) throw new Error('база не открыта');
      const steps = req.steps || [];
      const prepared = steps.map((s) => ({ stmt: db.prepare(s.sql), params: bindArgs(s.params) }));
      let changes = 0;
      const tx = db.transaction(() => {
        for (const p of prepared) changes += p.stmt.run(...p.params).changes;
      });
      tx();
      send({ id, ok: true, changes });
      return;
    }
    case 'shutdown':
      send({ id, ok: true, bye: true });
      if (db) { try { db.close(); } catch { /* уже закрыта */ } db = null; }
      process.exit(0);
      return;
    default:
      fail(id, new Error(`неизвестный метод: ${req.method}`));
  }
}

const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  const trimmed = line.trim();
  if (!trimmed) return;
  let req;
  try {
    req = JSON.parse(trimmed);
  } catch (e) {
    fail(null, e);
    return;
  }
  try {
    handle(req);
  } catch (e) {
    fail(req.id, e);
  }
});
/* Родитель умер или закрыл канал — жить незачем, сирот не оставляем. */
rl.on('close', () => {
  if (db) { try { db.close(); } catch { /* уже закрыта */ } db = null; }
  process.exit(0);
});
process.on('uncaughtException', (e) => {
  process.stderr.write(`store-sidecar: ${e && e.stack ? e.stack : e}\n`);
});
process.stderr.write('store-sidecar: готов (better-sqlite3)\n');
