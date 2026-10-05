# Ранбук релиза

## Сборка версии

Версия не хранится в коде как релизная константа. Бинарь получает её от
линкера:

```bash
go build -trimpath \
  -ldflags "-X main.version=v0.1.0 -X main.buildCommit=80d6e00 -X main.buildDate=2026-10-01T13:54:00Z" \
  -o bin/voidsearchswag ./cmd/voidsearchswag
```

Без заливки бинарь честно печатает `0.1.0-dev` и не тащит мету - самосбор
отличим от релиза с первого `voidsearchswag version`:

```
voidsearchswag 0.1.0-dev
voidsearchswag v0.1.0 (80d6e00, 2026-10-01T13:54:00Z)
```

JSON для скриптов: `voidsearchswag version --json` отдаёт
`program`, `version`, `commit`, `date`.

## Makefile

`VERSION` выводится сам, из git:

- `make build` - боевой бинарь текущей платформы с метой;
- `make cross` - четыре цели: windows/amd64, linux/amd64, linux/arm64,
  darwin/arm64, все CGO_ENABLED=0 (прод не тянет libc);
- `make release` - `test` + `cross` + `sha256sum` бинарей в `bin/sums.txt`.

`VERSION` по умолчанию - `git describe --tags --always --dirty`: ближайший
тег, без тегов - короткий хеш коммита, у грязного дерева - суффикс
`-dirty`. Задать руками: `make VERSION=v0.2.0 release`.

## CI

`.github/workflows/ci.yml` на каждый пуш: формат, vet, тесты с покрытием,
детектор гонок, кросс-сборка в `dist/` с той же заливкой версии,
`sha256sum` и выкладка артефактом `binaries`. Штамп проверяется
исполнением: контрольный бинарь обязан напечатать текущий коммит, иначе
шаг падает - глазомер не считается.

## Тег релиза

1. Дерево чистое и зелёное: `make ci` (локально) или зелёный пуш CI.
2. Тег в формате semver: `git tag v0.X.Y && git push --tags`.
3. Сборка релиза: `make release` - VERSION подхватит тег сам.
4. Бинари из `bin/` (или артефакт CI `binaries`) и `sums.txt` выкладываются;
   на сервере сумма проверяется: `sha256sum -c sums.txt`.
5. Запись в `CHANGELOG.md` - что вошло, что сломано, как откатиться.

Первый тег ещё не ставился: репозиторий живёт на dev-версии, история
этапов - в логе коммитов. Момент первого тега - осознанное решение
«эта сборка идёт в чужие руки», а не побочный эффект этой инструкции.
