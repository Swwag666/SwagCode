#!/usr/bin/env node
/* E-4: sidecar JS-плагинов — отдельный node-процесс рядом с
 * store-server.js. Протокол: newline-delimited JSON-RPC 2.0 по stdio
 * (тот же транспорт, что у стора и MCP-клиента).
 *
 * Методы:
 *   ping                        -> { pong: true }
 *   load   {dir, db}            -> { plugins: [manifest], errors: [{file,error}] }
 *   list                        -> { plugins: [manifest], errors: [...] }
 *   call   {name, arguments, timeout_ms} -> { ok, output }
 *   shutdown                    -> { bye: true } и выход
 *
 * Каждый плагин — файл plugins/*.js — исполняется в собственном
 * vm-контексте: на загрузку 5 с, на вызов handler — timeout_ms
 * (синхронный код vm убивает по дедлайну; async-хендлеры ревизии 85
 * дожидаются того же дедлайна). Завершение (shutdown/close stdin)
 * дожидается исходящих call-ответов — ревизия 86.
 * Мост к хранилищу: только read-only API prefs (не сессий — приватность).
 * В stdout — ТОЛЬКО строки протокола: console плагина перехватывается в
 * буфер и возвращается в output.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const readline = require('readline');

const LOAD_TIMEOUT_MS = 5000;
const MAX_CALL_TIMEOUT_MS = 300000;

/* name -> { file, description, parameters, handler, ctx } */
const plugins = new Map();
let lastErrors = [];
let currentDb = null;
let prefsDb = null;
/* Причина, по которой prefs-мост не поднялся. Молчаливый null из
 * prefs.get() неотличим от «такого ключа нет», поэтому причина
 * запоминается и уходит в errors загрузки: владелец видит «мост не
 * поднят», а не думает, что prefs пустой. */
let prefsError = null;

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

/* Prefs-мост. better-sqlite3 в readonly-режиме не видит свежий WAL без
 * прав на -shm, поэтому соединение обычное, но API плагина выполняет
 * только SELECT — запись из плагина невозможна (мост не даётprepare/
 * exec, только get/all). */
function openPrefs(db) {
  if (db === null) return null;
  if (currentDb === db && prefsDb) return prefsDb;
  try {
    if (prefsDb) prefsDb.close();
  } catch (_) {}
  prefsDb = null;
  prefsError = null;
  currentDb = db;
  try {
    /* require() резолвится от пути ЭТОГО скрипта: расклад без
     * node_modules/better-sqlite3 (pnpm install --ignore-scripts) мост не
     * поднимет, и это видно в errors, а не только в пустом prefs.get(). */
    const Database = require('better-sqlite3');
    prefsDb = new Database(db, { fileMustExist: true });
  } catch (e) {
    prefsDb = null; // нет базы или модуля — prefs.get вернёт null честно
    prefsError = String((e && e.message) || e).split('\n')[0];
  }
  return prefsDb;
}

function prefsApi() {
  return {
    get(key) {
      const d = openPrefs(currentDb);
      if (!d) return null;
      try {
        const row = d.prepare('SELECT value FROM prefs WHERE key = ?').get(String(key));
        return row ? String(row.value) : null;
      } catch (_) {
        return null;
      }
    },
    all() {
      const d = openPrefs(currentDb);
      if (!d) return {};
      try {
        const out = {};
        for (const r of d.prepare('SELECT key, value FROM prefs').all()) out[String(r.key)] = String(r.value);
        return out;
      } catch (_) {
        return {};
      }
    },
  };
}

function manifestOf(p, file) {
  return {
    name: p.name,
    description: p.description,
    parameters: p.parameters,
    file,
  };
}

function doLoad(params) {
  const dir = String((params && params.dir) || '');
  if (params && typeof params.db === 'string' && params.db.length > 0) {
    currentDb = params.db;
  }
  plugins.clear();
  lastErrors = [];
  /* База передана — пробуем prefs-мост сразу, на загрузке: ошибка
   * всплывает в errors рядом с ошибками плагинов, а не в момент вызова
   * handler'а молчаливым null. */
  if (currentDb !== null && openPrefs(currentDb) === null) {
    lastErrors.push({
      file: 'prefs',
      error: 'prefs-мост не поднят (better-sqlite3 недоступен из ' + __filename + '): ' + prefsError,
    });
  }
  const list = [];
  let files = [];
  try {
    files = fs
      .readdirSync(dir)
      .filter((f) => f.endsWith('.js'))
      .sort();
  } catch (e) {
    lastErrors.push({ file: dir, error: 'каталог не читается: ' + e.message });
    return { plugins: [], errors: lastErrors, dir };
  }
  for (const f of files) {
    const file = path.join(dir, f);
    try {
      const code = fs.readFileSync(file, 'utf8');
      let manifest = null;
      const sandbox = {
        swagcod: {
          define(m) {
            manifest = m;
          },
        },
        /* console плагина не должен писать в stdout — это протокольный
           канал; перехватываем в нишу, реальный вывод — в output вызова. */
        console: { log() {}, warn() {}, error() {} },
      };
      const ctx = vm.createContext(sandbox);
      vm.runInContext(code, ctx, { timeout: LOAD_TIMEOUT_MS, filename: file });
      if (!manifest || typeof manifest.name !== 'string' || !manifest.name.trim() || typeof manifest.handler !== 'function') {
        lastErrors.push({ file: f, error: 'нет swagcod.define({ name, handler })' });
        continue;
      }
      const name = manifest.name.trim();
      if (plugins.has(name)) {
        lastErrors.push({ file: f, error: 'имя уже занято: ' + name });
        continue;
      }
      const parameters =
        manifest.parameters && typeof manifest.parameters === 'object'
          ? manifest.parameters
          : { type: 'object' };
      const entry = {
        name,
        description: String(manifest.description || ''),
        parameters,
        handler: manifest.handler,
        ctx,
      };
      plugins.set(name, entry);
      list.push(manifestOf(entry, f));
    } catch (e) {
      lastErrors.push({ file: f, error: String((e && e.message) || e) });
    }
  }
  return { plugins: list, errors: lastErrors, dir };
}

/* Ревизия 85, из зондажа «пацаны»: handler в JS по умолчанию пишут async —
прежде Promise от runInContext проходил как «объект» и сериализовался
в "{}": плагин «работал», результат молча проглывался. Теперь thenable
дождётся с тем же дедлайном; таймаут async-части — честная ошибка.
Вызов стал асинхронным: ответ уходит по завершении, родитель ждёт по id. */
async function doCall(params) {
  const name = String((params && params.name) || '');
  const p = plugins.get(name);
  if (!p) return { ok: false, output: 'нет плагина: ' + name };
  const raw = Number(params && params.timeout_ms);
  const timeoutMs = Number.isFinite(raw) && raw >= 50 ? Math.min(raw, MAX_CALL_TIMEOUT_MS) : 30000;
  const logs = [];
  const pushLog = (...a) => logs.push(a.map((x) => (typeof x === 'string' ? x : JSON.stringify(x))).join(' '));
  /* Контекст плагина переиспользуется (состояние между вызовами живёт),
     но stdout протокольный — console всегда перехвачен. */
  p.ctx.console = { log: pushLog, warn: pushLog, error: pushLog };
  p.ctx.__swagcod_handler = p.handler;
  p.ctx.__swagcod_args = params && params.arguments && typeof params.arguments === 'object' ? params.arguments : {};
  p.ctx.__swagcod_prefs = prefsApi();
  const withLogs = (text) => (logs.length ? text + '\n[log]\n' + logs.join('\n') : text);
  const finalize = (out) => {
    if (out && typeof out === 'object') {
      const ok = out.ok !== false;
      const text = out.output != null ? String(out.output) : JSON.stringify(out);
      return { ok, output: withLogs(text) };
    }
    return { ok: true, output: withLogs(out == null ? '' : String(out)) };
  };
  try {
    const out = vm.runInContext('globalThis.__swagcod_handler(__swagcod_args, { prefs: __swagcod_prefs })', p.ctx, {
      timeout: timeoutMs,
      filename: p.name,
    });
    if (out && typeof out.then === 'function') {
      const timed = { timed: true };
      const timeoutPromise = new Promise((resolve) => {
        const t = setTimeout(() => resolve(timed), timeoutMs);
        if (typeof t.unref === 'function') t.unref();
      });
      const raced = await Promise.race([out, timeoutPromise]);
      if (raced === timed) {
        throw new Error('async-плагин не ответил в дедлайн (' + timeoutMs + ' мс)');
      }
      return finalize(raced);
    }
    return finalize(out);
  } catch (e) {
    const msg = String((e && e.message) || e);
    const friendly = msg.includes('timed out') || msg.includes('дедлайн')
      ? 'плагин не ответил в дедлайн (' + timeoutMs + ' мс)'
      : 'плагин: ' + msg;
    return { ok: false, output: withLogs(friendly) };
  }
}

/* Ревизия 86 (фикс регресса 85): call асинхронный, и завершение обязано
дожидаться его ответа. Раньше shutdown (и close stdin) делали exit(0)
сразу — при pipelined-очереди load/call/shutdown микротаска call.then
не успевала отправить ответ: клиент терял id 2 молча. Счётчик исходящих
вызовов + отложенный bye закрывают это; сиротский потолок —
MAX_CALL_TIMEOUT_MS + 5 c, чтобы зависший плагин не оставлял процесс
навсегда. */
let inflightCalls = 0;
let pendingShutdownId = null;
let stdinClosed = false;
let orphanGuard = null;

function exitNow() {
  try {
    if (prefsDb) prefsDb.close();
  } catch (_) {}
  process.exit(0);
}

function armOrphanGuard() {
  if (orphanGuard) return;
  orphanGuard = setTimeout(exitNow, MAX_CALL_TIMEOUT_MS + 5000);
  if (typeof orphanGuard.unref === 'function') orphanGuard.unref();
}

function tryFinish() {
  if (inflightCalls > 0) return;
  if (pendingShutdownId !== null) {
    const id = pendingShutdownId;
    pendingShutdownId = null;
    send({ jsonrpc: '2.0', id, result: { bye: true } });
    exitNow();
    return;
  }
  if (stdinClosed) exitNow();
}

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  let m;
  try {
    m = JSON.parse(line);
  } catch (_) {
    return; // мусор не наш протокол
  }
  if (m.id === undefined || m.id === null) return; // уведомления игнорируем
  let result = null;
  let error = null;
  try {
    switch (m.method) {
      case 'ping':
        result = { pong: true };
        break;
      case 'load':
        result = doLoad(m.params || {});
        break;
      case 'list':
        result = {
          plugins: [...plugins.values()].map((p) => manifestOf(p, p.file || '')),
          errors: lastErrors,
        };
        break;
      case 'call':
        /* Ревизия 85: call — единственный асинхронный метод (async-хендлеры
        плагинов дожидаются микротасками). Ответ уходит по завершении;
        ревизия 86: исходящие вызовы считаются, завершение их не убивает. */
        inflightCalls++;
        armOrphanGuard();
        doCall(m.params || {})
          .then((r) => send({ jsonrpc: '2.0', id: m.id, result: r }))
          .catch((e) =>
            send({ jsonrpc: '2.0', id: m.id, error: { code: -32000, message: String((e && e.message) || e) } })
          )
          .then(() => {
            inflightCalls--;
            tryFinish();
          });
        return;
      case 'shutdown':
        pendingShutdownId = m.id;
        tryFinish();
        return;
      default:
        error = { code: -32601, message: 'неизвестный метод: ' + String(m.method) };
    }
  } catch (e) {
    error = { code: -32000, message: String((e && e.message) || e) };
  }
  send(error ? { jsonrpc: '2.0', id: m.id, error } : { jsonrpc: '2.0', id: m.id, result });
});
/* stdin закрыт — приложение ушло: sidecar не должен жить сиротой.
Ревизия 86: если в момент close исходят call-ответы, сначала они,
потом выход (с сиротским потолком на зависший плагин). */
rl.on('close', () => {
  stdinClosed = true;
  if (inflightCalls > 0) {
    armOrphanGuard();
    return;
  }
  exitNow();
});
