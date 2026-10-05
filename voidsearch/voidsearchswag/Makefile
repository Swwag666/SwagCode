BIN := voidsearchswag
ifeq ($(OS),Windows_NT)
BIN := voidsearchswag.exe
endif

# Релизная мета заливается линкером (см. var version в main.go). VERSION
# можно задать руками: make VERSION=v0.2.0 cross. Без явного VERSION берётся
# git describe: последний тег, а без тегов - короткий хеш коммита, и суффикс
# -dirty честно показывает непрокоммиченное дерево. Вне git-репозитория -
# dev.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.buildCommit=$(COMMIT) -X main.buildDate=$(DATE)

.PHONY: setup build test race vet run tidy clean cover cover-total lint cross ci fmt-check release

setup:
	go run ./cmd/voidsearchswag setup

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/voidsearchswag

test:
	go test ./...

# -race требует cgo, а cgo на Windows нужен gcc-совместимый компилятор:
# MSVC cl.exe не подходит, cgo вызывает драйвер в gcc-стиле.
#
# Сам проект остаётся CGO-free. modernc.org/sqlite - чистый Go и одинаково
# работает при CGO_ENABLED=0 и CGO_ENABLED=1, поэтому включение cgo ради
# детектора гонок не тянет за собой ни одной Си-зависимости в бинарь: боевая
# сборка по-прежнему идёт через `make build` с CGO_ENABLED=0.
#
# На машине без gcc/clang цель падает с внятным сообщением вместо невнятной
# ошибки компоновщика. В CI детектор запускается всегда: ubuntu-latest несёт
# gcc из коробки, поэтому гонки проверяются на каждый пуш независимо от того,
# какой тулчейн стоит у разработчика локально.
race:
	CGO_ENABLED=1 go test -race -count=1 ./...

# cross собирает боевой бинарь под все целевые платформы с той же залитой
# версией, что и build: релиз без версии неотличим от самосбора.
# Проверка нужна, потому что сборка только под windows/amd64 не ловит
# платформенные ветки: имена бинарей tor, разделители путей и поведение
# os.Stat расходятся.
cross:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN).windows-amd64 ./cmd/voidsearchswag
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN).linux-amd64   ./cmd/voidsearchswag
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN).linux-arm64   ./cmd/voidsearchswag
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN).darwin-arm64  ./cmd/voidsearchswag

# release - полный релизный прогон: тесты, кросс-сборка и контрольные суммы
# рядом с бинарями (sums.txt в bin/). Тег кати отдельным шагом:
# git tag v0.X.Y && git push --tags, после чего VERSION берётся из тега сам.
release: test cross
	cd bin && sha256sum $(BIN).windows-amd64 $(BIN).linux-amd64 $(BIN).linux-arm64 $(BIN).darwin-arm64 > sums.txt
	@echo "собрано: $(VERSION) ($(COMMIT), $(DATE)); bin/sums.txt готов"

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt: не отформатировано"; echo "$$out"; exit 1; fi

run:
	go run ./cmd/voidsearchswag run

tidy:
	go mod tidy

clean:
ifeq ($(OS),Windows_NT)
	if exist bin rmdir /s /q bin
else
	rm -rf bin
endif

cover:
	go test ./... -cover

# cover-total печатает одно суммарное число покрытия. Нужно в CI как порог:
# по пакетам цифры скачут, а регресс всей кодовой базы видно по сумме.
cover-total:
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...

# ci - полный гейт в том порядке, в котором дёшево падать раньше:
# формат, статический анализ, тесты, гонки, кросс-сборка.
ci: fmt-check vet cover-total race cross
