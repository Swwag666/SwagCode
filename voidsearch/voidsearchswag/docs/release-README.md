# VoidSearchSwag - готовые сборки

Эта ветка (`release`) держит только артефакты: бинарник, суммы,
установщик. Исходники, документация и история - в ветке `main`.

## Установка в один клик (Windows)

```
git clone --depth 1 -b release <адрес-репо> vss
cd vss
install.bat
start.bat
```

`install.bat` сам:
- проверит SHA256 бинаря по `sums.txt`;
- скопирует `voidsearchswag-windows-amd64.exe` в `voidsearchswag.exe`
  (Go не нужен вообще);
- скачает Tor Expert Bundle (`setup`);
- сгенерирует `token.txt` и `start.bat`.

Сервер поднимется на `127.0.0.1:3333`, токен - в `token.txt`, отдай
его MCP-клиенту как Bearer. Токен не светится в argv: `start.bat`
читает его из файла (`VOIDSEARCH_HTTP_TOKEN_FILE`).

Если dist.torproject.org отдаёт 503:
```
set VOIDSEARCH_TOR_DIST=https://твоё-зеркало/torbrowser/
install.bat
```

Повторный запуск `install.bat` безопасен: токен и exe не
перезаписываются, Tor не перекачивается.

## Платформы

| Файл | Платформа |
|---|---|
| `voidsearchswag-windows-amd64.exe` | Windows x64 |
| `voidsearchswag-linux-amd64` | Linux x64 |
| `voidsearchswag-linux-arm64` | Linux ARM64 |
| `voidsearchswag-darwin-arm64` | macOS Apple Silicon |

Сборка помечена: `voidsearchswag version` печатает версию, коммит и
дату; на `0.1.0` совпадает с тегом `v0.1.0` в main. Сверяй `sums.txt`
(sha256) после любого копирования.

## Обновление

```
git fetch origin
git reset --hard origin/release
install.bat
```
