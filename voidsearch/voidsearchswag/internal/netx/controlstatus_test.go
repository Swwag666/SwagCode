package netx

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeControl - минимальный tor control-сервер для тестов.
//
// Протокол воспроизводится не приблизительно, а так, как его ожидает
// tornago.ControlAuthFromTor: эта функция обязательно ищет в ответе
// PROTOCOLINFO строку COOKIEFILE="...", читает указанный файл с диска и
// аутентифицируется его hex-содержимым. Фейк без cookie-файла приводит к
// «control-port-file missing from PROTOCOLINFO» и бесконечным повторам до
// дедлайна, поэтому dial не проходит вовсе - и тесты, которые должны проверять
// отказ GETINFO или аутентификации, зеленеют по неверной причине.
type fakeControl struct {
	ln      net.Listener
	addr    string
	version string
	infoOK  bool
	authOK  bool
	cookie  []byte

	mu         sync.Mutex
	cookiePath string
	requests   []string
}

// newFakeControl поднимает фейковый control-порт. version - что вернуть на
// GETINFO version; authOK - принимать ли AUTHENTICATE; infoOK - отвечать ли
// успешно на GETINFO.
func newFakeControl(t *testing.T, version string, authOK, infoOK bool) *fakeControl {
	t.Helper()

	// Cookie-файл обязателен: PROTOCOLINFO ссылается на него путём, и
	// ControlAuthFromTor читает его содержимое.
	cookie := []byte("0123456789abcdef0123456789abcdef")
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "control_auth_cookie")
	if err := os.WriteFile(cookiePath, cookie, 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeControl{
		ln:      ln,
		addr:    ln.Addr().String(),
		version: version,
		authOK:  authOK,
		infoOK:  infoOK,
		cookie:  cookie,
	}
	f.setCookiePath(cookiePath)
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeControl) setCookiePath(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cookiePath = p
}

func (f *fakeControl) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeControl) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		f.record(line)

		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "PROTOCOLINFO"):
			f.mu.Lock()
			cookiePath := f.cookiePath
			f.mu.Unlock()
			w.WriteString("250-PROTOCOLINFO 1\r\n")
			w.WriteString(`250-AUTH METHODS=COOKIE COOKIEFILE="` + cookiePath + `"` + "\r\n")
			w.WriteString("250-VERSION Tor=\"0.4.9.12\"\r\n")
			w.WriteString("250 OK\r\n")
		case strings.HasPrefix(up, "AUTHENTICATE"):
			if !f.authOK {
				w.WriteString("515 Authentication failed\r\n")
				break
			}
			// Проверяем, что токен действительно hex-содержимое cookie-файла.
			// Без этого фейк принимал бы любую аутентификацию и тест на отказ
			// не отличал бы «аутентификация не прошла» от «сервер сломан».
			token := strings.TrimSpace(strings.TrimPrefix(up, "AUTHENTICATE"))
			if strings.EqualFold(token, hex.EncodeToString(f.cookie)) || token == "" {
				w.WriteString("250 OK\r\n")
			} else {
				w.WriteString("515 Authentication failed\r\n")
			}
		case strings.HasPrefix(up, "GETINFO"):
			if !f.infoOK {
				w.WriteString("552 Unrecognized key\r\n")
				break
			}
			if strings.Contains(up, "VERSION") {
				w.WriteString("250-version=" + f.version + "\r\n")
				w.WriteString("250 OK\r\n")
				break
			}
			w.WriteString("250 OK\r\n")
		case strings.HasPrefix(up, "SIGNAL NEWNYM"):
			w.WriteString("250 OK\r\n")
		case strings.HasPrefix(up, "QUIT"):
			w.WriteString("250 closing connection\r\n")
			w.Flush()
			return
		default:
			w.WriteString("510 Unrecognized command\r\n")
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func (f *fakeControl) record(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, line)
}

// seen сообщает, приходила ли команда с таким началом.
func (f *fakeControl) seen(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(strings.ToUpper(r), want) {
			return true
		}
	}
	return false
}

// newRotator собирает externalRotator, указывающий на фейковый control.
func newRotator(f *fakeControl) *externalRotator {
	return &externalRotator{
		socksAddr: f.addr,
		ctrlAddr:  f.addr,
		timeout:   3 * time.Second,
	}
}

// requireDialSucceeded утверждает, что control-соединение действительно
// установлено. Без этой проверки тесты на отказ GETINFO или аутентификации
// зеленеют и тогда, когда dial упал целиком, - то есть не доказывают ничего.
func requireDialSucceeded(t *testing.T, f *fakeControl, tor *externalRotator) {
	t.Helper()
	tor.mu.Lock()
	ctrl := tor.control()
	tor.mu.Unlock()
	if ctrl == nil {
		t.Fatal("dial control не прошёл, тест не проверяет то, что заявлено")
	}
	if !f.seen("PROTOCOLINFO") {
		t.Fatal("сервер не получил PROTOCOLINFO, рукопожатие не состоялось")
	}
}

func TestControlStatusWithoutAddr(t *testing.T) {
	// Адрес control-порта не задан: это состояние конфигурации, а не сбой,
	// поэтому формулировка обязана отличаться от «не отвечает».
	tor := &externalRotator{socksAddr: "127.0.0.1:9050", ctrlAddr: "", timeout: 2 * time.Second}
	got := tor.controlStatus()
	if got != "не задан" {
		t.Errorf("controlStatus = %q, ожидала %q", got, "не задан")
	}
}

func TestControlStatusDialFailure(t *testing.T) {
	// Адрес есть, но порт закрыт: соединение не устанавливается.
	tor := &externalRotator{
		socksAddr: "127.0.0.1:9050",
		ctrlAddr:  "127.0.0.1:1",
		timeout:   500 * time.Millisecond,
	}
	got := tor.controlStatus()
	if !strings.HasPrefix(got, "не отвечает") {
		t.Errorf("controlStatus = %q, ожидала начало %q", got, "не отвечает")
	}
	if strings.Contains(got, "подключён") {
		t.Errorf("недоступный control назван подключённым: %q", got)
	}
}

func TestControlStatusVerifiedConnected(t *testing.T) {
	f := newFakeControl(t, "0.4.9.12", true, true)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)

	got := tor.controlStatus()
	if !strings.HasPrefix(got, "подключён") {
		t.Fatalf("controlStatus = %q, ожидала начало %q", got, "подключён")
	}
	if !strings.Contains(got, "0.4.9.12") {
		t.Errorf("версия не попала в статус: %q", got)
	}
	// Статус обязан опираться на круговой запрос, а не на хендл.
	if !f.seen("GETINFO") {
		t.Error("статус получен без GETINFO, значит проверка не настоящая")
	}
}

// TestControlStatusDeadChannelNotReportedConnected - главная регрессия дефекта:
// шестикратный таймаут NEWNYM при строке «control=подключён».
//
// Канал принимает соединение и проходит аутентификацию, но на GETINFO отвечает
// ошибкой. Ненулевой хендл здесь есть, поэтому прежняя проверка
// boolWord(t.ctrl != nil) напечатала бы «подключён» и соврала.
func TestControlStatusDeadChannelNotReportedConnected(t *testing.T) {
	f := newFakeControl(t, "0.4.9.12", true, false)
	tor := newRotator(f)
	defer tor.Close()

	// Критично: dial обязан пройти, иначе тест проверял бы не отказ GETINFO, а
	// неработающее соединение, и зеленел бы по неверной причине.
	requireDialSucceeded(t, f, tor)
	if !f.seen("AUTHENTICATE") {
		t.Fatal("аутентификация не выполнялась, тест не проверяет заявленное")
	}

	got := tor.controlStatus()
	if strings.HasPrefix(got, "подключён") {
		t.Errorf("мёртвый канал назван подключённым: %q", got)
	}
	if !strings.HasPrefix(got, "не отвечает") {
		t.Errorf("controlStatus = %q, ожидала начало %q", got, "не отвечает")
	}
	if !strings.Contains(got, "(") {
		t.Errorf("статус не объясняет причину: %q", got)
	}
	if !f.seen("GETINFO") {
		t.Error("проверка не дошла до GETINFO")
	}
}

func TestControlStatusDropsDeadHandle(t *testing.T) {
	// После неудачной проверки битый хендл обязан сбрасываться: иначе мёртвый
	// сокет пережил бы отчёт и следующая команда NEWNYM упала бы мгновенно на
	// уже непригодном соединении с истёкшим абсолютным дедлайном.
	f := newFakeControl(t, "0.4.9.12", true, false)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)
	tor.controlStatus()

	tor.mu.Lock()
	ctrl := tor.ctrl
	tor.mu.Unlock()
	if ctrl != nil {
		t.Error("битый хендл не сброшен после неудачной проверки")
	}
}

func TestControlStatusAuthFailure(t *testing.T) {
	// Аутентификация не проходит: dialControl обязан вернуть ошибку, и статус
	// не может утверждать подключение.
	f := newFakeControl(t, "0.4.9.12", false, true)
	tor := newRotator(f)
	defer tor.Close()

	got := tor.controlStatus()
	if strings.HasPrefix(got, "подключён") {
		t.Errorf("control с отказавшей аутентификацией назван подключённым: %q", got)
	}
	if !strings.HasPrefix(got, "не отвечает") {
		t.Errorf("controlStatus = %q, ожидала начало %q", got, "не отвечает")
	}
	// Рукопожатие началось, но аутентификация отвергнута: это отличает случай
	// от полностью недоступного порта.
	if !f.seen("PROTOCOLINFO") {
		t.Error("сервер не получил PROTOCOLINFO, значит проверен не отказ аутентификации")
	}
	if !f.seen("AUTHENTICATE") {
		t.Error("сервер не получил AUTHENTICATE, значит проверен не отказ аутентификации")
	}
}

func TestControlStatusEmptyVersion(t *testing.T) {
	// Пустая версия - тоже не «подключён»: канал формально жив, но tor не отдал
	// версию, то есть данные не возвращаются.
	//
	// Проверяется через ошибку, а не через пустую строку: tornago.getInfo
	// возвращает «key not found in GETINFO response» всякий раз, когда
	// разобранный результат пуст, поэтому комбинация (пустая строка, nil)
	// со стороны зависимости невозможна. Отдельная ветка на пустую строку в
	// controlStatus была бы недостижимым кодом и убрана намеренно.
	f := newFakeControl(t, "", true, true)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)

	got := tor.controlStatus()
	if strings.HasPrefix(got, "подключён") {
		t.Errorf("пустая версия принята за живое управление: %q", got)
	}
	if !strings.HasPrefix(got, "не отвечает") {
		t.Errorf("controlStatus = %q, ожидала начало %q", got, "не отвечает")
	}
	if !strings.Contains(got, "GETINFO") {
		t.Errorf("статус не называет причину: %q", got)
	}
}

func TestControlStatusIsIdempotent(t *testing.T) {
	// Повторный вызов после успешной проверки не обязан ломать состояние:
	// хендл живой, GETINFO проходит снова.
	f := newFakeControl(t, "0.4.9.12", true, true)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)
	first := tor.controlStatus()
	second := tor.controlStatus()
	if first != second {
		t.Errorf("статус нестабилен: %q затем %q", first, second)
	}
	if !strings.HasPrefix(first, "подключён") {
		t.Errorf("ожидала успешный статус, получено %q", first)
	}
}

func TestControlStatusConcurrentSafe(t *testing.T) {
	// controlStatus берёт mutex и вызывает control() под ним, поэтому
	// параллельные вызовы не должны гонять состояние. Проверяется на живом
	// фейке: если бы блокировка отсутствовала, гонку поймал бы и -race в CI.
	f := newFakeControl(t, "0.4.9.12", true, true)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)

	const n = 8
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = tor.controlStatus()
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("controlStatus завис при параллельных вызовах")
	}

	for i, got := range results {
		if !strings.HasPrefix(got, "подключён") {
			t.Errorf("горизонт %d: статус %q", i, got)
		}
	}
}

func TestControlStatusMatchesHealthy(t *testing.T) {
	// Статус и Healthy обязаны соглашаться: обе проверки опираются на
	// GETINFO("version"). Расхождение вернуло бы исходный дефект - одна строка
	// говорит «подключён», другая считает транспорт мёртвым.
	f := newFakeControl(t, "0.4.9.12", true, true)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)
	status := tor.controlStatus()
	healthy := tor.Healthy()
	if strings.HasPrefix(status, "подключён") != healthy {
		t.Errorf("расхождение: controlStatus=%q, Healthy=%v", status, healthy)
	}
}

func TestControlStatusDisagreesWithHealthyOnDeadChannel(t *testing.T) {
	// Обратный случай: GETINFO отказывает. Статус не должен утверждать
	// подключение. Healthy при этом может вернуть true по запасной ветке
	// (доступность socks-порта), поэтому здесь проверяется только статус -
	// именно он был источником ложного «подключён».
	f := newFakeControl(t, "0.4.9.12", true, false)
	tor := newRotator(f)
	defer tor.Close()

	requireDialSucceeded(t, f, tor)
	status := tor.controlStatus()
	if strings.HasPrefix(status, "подключён") {
		t.Errorf("при отказе GETINFO статус утверждает подключение: %q", status)
	}
}
