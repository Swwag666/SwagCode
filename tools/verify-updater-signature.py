#!/usr/bin/env python3
"""Проверка подписи артефакта автообновления SwagCod.

Зачем нужен: tauri-cli 2.12.1 не имеет `signer verify` (только sign/generate),
а неверная подпись означает, что ни одно установленное приложение не сможет
обновиться - апдейтер молча отвергнет артефакт. Ошибка при этом не видна ни в
одних воротах: сборка и публикация проходят зелёными.

Что проверяется:
  1. key_id подписи совпадает с key_id публичного ключа из
     crates/app/tauri.conf.json (plugins.updater.pubkey). Несовпадение значит,
     что артефакт подписан другой парой - например секрет CI расходится с
     локальным ~/.swagcod-updater.
  2. Основная подпись содержимого: Ed25519 над Blake2b-512 prehash файла.
     Именно её проверяет tauri-plugin-updater.
  3. Глобальная подпись trusted comment: Ed25519 над
     (64 байта основной подписи || тело trusted comment без префикса).
     Апдейтер её не проверяет (в minisign-verify это отдельный метод
     verify_trusted_comment), поэтому сбой здесь - предупреждение, а не отказ.

Формат .sig: base64 от minisign-файла подписи, то есть
    "untrusted comment: ...\\n"
    base64(algo[2] + key_id[8] + ed25519_sig[64])     -> 74 байта
    "trusted comment: ...\\n"
    base64(global_sig[64])
Публичный ключ в конфиге - base64 от minisign-файла ключа:
    "untrusted comment: ...\\n" + base64("Ed" + key_id[8] + pk[32])  -> 42 байта

Использование:
    python tools/verify-updater-signature.py <setup.exe> <setup.exe.sig>
    python tools/verify-updater-signature.py --tag v0.2.1        # из релиза GitHub
    python tools/verify-updater-signature.py --tag v0.2.1 --download-dir %TEMP%\\sig

Зависимости: `cryptography` (локально есть; в CI этот инструмент не гоняется).
Для режима --tag нужен `gh` в PATH, авторизованный на репозиторий.
Вывод намеренно ASCII: консоль Windows в кодовой странице 866/1251 ломает
кириллицу, а результат проверки читает человек.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import subprocess
import sys
import tempfile

try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
except ImportError:  # pragma: no cover - окружение без cryptography
    print("FAIL: нужен пакет 'cryptography': pip install cryptography")
    sys.exit(2)

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONFIG = os.path.join(REPO_ROOT, "crates", "app", "tauri.conf.json")
GH_OWNER_REPO = "Swwag666/SwagCode"


def b64(text: str) -> bytes:
    return base64.b64decode(text.strip())


def read_minisign_file(payload: bytes) -> list[str]:
    return payload.decode("utf-8").splitlines()


def load_pubkey() -> tuple[bytes, bytes, str]:
    """(key_id, raw_ed25519_pubkey, comment) из tauri.conf.json."""
    with open(CONFIG, encoding="utf-8") as fh:
        conf = json.load(fh)
    pub_b64 = conf["plugins"]["updater"]["pubkey"]
    lines = read_minisign_file(b64(pub_b64))
    raw = b64(lines[1])
    if len(raw) != 42:
        raise ValueError(f"публичный ключ {len(raw)} байт, ожидалось 42")
    return raw[2:10], raw[10:], lines[0]


def try_verify(key: Ed25519PublicKey, sig: bytes, candidates: dict[str, bytes]) -> list[str]:
    matched = []
    for name, payload in candidates.items():
        try:
            key.verify(sig, payload)
            matched.append(name)
        except Exception:
            continue
    return matched


def fetch_from_release(tag: str, download_dir: str) -> tuple[str, str]:
    """Скачать setup.exe + .sig из релиза GitHub через gh. Возвращает пути."""
    os.makedirs(download_dir, exist_ok=True)
    cmd = [
        "gh", "release", "download", tag,
        "--repo", GH_OWNER_REPO,
        "--dir", download_dir,
        "--clobber",
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"gh release download {tag} не удался: {proc.stderr.strip()[:300]}")
    setup = sig = None
    for name in os.listdir(download_dir):
        full = os.path.join(download_dir, name)
        if name.endswith(".sig"):
            sig = full
        elif name.endswith("-setup.exe"):
            setup = full
    if not setup or not sig:
        raise RuntimeError(
            f"в релизе {tag} нет пары setup.exe/.sig: {sorted(os.listdir(download_dir))}"
        )
    return setup, sig


def verify(target: str, sig_path: str) -> int:
    pub_key_id, pk_bytes, pub_comment = load_pubkey()
    print(f"pubkey : {pub_comment}")
    print(f"         key_id={pub_key_id.hex()} pk_len={len(pk_bytes)}")

    with open(target, "rb") as fh:
        data = fh.read()
    lines = read_minisign_file(b64(open(sig_path, "rb").read()))
    if len(lines) < 4:
        print(f"FAIL: в подписи {len(lines)} строк, ожидалось 4")
        return 1
    sig_raw = b64(lines[1])
    algo, key_id, signature = sig_raw[:2], sig_raw[2:10], sig_raw[10:]
    global_sig = b64(lines[3])
    print(f"sig    : algo={algo!r} key_id={key_id.hex()} sig_len={len(signature)} "
          f"global_len={len(global_sig)}")
    print(f"         {lines[2][:110]}")
    print(f"file   : {os.path.basename(target)} = {len(data)} bytes")

    if len(sig_raw) != 74 or len(global_sig) != 64:
        print("FAIL: неожиданная геометрия подписи (ожидалось 74 и 64 байта)")
        return 1
    if key_id != pub_key_id:
        print("FAIL: key_id НЕ совпадает с публичным ключом конфига")
        print("      артефакт подписан другой парой - апдейтер его отвергнет")
        return 1
    print("OK   : key_id matches the public key from tauri.conf.json")

    key = Ed25519PublicKey.from_public_bytes(pk_bytes)
    prehash = hashlib.blake2b(data, digest_size=64).digest()

    content_ok = try_verify(key, signature, {"prehash(blake2b512)": prehash, "raw": data})
    if not content_ok:
        print("FAIL: подпись содержимого не верифицируется ни на одной схеме")
        return 1
    print(f"OK   : content signature verifies via {content_ok}")

    trusted = lines[2]
    body = trusted.split(": ", 1)[1].encode("utf-8") if ": " in trusted else trusted.encode("utf-8")
    global_ok = try_verify(key, global_sig, {
        "sig64||trusted_body": signature + body,
        "sig74||trusted_full": sig_raw + trusted.encode("utf-8"),
        "sig64||prehash": signature + prehash,
        "sig64||raw": signature + data,
    })
    if not global_ok:
        print("WARN : global trusted-comment signature not reproduced")
        print("       updater does not check it; content signature is what matters")
        print("RESULT: VALID for the updater (content signature + key_id)")
        return 0
    print(f"OK   : global signature verifies via {global_ok}")
    print("RESULT: SIGNATURE VALID - updater will accept this artifact")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="Verify a Tauri updater artifact signature")
    ap.add_argument("target", nargs="?", help="path to the setup .exe")
    ap.add_argument("sig", nargs="?", help="path to the .sig file")
    ap.add_argument("--tag", help="download assets of this release tag via gh instead")
    ap.add_argument("--download-dir", default=os.path.join(tempfile.gettempdir(), "swagcod-sig"),
                    help="where to put assets downloaded with --tag")
    args = ap.parse_args()

    if args.tag:
        try:
            target, sig = fetch_from_release(args.tag, args.download_dir)
        except Exception as exc:
            print(f"FAIL: {exc}")
            return 2
        print(f"release: {args.tag} -> {target}")
        return verify(target, sig)

    if not args.target or not args.sig:
        ap.error("нужны либо <setup.exe> <setup.exe.sig>, либо --tag")
    return verify(args.target, args.sig)


if __name__ == "__main__":
    sys.exit(main())
