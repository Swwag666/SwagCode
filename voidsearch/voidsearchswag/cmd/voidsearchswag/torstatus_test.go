package main

import (
	"context"
	"strings"
	"testing"

	"voidsearchswag/internal/netx"
)

// Ниже - отдельные заглушки, а не переиспользование stubRotator из tord_test.go:
// тот реализует ControlAddr и тем самым покрывает только одну из трёх ветвей
// torStatusLine. Здесь каждая заглушка соответствует ровно одной ветви, чтобы
// тест доказывал выбор ветви, а не совпадал с ней случайно.

// tsBaseRotator реализует только обязательный netx.Rotator: ни ControlStatus,
// ни ControlAddr. Так ведут себя прямой транспорт и прокси-пул.
type tsBaseRotator struct {
	kind string
	spec string
}

func (r *tsBaseRotator) Kind() string                 { return r.kind }
func (r *tsBaseRotator) TransportSpec() string        { return r.spec }
func (r *tsBaseRotator) Rotate(context.Context) error { return nil }
func (r *tsBaseRotator) Healthy() bool                { return true }
func (r *tsBaseRotator) Close() error                 { return nil }

// tsStatusRotator добавляет ControlStatus - как externalRotator.
type tsStatusRotator struct {
	tsBaseRotator
	status string
}

func (r *tsStatusRotator) ControlStatus() string { return r.status }

// tsAddrRotator добавляет только ControlAddr - как torRotator собственного
// демона, который знает адрес, но не проверяет канал круговым запросом.
type tsAddrRotator struct {
	tsBaseRotator
	addr string
}

func (r *tsAddrRotator) ControlAddr() string { return r.addr }

// tsBothRotator реализует оба опциональных метода: проверяется приоритет.
type tsBothRotator struct {
	tsBaseRotator
	status string
	addr   string
}

func (r *tsBothRotator) ControlStatus() string { return r.status }
func (r *tsBothRotator) ControlAddr() string   { return r.addr }

func TestTorStatusLineWithVerifiedControl(t *testing.T) {
	// Ротатор, умеющий проверять канал, обязан дать именно проверенный статус.
	r := &tsStatusRotator{
		tsBaseRotator: tsBaseRotator{kind: "tor", spec: "socks5://127.0.0.1:9050"},
		status:        "подключён, tor 0.4.9.12",
	}
	got := torStatusLine(r)
	if !strings.Contains(got, "control подключён, tor 0.4.9.12") {
		t.Errorf("проверенный статус не попал в строку: %q", got)
	}
	if !strings.Contains(got, "socks5://127.0.0.1:9050") {
		t.Errorf("транспорт потерян: %q", got)
	}
}

func TestTorStatusLineDeadControlNotReportedConnected(t *testing.T) {
	// Главная регрессия: мёртвый канал не должен превращаться в «подключён».
	r := &tsStatusRotator{
		tsBaseRotator: tsBaseRotator{kind: "tor", spec: "socks5://127.0.0.1:9050"},
		status:        "не отвечает (i/o timeout)",
	}
	got := torStatusLine(r)
	if strings.Contains(got, "control подключён") {
		t.Errorf("мёртвый канал назван подключённым: %q", got)
	}
	if !strings.Contains(got, "не отвечает") {
		t.Errorf("причина не показана: %q", got)
	}
}

func TestTorStatusLineWithAddrOnly(t *testing.T) {
	// Ротатор знает адрес, но не проверяет канал: строка обязана показывать
	// адрес, а не утверждать подключение.
	r := &tsAddrRotator{
		tsBaseRotator: tsBaseRotator{kind: "tor", spec: "socks5://127.0.0.1:9050"},
		addr:          "127.0.0.1:9051",
	}
	got := torStatusLine(r)
	if !strings.Contains(got, "control 127.0.0.1:9051") {
		t.Errorf("адрес control не показан: %q", got)
	}
	if strings.Contains(got, "подключён") {
		t.Errorf("адрес принят за проверенное подключение: %q", got)
	}
}

func TestTorStatusLineWithEmptyAddr(t *testing.T) {
	r := &tsAddrRotator{
		tsBaseRotator: tsBaseRotator{kind: "tor", spec: "socks5://127.0.0.1:9050"},
		addr:          "",
	}
	got := torStatusLine(r)
	if !strings.Contains(got, "control не задан") {
		t.Errorf("пустой адрес не объяснён: %q", got)
	}
}

func TestTorStatusLineWithoutControl(t *testing.T) {
	// Ротатор не реализует ни один из опциональных интерфейсов: прямой
	// транспорт или прокси-пул. Отсутствие метода не ошибка.
	r := &tsBaseRotator{kind: "direct", spec: "direct"}
	got := torStatusLine(r)
	if !strings.Contains(got, "control не задан") {
		t.Errorf("для транспорта без control строка %q", got)
	}
	if !strings.Contains(got, "direct") {
		t.Errorf("вид транспорта потерян: %q", got)
	}
}

func TestTorStatusLinePrefersVerifiedStatusOverAddr(t *testing.T) {
	// Ротатор с обоими методами обязан давать проверенный статус: адрес сам по
	// себе ничего не говорит о пригодности канала.
	r := &tsBothRotator{
		tsBaseRotator: tsBaseRotator{kind: "tor", spec: "socks5://127.0.0.1:9050"},
		status:        "не отвечает (соединение разорвано)",
		addr:          "127.0.0.1:9051",
	}
	got := torStatusLine(r)
	if !strings.Contains(got, "не отвечает") {
		t.Errorf("проверенный статус не предпочтён адресу: %q", got)
	}
	if strings.Contains(got, "9051") {
		t.Errorf("адрес подмешан вместо проверенного статуса: %q", got)
	}
}

func TestTorStatusLineAlwaysMentionsTransport(t *testing.T) {
	// Строка входит в диагностический отчёт, поэтому транспорт обязан быть в
	// ней при любом состоянии control: иначе не понять, где именно проблема.
	spec := "socks5://127.0.0.1:9050"
	for _, status := range []string{"подключён, tor 0.4.9.12", "не задан", "не отвечает (сбой)"} {
		r := &tsStatusRotator{
			tsBaseRotator: tsBaseRotator{kind: "tor", spec: spec},
			status:        status,
		}
		got := torStatusLine(r)
		if !strings.Contains(got, spec) {
			t.Errorf("транспорт не упомянут при статусе %q: %q", status, got)
		}
	}
}

func TestTorStatusLineStubsSatisfyRotator(t *testing.T) {
	// Заглушки обязаны удовлетворять netx.Rotator, иначе torStatusLine
	// принимала бы их только благодаря структурной типизации теста.
	var _ netx.Rotator = &tsBaseRotator{}
	var _ netx.Rotator = &tsStatusRotator{}
	var _ netx.Rotator = &tsAddrRotator{}
	var _ netx.Rotator = &tsBothRotator{}
}
