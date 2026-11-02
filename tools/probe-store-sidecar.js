#!/usr/bin/env node
'use strict';

/* tools/probe-store-sidecar.js — «Кривые Руки»: ломаем sidecar/store-server.js
   (часть B) мусором поверх линейного JSON-RPC на stdio.

   Запуск: node tools/probe-store-sidecar.js
   Каждый ответ ждёт до 10 с (по ТЗ); для заведомой тишины — 2–2.5 с.
   Скрипт всегда выходит 0: это пробник, а не CI-тест. */

const { spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const SIDECAR = path.resolve(__dirname, '..', 'sidecar', 'store-server.js');
const NODE = process.env.SWAGCOD_NODE || 'node';
const RESP_MS = 10_000;
const SILENCE_MS = 2_500;

const workdir = fs.mkdtempSync(path.join(os.tmpdir(), 'probe-sidecar-'));
const findings = [];
const procs = new Set();
let cases = 0;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const j = (obj) => JSON.stringify(obj);
const oneLine = (s) => String(s).replace(/[\r\n\t]+/g, ' ').slice(0, 300);

function finding(sev, name, detail) {
  findings.push({ sev, name, detail });
  console.log(`    >>> НАХОДКА [${sev}] ${name} — ${detail}`);
}

class Sidecar {
  constructor() {
    this.proc = spawn(NODE, [SIDECAR], { cwd: workdir, stdio: ['pipe', 'pipe', 'pipe'] });
    procs.add(this);
    this.buf = '';
    this.rawOut = '';
    this.stderr = '';
    this.lines = [];
    this.waiters = [];
    this.exited = null;
    this.exitWaiters = [];
    this.proc.stdout.setEncoding('utf8');
    this.proc.stdout.on('data', (chunk) => {
      this.rawOut += chunk;
      this.buf += chunk;
      let i;
      while ((i = this.buf.indexOf('\n')) >= 0) {
        const line = this.buf.slice(0, i);
        this.buf = this.buf.slice(i + 1);
        const w = this.waiters.shift();
        if (w) w(line);
        else this.lines.push(line);
      }
    });
    this.proc.stderr.setEncoding('utf8');
    this.proc.stderr.on('data', (chunk) => { this.stderr += chunk; });
    this.proc.on('exit', (code, signal) => {
      this.exited = { code, signal };
      const ws = this.waiters.splice(0);
      for (const w of ws) w(null);
      const ew = this.exitWaiters.splice(0);
      for (const w of ew) w(this.exited);
    });
    this.proc.on('error', (e) => { this.spawnError = e; });
  }

  async waitReady() {
    const t0 = Date.now();
    while (!this.stderr.includes('готов') && Date.now() - t0 < 10_000) await sleep(50);
    if (!this.stderr.includes('готов')) {
      finding('high', 'sidecar не поднялся', `нет ready-строки за 10 с; stderr: ${oneLine(this.stderr)}`);
      return false;
    }
    return true;
  }

  write(chunk) {
    return new Promise((res, rej) => {
      this.proc.stdin.write(chunk, (err) => (err ? rej(err) : res()));
    });
  }

  async nextLine(timeoutMs = RESP_MS) {
    if (this.lines.length) return this.lines.shift();
    if (this.exited) return null;
    return new Promise((res) => {
      const w = (l) => { clearTimeout(timer); res(l); };
      const timer = setTimeout(() => {
        const i = this.waiters.indexOf(w);
        if (i >= 0) this.waiters.splice(i, 1);
        res(null);
      }, timeoutMs);
      this.waiters.push(w);
    });
  }

  async waitExit(timeoutMs = 5_000) {
    if (this.exited) return this.exited;
    return new Promise((res) => {
      const w = (x) => { clearTimeout(t); res(x); };
      const t = setTimeout(() => {
        const i = this.exitWaiters.indexOf(w);
        if (i >= 0) this.exitWaiters.splice(i, 1);
        res(null);
      }, timeoutMs);
      this.exitWaiters.push(w);
    });
  }

  async kill() {
    if (this.exited === null) {
      try { this.proc.stdin.end(); } catch { /* уже закрыт */ }
      try { this.proc.kill(); } catch { /* уже мёртв */ }
    }
    await this.waitExit(3_000);
  }
}

/** Отправить строку (перенос добавится при отсутствии) и дождаться ответа. */
async function roundtrip(sc, label, line, opts = {}) {
  cases++;
  const stderrBefore = sc.stderr.length;
  const t0 = Date.now();
  await sc.write(line.endsWith('\n') ? line : line + '\n');
  const raw = await sc.nextLine(opts.timeout ?? RESP_MS);
  const ms = Date.now() - t0;
  const errDelta = sc.stderr.slice(stderrBefore);
  if (raw === null) {
    finding(opts.sev ?? 'high', label, `нет ответа за ${ms} мс (процесс ${sc.exited ? 'умер' : 'жив'}); stderr: ${oneLine(errDelta)}`);
    return { raw: null, resp: null, ms, errDelta };
  }
  let resp = null;
  let unparseable = false;
  try { resp = JSON.parse(raw); } catch { unparseable = true; }
  if (unparseable) {
    finding('high', label, `ответ не JSON: ${oneLine(raw)}`);
  } else {
    console.log(`[${label}] ${ms} мс → ${oneLine(raw)}`);
  }
  return { raw, resp, ms, errDelta };
}

/** Ожидаем тишину: ответ был бы сюрпризом. */
async function silence(sc, label, waitMs = SILENCE_MS) {
  const raw = await sc.nextLine(waitMs);
  if (raw === null) {
    console.log(`[${label}] тишина ${waitMs} мс — как ожидали`);
  } else {
    finding('mid', label, `ожидали тишину, пришло: ${oneLine(raw)}`);
  }
  return raw;
}

async function main() {
  console.log(`пробник sidecar: node=${NODE}, sidecar=${SIDECAR}`);
  console.log(`workdir: ${workdir}`);
  const probe1 = path.join(workdir, 'probe1.db');
  const probe2 = path.join(workdir, 'probe2.db');

  const sc = new Sidecar();
  if (!(await sc.waitReady())) return;

  await roundtrip(sc, 'B01 мусорная строка (не JSON)', '⌁ вообще не JSON [[[{' + '\n');

  // B02: JSON-литерал null
  {
    cases++;
    const before = sc.stderr.length;
    const t0 = Date.now();
    await sc.write('null\n');
    const raw = await sc.nextLine(3_000);
    const delta = sc.stderr.slice(before);
    if (raw === null) {
      finding('high', 'B02 JSON-литерал null: ответа нет, uncaughtException-стек в stderr',
        `${Date.now() - t0} мс, процесс жив: ${!sc.exited}; stderr: ${oneLine(delta)}`);
    } else {
      console.log(`[B02] неожиданно есть ответ: ${oneLine(raw)}`);
    }
  }

  // B02b: прочие скалярные JSON-литералы — кто из них отвечает, а кто вешает
  {
    cases++;
    for (const [name, text] of [['42', '42\n'], ['строка', '"abc"\n'], ['массив', '[1, 2]\n'], ['true', 'true\n']]) {
      const t0 = Date.now();
      await sc.write(text);
      const raw = await sc.nextLine(2_000);
      if (raw === null) {
        finding('high', `B02b JSON-литерал ${name}: ответа нет`, '2 с тишины');
      } else {
        console.log(`[B02b ${name}] ${Date.now() - t0} мс → ${oneLine(raw)}`);
      }
    }
  }

  await roundtrip(sc, 'B03 ping после null-инцидента (живучесть)', j({ id: 3, method: 'ping' }));
  await roundtrip(sc, 'B04a ping без id', j({ method: 'ping' }));
  await roundtrip(sc, 'B04b ping со строковым id', j({ id: 'строка', method: 'ping' }));
  await roundtrip(sc, 'B05 method=""', j({ id: 5, method: '' }));
  await roundtrip(sc, 'B06 method="nosuch"', j({ id: 6, method: 'nosuch' }));

  // B07: open без path (и path:null — тот же корень: String(req.path))
  {
    const r = await roundtrip(sc, 'B07a open без path', j({ id: 10, method: 'open' }));
    const junk = path.join(workdir, 'undefined');
    if (r.resp && r.resp.ok) {
      finding('mid', 'B07 open без path молча создаёт файл "undefined"',
        `ответ: ${oneLine(r.raw)}; файл существует: ${fs.existsSync(junk)}`);
    }
    const r2 = await roundtrip(sc, 'B07b open path=null', j({ id: 11, method: 'open', path: null }));
    const junk2 = path.join(workdir, 'null');
    if (r2.resp && r2.resp.ok) {
      finding('mid', 'B07b open path=null молча создаёт файл "null"',
        `ответ: ${oneLine(r2.raw)}; файл существует: ${fs.existsSync(junk2)}`);
    }
  }

  // B08: open path="" (пустая строка) — ревизия 84: честный отказ
  {
    const r0 = await roundtrip(sc, 'B08a open path=""', j({ id: 12, method: 'open', path: '' }));
    if (r0.resp && r0.resp.ok) {
      finding('mid', 'B08 open path="" всё ещё даёт анонимную временную базу',
        `open "" ответил ok:true: ${oneLine(r0.raw)}`);
    } else {
      console.log(`[B08] open "" отвергнут: ${oneLine(r0.raw)}`);
    }
    const ri = await roundtrip(sc, 'B08b insert после отказа', j({ id: 13, method: 'exec', sql: "INSERT INTO prefs(key, value) VALUES ('tmp', 'v')", params: null }));
    if (ri.resp && ri.resp.ok) {
      finding('mid', 'B08 exec после отказа open "" пишет в базу', oneLine(ri.raw));
    }
  }

  await roundtrip(sc, 'B09 open в несуществующий глубокий путь', j({ id: 17, method: 'open', path: path.join(workdir, 'deep', 'sub', 'new.db') }));
  await roundtrip(sc, 'B10 open юникод-путь', j({ id: 18, method: 'open', path: path.join(workdir, 'каталог-🎉-проверка', 'база.db') }));

  await roundtrip(sc, 'B11a open рабочей базы probe1.db', j({ id: 20, method: 'open', path: probe1 }));
  await roundtrip(sc, 'B11b integrity', j({ id: 21, method: 'integrity' }));
  await roundtrip(sc, 'B11c маркер m1 в probe1', j({ id: 22, method: 'exec', sql: "INSERT INTO prefs(key, value) VALUES ('m1', 'probe1')", params: null }));

  await roundtrip(sc, 'B12 query с синтаксической ошибкой', j({ id: 36, method: 'query', sql: 'SELECT FROM WHERE', params: [] }));

  await roundtrip(sc, 'B13a params=объект', j({ id: 33, method: 'exec', sql: "INSERT INTO prefs(key, value) VALUES ('a', 'b')", params: { a: 1 } }));
  await roundtrip(sc, 'B13b params=строка', j({ id: 34, method: 'exec', sql: "UPDATE prefs SET value = 'x' WHERE key = 'a'", params: 'abc' }));
  await roundtrip(sc, 'B13c params=null', j({ id: 35, method: 'exec', sql: "INSERT INTO prefs(key, value) VALUES ('m2', 'null-params')", params: null }));

  await roundtrip(sc, 'B14 batch с пустыми шагами', j({ id: 40, method: 'batch', steps: [] }));
  await roundtrip(sc, 'B15 batch: шаг без sql', j({ id: 41, method: 'batch', steps: [{ params: [] }] }));

  // B16: атомарность batch — валидный шаг + FK-нарушитель
  {
    await roundtrip(sc, 'B16a batch [INSERT ок, FK-нарушитель]', j({
      id: 42,
      method: 'batch',
      steps: [
        { sql: "INSERT INTO prefs(key, value) VALUES ('batch1', 'v')" },
        { sql: "INSERT INTO messages(session_id, ord, role, content, reasoning) VALUES ('ghost', 0, 'user', 'x', '')" },
      ],
    }));
    const r = await roundtrip(sc, 'B16b проверка отката', j({ id: 43, method: 'query', sql: "SELECT COUNT(*) FROM prefs WHERE key = 'batch1'", params: [] }));
    const n = r.resp && r.resp.rows ? r.resp.rows[0][0] : '?';
    if (n !== 0) finding('high', 'B16 batch не атомарен', `после упавшего batch в базе остался шаг 1: ${n} строк`);
    else console.log('[B16] batch откатился целиком — атомарность ок');
  }

  // B17: 10 МБ одним запросом (?N-стиль — контракт биндинга sidecar)
  {
    const big = 'x'.repeat(10_000_000);
    const t0 = Date.now();
    cases++;
    const before = sc.stderr.length;
    await sc.write(`{"id":70,"method":"exec","sql":"INSERT INTO prefs(key, value) VALUES (?1, ?2)","params":["huge10mb",${JSON.stringify(big)}]}\n`);
    const raw = await sc.nextLine(RESP_MS);
    const ms = Date.now() - t0;
    console.log(`[B17a] 10 МБ строка: ответ за ${ms} мс → ${oneLine(raw)}`);
    if (raw === null) {
      finding('high', 'B17a 10 МБ exec: нет ответа', `за ${ms} мс; stderr: ${oneLine(sc.stderr.slice(before))}`);
    } else {
      let ok = false;
      try { ok = JSON.parse(raw).ok === true; } catch {}
      if (!ok) finding('high', 'B17a 10 МБ exec: ответ с ошибкой', oneLine(raw));
    }
    const rl = await roundtrip(sc, 'B17b длина прочитанного', j({ id: 71, method: 'query', sql: "SELECT length(value) FROM prefs WHERE key = 'huge10mb'", params: [] }));
    const ln = rl.resp && rl.resp.rows && rl.resp.rows[0] ? rl.resp.rows[0][0] : -1;
    if (ln !== 10_000_000) finding('high', 'B17b тихая порча 10 МБ', `length(value) = ${ln}, ожидалось 10000000`);
    else console.log('[B17b] length(value) = 10000000 — запись не порезана');
    // полный roundtrip: ответ сам тащит 10 МБ
    cases++;
    const t1 = Date.now();
    await sc.write(j({ id: 72, method: 'query', sql: "SELECT value FROM prefs WHERE key = 'huge10mb'", params: [] }) + '\n');
    const rawFull = await sc.nextLine(RESP_MS);
    const ms2 = Date.now() - t1;
    let okFull = false;
    try { okFull = JSON.parse(rawFull).rows[0][0] === big; } catch {}
    console.log(`[B17c] ответ с 10 МБ пришёл за ${ms2} мс, байт-в-байт: ${okFull}`);
    if (!okFull) finding('high', 'B17c ответ с 10 МБ не совпал', `длина строки ответа ${rawFull ? rawFull.length : 0}`);
    // ловушка контракта: обычные "?"-плейсхолдеры с массивом params
    cases++;
    await sc.write(`{"id":73,"method":"exec","sql":"INSERT INTO prefs(key, value) VALUES (?, ?)","params":["plainq",${JSON.stringify('v'.repeat(1_000))}]}\n`);
    const rawQ = await sc.nextLine(RESP_MS);
    console.log(`[B17d] плейн-? SQL с массивом params → ${oneLine(rawQ)}`);
    let qErr = '';
    try { qErr = JSON.parse(rawQ).error || ''; } catch {}
    if (qErr.includes('Too few parameter values')) {
      finding('low', 'B17d плейн-? SQL не биндится (bindArgs даёт {1:…}, better-sqlite3 ждёт ?N)',
        `стандартные "?"-плейсхолдеры + params-массив дают малопонятную ошибку "${qErr}"; продакшн-клиент шлёт ?N-стиль, для него пути нет`);
    }
  }

  // B18: строка без завершающего \n
  {
    cases++;
    await sc.write(JSON.stringify({ id: 80, method: 'ping' })); // без \n
    await silence(sc, 'B18a ответ не приходит, пока строка не завершена', 2_000);
    await sc.write('\n');
    const raw = await sc.nextLine(5_000);
    console.log(`[B18b] после дописывания \\n ответ пришёл: ${oneLine(raw)}`);
    if (raw === null) finding('high', 'B18b ответ не пришёл даже после \\n', 'тишина 5 с');
  }

  // B19: два запроса в одной строке — ревизия 84: осознанное ограничение
  // линейного протокола (одна строка = один запрос; хвост строки получает
  // один честный fail-ответ, а не молчание). Проверяем только пайплайн
  // двумя строками — он обязан отвечать обоим.
  {
    cases++;
    const aDb = path.join(workdir, 'a.db');
    const bDb = path.join(workdir, 'b.db');
    await sc.write(`{"id":90,"method":"open","path":${JSON.stringify(aDb)}} {"id":91,"method":"open","path":${JSON.stringify(bDb)}}\n`);
    const r1 = await sc.nextLine(5_000);
    console.log(`[B19a] хвост одной строки = один честный fail: ${oneLine(r1)}`);
    await silence(sc, 'B19b второго ответа нет', 2_000);
    // контроль: те же два open, но двумя строками в одном write
    cases++;
    const cDb = path.join(workdir, 'c.db');
    const dDb = path.join(workdir, 'd.db');
    await sc.write(`{"id":92,"method":"open","path":${JSON.stringify(cDb)}}\n{"id":93,"method":"open","path":${JSON.stringify(dDb)}}\n`);
    const rc = await sc.nextLine(5_000);
    const rd = await sc.nextLine(5_000);
    console.log(`[B19c] пайплайн двумя строками: ${oneLine(rc)} | ${oneLine(rd)}`);
    if (rc === null || rd === null) finding('high', 'B19c пайплайн двумя строками потерял ответ', `rc=${oneLine(rc)} rd=${oneLine(rd)}`);
  }
  await roundtrip(sc, 'B19d возврат на probe1', j({ id: 94, method: 'open', path: probe1 }));

  // B20: open на каталог — неудачный open и судьба старой базы
  {
    const r = await roundtrip(sc, 'B20a open path=каталог', j({ id: 60, method: 'open', path: workdir }));
    const q = await roundtrip(sc, 'B20b query после неудачного open', j({ id: 61, method: 'query', sql: "SELECT value FROM prefs WHERE key = 'm1'", params: [] }));
    const rows = q.resp && q.resp.rows;
    if (rows && rows.length === 1 && rows[0][0] === 'probe1') {
      finding('mid', 'B20 неудачный open оставляет старую базу подключённой',
        `open каталога ответил ошибкой (${oneLine(r.raw)}), но query продолжает отвечать данными СТАРОЙ базы: ${oneLine(q.raw)}`);
    } else {
      console.log(`[B20b] после неудачного open query вернул: ${oneLine(q.raw)}`);
    }
  }

  // B21: второе open поверх открытого
  {
    await roundtrip(sc, 'B21a второе open (probe2) поверх открытого probe1', j({ id: 50, method: 'open', path: probe2 }));
    const q = await roundtrip(sc, 'B21b query видит новую базу', j({ id: 51, method: 'query', sql: 'SELECT COUNT(*) FROM prefs', params: [] }));
    const n = q.resp && q.resp.rows ? q.resp.rows[0][0] : '?';
    console.log(`[B21b] после второго open prefs count = ${n} (в probe1 было 3)`);
    let unlinkErr = null;
    try { fs.unlinkSync(probe1); } catch (e) { unlinkErr = e; }
    if (unlinkErr) {
      finding('mid', 'B21 повторный open не закрывает прежний Database — хендл течёт',
        `unlink probe1.db из родителя → ${unlinkErr.code}: ${oneLine(unlinkErr.message)}`);
    } else {
      console.log('[B21c] старый файл удалился — хендл не удерживается');
    }
  }

  // B22: Infinity через 1e999 и NaN-литерал — ревизия 84: честный отказ
  {
    const r0 = await roundtrip(sc, 'B22a exec с 1e999 (Infinity)', '{"id":73,"method":"exec","sql":"INSERT INTO metrics(ts_ms, name, value) VALUES (?1, ?2, ?3)","params":[1, "inf", 1e999]}\n');
    if (r0.resp && r0.resp.ok) {
      const q = await roundtrip(sc, 'B22b чтение inf обратно', j({ id: 75, method: 'query', sql: "SELECT value FROM metrics WHERE name = 'inf'", params: [] }));
      finding('low', 'B22 Infinity (1e999) записался в базу',
        `exec ok:true, SELECT вернул ${JSON.stringify(q.resp && q.resp.rows ? q.resp.rows[0] : '?')}`);
    } else {
      console.log(`[B22] не-конечное число отвергнуто: ${oneLine(r0.raw)}`);
    }
    await roundtrip(sc, 'B22c NaN-литерал в JSON', '{"id":74,"method":"exec","sql":"INSERT INTO metrics(ts_ms, name, value) VALUES (?1, ?2, ?3)","params":[2, "nan", NaN]}\n');
  }

  // B23: непарный суррогат — ревизия 84: документированное ограничение
  // (JSON.stringify из Node уже не может вынести непарный суррогат в
  // wire-формат без повреждения; замена на U+FFFD — безопасный минимум).
  {
    await roundtrip(sc, 'B23a exec с непарным суррогатом', j({ id: 76, method: 'exec', sql: 'INSERT INTO prefs(key, value) VALUES (?1, ?2)', params: ['sur', '\ud800'] }));
    const q = await roundtrip(sc, 'B23b чтение суррогата обратно', j({ id: 77, method: 'query', sql: "SELECT value FROM prefs WHERE key = 'sur'", params: [] }));
    const v = q.resp && q.resp.rows && q.resp.rows[0] ? q.resp.rows[0][0] : '?';
    console.log(`[B23] непарный суррогат → U+${typeof v === 'string' && v.length ? v.codePointAt(0).toString(16) : '?'} (документированная замена)`);
  }

  // B24: NUL в строке параметра
  {
    await roundtrip(sc, 'B24a exec с NUL в значении', j({ id: 78, method: 'exec', sql: 'INSERT INTO prefs(key, value) VALUES (?1, ?2)', params: ['nul', 'a\u0000b'] }));
    const q = await roundtrip(sc, 'B24b чтение NUL обратно', j({ id: 79, method: 'query', sql: "SELECT value FROM prefs WHERE key = 'nul'", params: [] }));
    const v = q.resp && q.resp.rows && q.resp.rows[0] ? q.resp.rows[0][0] : '?';
    if (v !== 'a\u0000b') finding('mid', 'B24 NUL в строке параметра тихо портится', `записали "a\\0b" (3 симв.), прочитали ${JSON.stringify(v)}`);
    else console.log('[B24] NUL roundtrip байт-в-байт');
  }

  await sc.kill();

  // B25: shutdown — доходит ли прощальный ответ до выхода (5 запусков)
  let byeLost = 0;
  for (let i = 1; i <= 5; i++) {
    cases++;
    const s2 = new Sidecar();
    await s2.waitReady();
    await roundtrip(s2, `B25-${i}a open`, j({ id: 1, method: 'open', path: path.join(workdir, `bye-${i}.db`) }));
    const t0 = Date.now();
    await s2.write(j({ id: 2, method: 'shutdown' }) + '\n');
    const exit = await s2.waitExit(4_000);
    await sleep(300); // дать пайпу допринестись после exit-события
    const bye = s2.rawOut.includes('"bye":true');
    const ms = Date.now() - t0;
    console.log(`[B25-${i}] exit=${exit ? exit.code : 'не вышел'} bye=${bye} за ${ms} мс`);
    if (!bye) byeLost++;
    if (exit && exit.code !== 0) finding('mid', `B25-${i} нечистый выход`, `exit code ${exit.code}`);
    if (!exit) finding('high', `B25-${i} процесс не вышел после shutdown`, '4 с без exit');
  }
  if (byeLost > 0) {
    finding('high', 'B25 shutdown: прощальный ответ теряется (process.exit сразу после stdout.write)',
      `${byeLost} из 5 запусков не увидели {"bye":true} — родитель висит на shutdown до таймаута`);
  }

  // B26: ping после shutdown (сперва разобрать bye-ответ shutdown)
  {
    cases++;
    const s3 = new Sidecar();
    await s3.waitReady();
    await s3.write(j({ id: 1, method: 'open', path: path.join(workdir, 'b26.db') }) + '\n');
    await s3.nextLine(5_000);
    await s3.write(j({ id: 2, method: 'shutdown' }) + '\n');
    const bye = await s3.nextLine(3_000);
    const exit = await s3.waitExit(4_000);
    await sleep(200);
    let writeErr = null;
    try { await s3.write(j({ id: 3, method: 'ping' }) + '\n'); } catch (e) { writeErr = e; }
    const raw = await s3.nextLine(2_000);
    console.log(`[B26] bye=${oneLine(bye)} exit=${exit ? exit.code : '?'}; ping после shutdown: write=${writeErr ? writeErr.code : 'ок'}, ответ=${raw === null ? 'нет' : oneLine(raw)}`);
    if (raw !== null) finding('mid', 'B26 ping после shutdown получил ответ', oneLine(raw));
  }

  // B27: shutdown дважды
  {
    cases++;
    const s4 = new Sidecar();
    await s4.waitReady();
    await s4.write(j({ id: 1, method: 'open', path: path.join(workdir, 'b27.db') }) + '\n');
    await s4.nextLine(5_000);
    await s4.write(j({ id: 2, method: 'shutdown' }) + '\n' + j({ id: 3, method: 'shutdown' }) + '\n');
    const exit = await s4.waitExit(4_000);
    await sleep(300);
    const byes = (s4.rawOut.match(/"bye":true/g) || []).length;
    console.log(`[B27] двойной shutdown: exit=${exit ? exit.code : '?'}, bye-ответов=${byes} из 2`);
    if (byes === 0) finding('mid', 'B27 двойной shutdown: оба ответа потеряны', 'rawOut пуст');
  }

  // B28: stdin закрыт с незавершённой строкой
  {
    cases++;
    const s5 = new Sidecar();
    await s5.waitReady();
    await s5.write(j({ id: 1, method: 'open', path: path.join(workdir, 'b28.db') }) + '\n');
    await s5.nextLine(5_000);
    await s5.write(JSON.stringify({ id: 9, method: 'ping' })); // без \n
    await sleep(500);
    const none = await s5.nextLine(1_500);
    const t0 = Date.now();
    s5.proc.stdin.end();
    const exit = await s5.waitExit(3_000);
    console.log(`[B28] EOF с зависшей строкой: ответ=${none === null ? 'нет' : oneLine(none)}, exit=${exit ? exit.code : 'не вышел'} за ${Date.now() - t0} мс`);
    if (exit === null) finding('high', 'B28 процесс не вышел после закрытия stdin', '3 с без exit — сирота');
  }
}

async function finish() {
  for (const p of procs) await p.kill();
  console.log('\n=== ИТОГ ПРОБНИКА SIDECAR ===');
  console.log(`кейсов прогнано: ${cases}`);
  if (findings.length === 0) {
    console.log('находок нет');
  } else {
    console.log(`находок: ${findings.length}`);
    for (const f of findings) console.log(`[${f.sev}] ${f.name} — ${f.detail}`);
  }
  console.log(`workdir: ${workdir}`);
  try {
    fs.rmSync(workdir, { recursive: true, force: true, maxRetries: 3, retryDelay: 200 });
  } catch { /* что-то осталось под хендлом — пусть живёт в temp */ }
}

main()
  .then(finish)
  .catch(async (e) => {
    console.error('пробник упал:', e);
    await finish();
  });
