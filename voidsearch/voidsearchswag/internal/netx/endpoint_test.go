package netx

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// useTempDataDir переводит data-каталог во временный, чтобы тесты не трогали
// боевой ~/.voidsearchswag и не конфликтовали с живым демоном.
func useTempDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VOIDSEARCH_DATA_DIR", dir)
	return dir
}

func TestTorEndpointPathFollowsDataDir(t *testing.T) {
	dir := useTempDataDir(t)
	got := TorEndpointPath()
	if !strings.HasPrefix(got, dir) {
		t.Errorf("путь %q вне data-каталога %q", got, dir)
	}
	if !strings.HasSuffix(got, "tor-endpoint.json") {
		t.Errorf("неожиданное имя файла: %q", got)
	}
}

func TestSaveLoadTorEndpointRoundTrip(t *testing.T) {
	useTempDataDir(t)

	path, err := SaveTorEndpoint("127.0.0.1:9050", "127.0.0.1:9051")
	if err != nil {
		t.Fatal(err)
	}
	if path != TorEndpointPath() {
		t.Errorf("путь %q != %q", path, TorEndpointPath())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл не создан: %v", err)
	}

	ep, err := LoadTorEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if ep.Socks != "127.0.0.1:9050" || ep.Control != "127.0.0.1:9051" {
		t.Errorf("адреса потеряны: %+v", ep)
	}
	if ep.PID != os.Getpid() {
		t.Errorf("PID %d, ожидала %d", ep.PID, os.Getpid())
	}
	if ep.Started.IsZero() {
		t.Error("время старта не записано")
	}
}

func TestSaveTorEndpointOverwrites(t *testing.T) {
	useTempDataDir(t)
	if _, err := SaveTorEndpoint("127.0.0.1:1111", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveTorEndpoint("127.0.0.1:2222", ""); err != nil {
		t.Fatal(err)
	}
	ep, err := LoadTorEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if ep.Socks != "127.0.0.1:2222" {
		t.Errorf("старый эндпоинт не перезаписан: %+v", ep)
	}
	// Временный файл не должен оставаться: иначе каталог зарастает мусором.
	if _, err := os.Stat(TorEndpointPath() + ".tmp"); !os.IsNotExist(err) {
		t.Error("временный файл не убран")
	}
}

func TestLoadTorEndpointMissingIsError(t *testing.T) {
	useTempDataDir(t)
	if _, err := LoadTorEndpoint(); err == nil {
		t.Error("отсутствие файла не ошибка")
	} else if !os.IsNotExist(err) {
		t.Errorf("ошибка не про отсутствие файла: %v", err)
	}
}

func TestLoadTorEndpointRejectsBrokenJSON(t *testing.T) {
	dir := useTempDataDir(t)
	if err := os.WriteFile(dir+"/tor-endpoint.json", []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTorEndpoint(); err == nil {
		t.Error("битый JSON принят")
	}
}

func TestLoadTorEndpointRejectsEmptySocks(t *testing.T) {
	// Эндпоинт без socks-адреса бесполезен: подключаться некуда. Принять его
	// значит позже упасть в самом неожиданном месте.
	dir := useTempDataDir(t)
	data, _ := json.Marshal(TorEndpoint{Socks: "  ", Control: "127.0.0.1:9051"})
	if err := os.WriteFile(dir+"/tor-endpoint.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTorEndpoint(); err == nil {
		t.Error("пустой socks принят")
	}
}

func TestRemoveTorEndpoint(t *testing.T) {
	useTempDataDir(t)
	if _, err := SaveTorEndpoint("127.0.0.1:9050", ""); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTorEndpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(TorEndpointPath()); !os.IsNotExist(err) {
		t.Error("файл не удалён")
	}
	// Удаление уже отсутствующего файла не должно быть ошибкой: демон может
	// останавливаться дважды или файл мог убрать другой процесс.
	if err := RemoveTorEndpoint(); err != nil {
		t.Errorf("повторное удаление: %v", err)
	}
}

func TestLiveTorEndpointRequiresLivePort(t *testing.T) {
	useTempDataDir(t)

	// Мёртвый порт: файл есть, но демона за ним нет. Доверять такому файлу
	// значит получить серию таймаутов вместо быстрого старта.
	if _, err := SaveTorEndpoint("127.0.0.1:1", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := LiveTorEndpoint(500 * time.Millisecond); ok {
		t.Error("мёртвый порт признан живым")
	}

	// Живой слушатель обязан находиться.
	addr, stop := liveSock(t)
	defer stop()
	if _, err := SaveTorEndpoint(addr, ""); err != nil {
		t.Fatal(err)
	}
	ep, ok := LiveTorEndpoint(2 * time.Second)
	if !ok {
		t.Fatal("живой эндпоинт не найден")
	}
	if ep.Socks != addr {
		t.Errorf("адрес %q, ожидала %q", ep.Socks, addr)
	}
}

func TestLiveTorEndpointNoFile(t *testing.T) {
	useTempDataDir(t)
	if _, ok := LiveTorEndpoint(time.Second); ok {
		t.Error("эндпоинт найден без файла")
	}
}

func TestLiveTorEndpointDefaultTimeout(t *testing.T) {
	// Нулевой таймаут не должен означать «ждать вечно»: подставляем дефолт.
	useTempDataDir(t)
	if _, err := SaveTorEndpoint("127.0.0.1:1", ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := LiveTorEndpoint(0)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("мёртвый порт признан живым")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("нулевой таймаут завис")
	}
}

func TestStartTorReusesPublishedEndpoint(t *testing.T) {
	useTempDataDir(t)
	addr, stop := liveSock(t)
	defer stop()

	if _, err := SaveTorEndpoint(addr, ""); err != nil {
		t.Fatal(err)
	}
	// Бинарь намеренно не задан: переиспользование живого демона не должно
	// требовать установленного tor.exe.
	r, err := StartTor(Config{Logger: nopLogger{}})
	if err != nil {
		t.Fatalf("StartTor не подключился к опубликованному эндпоинту: %v", err)
	}
	defer r.Close()
	if r.Kind() != "tor" {
		t.Errorf("Kind=%q", r.Kind())
	}
	if got, want := r.TransportSpec(), "socks5://"+addr; got != want {
		t.Errorf("TransportSpec=%q, ожидала %q", got, want)
	}
}

func TestStartTorNoReuseSkipsEndpoint(t *testing.T) {
	useTempDataDir(t)
	addr, stop := liveSock(t)
	defer stop()
	if _, err := SaveTorEndpoint(addr, ""); err != nil {
		t.Fatal(err)
	}

	// tord обязан игнорировать собственный эндпоинт и поднимать новый демон,
	// иначе команда подключается сама к себе.
	_, err := StartTor(Config{TorNoReuse: true, TorBinary: "   ", Logger: nopLogger{}})
	if err == nil {
		t.Fatal("с TorNoReuse StartTor всё равно подключился к эндпоинту")
	}
	if !strings.Contains(err.Error(), "tor-бинарь") {
		t.Errorf("ошибка не про отсутствующий бинарь: %v", err)
	}
}

func TestStartTorFallsBackWhenEndpointDead(t *testing.T) {
	useTempDataDir(t)
	// Файл остался от упавшего процесса: должен быть откат к собственному
	// демону, а не ошибка подключения.
	if _, err := SaveTorEndpoint("127.0.0.1:1", ""); err != nil {
		t.Fatal(err)
	}
	_, err := StartTor(Config{TorBinary: "   ", Logger: nopLogger{}})
	if err == nil {
		t.Fatal("StartTor прошёл без бинаря и без живого эндпоинта")
	}
	if !strings.Contains(err.Error(), "tor-бинарь") {
		t.Errorf("откат не сработал, ошибка: %v", err)
	}
}
