package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/search"
)

// engineFetch - это путь парсера: им идут команда parse, команда page и
// инструменты MCP parse/page_doc. Адрес там тоже диктует внешний вызывающий,
// поэтому барьер служебных сетей обязан наследоваться, а не оставаться
// достоянием одного инструмента fetch.
func TestEngineFetchAdapterBlocksPrivateTarget(t *testing.T) {
	eng, _, err := search.Build(search.DeepConfig{Transport: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = engineFetch{eng}.FetchURL(ctx, "http://127.0.0.1:18099/")
	if !errors.Is(err, netx.ErrPrivateTarget) {
		t.Errorf("локальный адрес: ошибка %v, хочу %v", err, netx.ErrPrivateTarget)
	}
	_, err = engineFetch{eng}.FetchURL(ctx, "http://169.254.169.254/latest/meta-data/")
	if !errors.Is(err, netx.ErrPrivateTarget) {
		t.Errorf("адрес метаданных: ошибка %v, хочу %v", err, netx.ErrPrivateTarget)
	}
	_, err = engineFetch{eng}.FetchURL(ctx, "file:///C:/Windows/win.ini")
	if !errors.Is(err, netx.ErrBadScheme) {
		t.Errorf("file: ошибка %v, хочу %v", err, netx.ErrBadScheme)
	}
}

// Явный переключатель снимает барьер, и запрос уходит в транспорт: локальный
// стенд разработчика обязан оставаться достижимым тем же путём.
func TestEngineFetchAdapterHonoursAllowPrivate(t *testing.T) {
	eng, _, err := search.Build(search.DeepConfig{Transport: "direct", AllowPrivateTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = engineFetch{eng}.FetchURL(ctx, "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("запрос на закрытый порт прошёл")
	}
	if errors.Is(err, netx.ErrPrivateTarget) || errors.Is(err, netx.ErrBadScheme) {
		t.Errorf("переключатель не снял барьер: %v", err)
	}
}

// Адаптер без ядра по-прежнему сообщает про инициализацию, а не про адрес: две
// разные поломки обязаны различаться в тексте ошибки.
func TestEngineFetchAdapterWithoutEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := engineFetch{}.FetchURL(ctx, "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("адаптер без ядра прошёл")
	}
	if errors.Is(err, netx.ErrPrivateTarget) {
		t.Errorf("получена причина барьера вместо причины про ядро: %v", err)
	}
}

// Команда classify берёт адрес из командной строки и идёт тем же вызовом
// Engine.FetchURL, поэтому барьер обязан действовать и там.
func TestClassifyRefusesPrivateTarget(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")

	stdout, stderr, code := runMainSplit(t, "classify", "--no-tor", "http://127.0.0.1:18099/")
	if code == 0 {
		t.Fatalf("код возврата 0: классификация служебного адреса прошла (stdout %s)", firstN(stdout, 200))
	}
	combined := stdout + stderr
	if !strings.Contains(combined, "служебной или приватной сети") {
		t.Errorf("причина не названа: stdout=%s stderr=%s", firstN(stdout, 200), firstN(stderr, 200))
	}
	if strings.Contains(stdout, "тип: ") {
		t.Errorf("вердикт напечатан при отказе: %s", firstN(stdout, 200))
	}
}

// Переменная окружения снимает барьер на всём пути команды, а не только внутри
// ядра: этим прогоном проверяется связь config -> DeepConfig -> Engine, которую
// unit-тесты адаптера не видят.
func TestClassifyHonoursAllowPrivateEnv(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", t.TempDir())
	t.Setenv("VOIDSEARCH_TOR", "off")
	t.Setenv("VOIDSEARCH_TRANSPORT", "direct")
	t.Setenv("VOIDSEARCH_BACKUP_DIR", "")
	t.Setenv("VOIDSEARCH_ALLOW_PRIVATE", "1")

	stdout, stderr, _ := runMainSplit(t, "classify", "--no-tor", "http://127.0.0.1:1/")
	if strings.Contains(stdout+stderr, "служебной или приватной сети") {
		t.Errorf("переменная не дошла до ядра: %s", firstN(stderr, 300))
	}
}
