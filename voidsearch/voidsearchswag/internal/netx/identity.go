package netx

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nao1215/tornago"
)

type Rotator interface {
	Kind() string
	TransportSpec() string
	Rotate(ctx context.Context) error
	Healthy() bool
	Close() error
}

type torRotator struct {
	mu       sync.Mutex
	proc     *tornago.TorProcess
	ctrl     *tornago.ControlClient
	proxyURL string
	lastNew  time.Time
	cooldown time.Duration
	timeout  time.Duration
	log      Logger
}

func StartTor(cfg Config) (Rotator, error) {
	// Внешний tor важнее собственного: если пользователь явно указал
	// socks-адрес, поднимать второй демон нельзя - это и лишние 25 секунд,
	// и борьба за tor-data/lock с уже работающим процессом.
	if strings.TrimSpace(cfg.TorSocksAddr) != "" {
		return attachTor(cfg)
	}
	// Явного адреса нет, но демон может быть уже поднят командой tord и
	// опубликован в файле эндпоинта. Проверка стоит миллисекунды и экономит
	// 20-25 секунд bootstrap на каждый вызов CLI. Файл от упавшего процесса
	// отсекается живой проверкой порта, поэтому откат к собственному демону
	// остаётся корректным.
	if !cfg.TorNoReuse {
		if ep, ok := LiveTorEndpoint(2 * time.Second); ok {
			cfg.TorSocksAddr = ep.Socks
			if cfg.TorControlAddr == "" {
				cfg.TorControlAddr = ep.Control
			}
			if cfg.Logger != nil {
				cfg.Logger.Infof("использую уже запущенный tor: socks=%s", ep.Socks)
			}
			if r, err := attachTor(cfg); err == nil {
				return r, nil
			} else if cfg.Logger != nil {
				cfg.Logger.Infof("живой tor не подключился (%v), поднимаю свой", err)
			}
		}
	}
	bin := strings.TrimSpace(cfg.TorBinary)
	if bin == "" {
		return nil, fmt.Errorf("tor-бинарь не найден: укажи VOIDSEARCH_TOR_BINARY или запусти setup")
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("tor-бинарь %s: %w", bin, err)
	}
	dataDir := cfg.TorDataDir
	if dataDir == "" {
		dataDir = DefaultTorDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("tor datadir: %w", err)
	}

	opts := []tornago.TorLaunchOption{
		tornago.WithTorBinary(bin),
		tornago.WithTorSocksAddr("127.0.0.1:0"),
		tornago.WithTorControlAddr("127.0.0.1:0"),
		tornago.WithTorDataDir(dataDir),
		tornago.WithTorStartupTimeout(cfg.timeoutOrDefault()),
	}
	if cfg.TorOwnProcess {
		opts = append(opts, tornago.WithTorExtraArgs("--__OwningControllerProcess", strconv.Itoa(os.Getpid())))
	}
	launch, err := tornago.NewTorLaunchConfig(opts...)
	if err != nil {
		return nil, fmt.Errorf("tor launch cfg: %w", err)
	}

	proc, err := tornago.StartTorDaemon(launch)
	if err != nil {
		return nil, fmt.Errorf("tor start: %w", err)
	}

	t := &torRotator{
		proc:     proc,
		proxyURL: "socks5://" + proc.SocksAddr(),
		cooldown: cfg.MinNewnymInterval,
		timeout:  30 * time.Second,
		log:      cfg.Logger,
	}
	if t.cooldown <= 0 {
		t.cooldown = 11 * time.Second
	}
	if err := t.connectControl(); err != nil {
		proc.Stop()
		return nil, err
	}
	if err := t.waitBootstrap(cfg.timeoutOrDefault()); err != nil {
		proc.Stop()
		return nil, err
	}
	t.logf("tor-демон поднят: socks=%s control=%s datadir=%s", proc.SocksAddr(), proc.ControlAddr(), dataDir)
	return t, nil
}

// waitBootstrap ждёт, пока tor построит цепи. StartTorDaemon ждёт только
// открытия портов, а это происходит задолго до готовности: socks-порт
// слушает сразу, но любой запрос через него до 100% падает по таймауту.
func (t *torRotator) waitBootstrap(timeout time.Duration) error {
	return waitBootstrapOn(context.Background(), t.ctrl, timeout, t.logf)
}

// waitBootstrapOn - общая реализация для собственного и внешнего tor. Один
// алгоритм на два ротатора: расхождение в ожидании готовности дало бы разные
// гарантии для одного и того же транспорта.
func waitBootstrapOn(parent context.Context, ctrl *tornago.ControlClient, timeout time.Duration, logf func(string, ...any)) error {
	if ctrl == nil {
		return fmt.Errorf("tor control не подключён")
	}
	deadline := time.Now().Add(timeout)
	backoff := 250 * time.Millisecond

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("tor не построил цепи за %v", timeout)
		}
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		phase, err := ctrl.GetInfo(ctx, "status/bootstrap-phase")
		cancel()

		if err == nil {
			if strings.Contains(phase, "PROGRESS=100") {
				logf("tor bootstrap: 100%%")
				return nil
			}
			if s := bootstrapPct(phase); s != "" {
				logf("tor bootstrap: %s", s)
			}
		} else if parent.Err() != nil {
			return parent.Err()
		}

		select {
		case <-parent.Done():
			return parent.Err()
		case <-time.After(backoff):
		}
		if backoff < 3*time.Second {
			backoff = backoff * 2
		}
	}
}

func bootstrapPct(phase string) string {
	i := strings.Index(phase, "PROGRESS=")
	if i < 0 {
		return ""
	}
	rest := phase[i+len("PROGRESS="):]
	j := strings.IndexAny(rest, " \t\r\n")
	if j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return ""
	}
	return rest + "%"
}

func (t *torRotator) connectControl() error {
	ctrl, err := dialControl(t.proc.ControlAddr(), t.timeout)
	if err != nil {
		return err
	}
	t.ctrl = ctrl
	return nil
}

// dropControl сбрасывает битое control-соединение. Дедлайн на сокете в Go
// абсолютный: после одного i/o timeout соединение непригодно навсегда, поэтому
// его надо выбросить, а не переиспользовать в надежде, что «само пройдёт».
func (t *torRotator) dropControl() {
	if t.ctrl != nil {
		t.ctrl.Close()
		t.ctrl = nil
	}
}

func (t *torRotator) Kind() string { return "tor" }

// ControlAddrer реализуют ротаторы, которые знают адрес control-порта.
// Интерфейс опциональный: внешний tor может быть подключён только по socks,
// и требовать от него control значило бы сломать переиспользование.
type ControlAddrer interface {
	ControlAddr() string
}

func (t *torRotator) ControlAddr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc == nil {
		return ""
	}
	return t.proc.ControlAddr()
}

func (t *torRotator) TransportSpec() string { return t.proxyURL }

// ControlStatus возвращает проверенное состояние control-канала собственного
// демона.
//
// Метод нужен здесь, а не только у externalRotator, потому что основной путь
// onioncheck идёт именно через собственный демон. Без него диагностика
// печатала адрес control-порта, и строка «control 127.0.0.1:61867» читалась как
// «управление доступно» ровно в тот момент, когда шесть команд NEWNYM подряд
// уходили в таймаут. Наличие адреса ничего не говорит о пригодности канала.
//
// Живой прогон воспроизвёл дефект: «tor newnym: ... i/o timeout» и пять
// «ротация не удалась: tor control не подключён» при статусной строке, которая
// по-прежнему показывала адрес как ни в чём не бывало.
//
// Проверка та же, что у externalRotator: круговой GETINFO("version"). Возвращает
// «не задан», «не отвечает (причина)» или «подключён, tor <версия>».
func (t *torRotator) ControlStatus() string {
	t.mu.Lock()
	addr := ""
	if t.proc != nil {
		addr = t.proc.ControlAddr()
	}
	if addr == "" {
		t.mu.Unlock()
		return "не задан"
	}
	// Соединение пересоздаётся при необходимости: после сбоя команды dropControl
	// обнуляет t.ctrl, и без переподключения статус навсегда остался бы
	// «не отвечает» даже на живом демоне.
	var err error
	if t.ctrl == nil {
		err = t.connectControl()
	}
	ctrl := t.ctrl
	timeout := t.timeout
	t.mu.Unlock()

	if err != nil || ctrl == nil {
		if err == nil {
			err = fmt.Errorf("соединение не установлено")
		}
		return "не отвечает (" + err.Error() + ")"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	v, gerr := ctrl.GetInfo(ctx, "version")
	if gerr != nil {
		// Канал бит: сбрасываем под блокировкой, чтобы следующий вызов
		// переподключился. Дедлайн на сокете в Go абсолютный, поэтому
		// однажды истёкшее соединение навсегда отвечает i/o timeout,
		// оставаясь ненулевым.
		t.mu.Lock()
		t.dropControl()
		t.mu.Unlock()
		return "не отвечает (" + gerr.Error() + ")"
	}
	// Пустая версия невозможна при nil-ошибке: tornago.getInfo возвращает
	// «key not found in GETINFO response» всякий раз, когда результат пуст.
	return "подключён, tor " + strings.TrimSpace(v)
}

// ensureControlLocked устанавливает control-соединение, если его нет.
// Вызывается под t.mu.
//
// Вынесено из Rotate ради проверяемости: address передаётся параметром, поэтому
// логику переподключения можно покрыть тестом без настоящего tor-процесса.
// Поля tornago.TorProcess неэкспортированы, и сконструировать процесс,
// указывающий на тестовый control-порт, из теста невозможно.
//
// Пустой адрес означает, что процесса нет и переподключаться некуда: rotator
// создаётся до поднятия tor, и connectControl разыменовывает t.proc, поэтому
// без этой проверки Rotate паниковал на ещё не подключённом ротаторе.
func (t *torRotator) ensureControlLocked(addr string) error {
	if t.ctrl != nil {
		return nil
	}
	if addr == "" {
		return fmt.Errorf("процесс tor не поднят")
	}
	ctrl, err := dialControl(addr, t.timeout)
	if err != nil {
		return err
	}
	t.ctrl = ctrl
	t.logf("tor control переподключен")
	return nil
}

func (t *torRotator) Rotate(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Отсутствие хендла - не повод сдаваться, а повод переподключиться.
	//
	// Прежняя версия здесь немедленно возвращала «tor control не подключён».
	// При этом ветка повтора ниже вызывает dropControl(), который обнуляет
	// t.ctrl, и после одного сбоя NEWNYM каждая последующая ротация до конца
	// жизни процесса падала мгновенно, даже не пытаясь установить соединение.
	// Получалась постоянная защёлка: один медленный NEWNYM выводил управление
	// из строя навсегда. В живом прогоне это выглядело как шесть подряд
	// «ротация не удалась: tor control не подключён» при живом демоне, чей
	// control-порт отвечал на GETINFO.
	//
	// externalRotator.Rotate вёл себя иначе и правильно: брал соединение через
	// control() с ленивым переподключением. Здесь теперь та же схема.
	addr := ""
	if t.proc != nil {
		addr = t.proc.ControlAddr()
	}
	if err := t.ensureControlLocked(addr); err != nil {
		return fmt.Errorf("tor control не подключён: %w", err)
	}
	if d := time.Since(t.lastNew); d < t.cooldown {
		wait := t.cooldown - d
		t.logf("newnym cooldown: ждём %v", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	if err := t.ctrl.NewIdentity(ctx); err != nil {
		// Сбой команды оставляет сокет с истёкшим абсолютным дедлайном: без
		// переподключения все последующие ротации падали бы мгновенно, пока
		// процесс жив. Пробуем свежим соединением ровно один раз.
		t.logf("tor newnym: %v, переподключаю control", err)
		t.dropControl()
		if cerr := t.connectControl(); cerr != nil {
			return fmt.Errorf("tor newnym: %w (переподключение control: %v)", err, cerr)
		}
		if err2 := t.ctrl.NewIdentity(ctx); err2 != nil {
			// Хендл сбрасывается, но следующая ротация теперь переподключится
			// сама: защёлки больше нет.
			t.dropControl()
			return fmt.Errorf("tor newnym (повтор): %w", err2)
		}
	}
	t.lastNew = time.Now()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (t *torRotator) Healthy() bool {
	t.mu.Lock()
	ctrl := t.ctrl
	proc := t.proc
	t.mu.Unlock()
	if ctrl == nil || proc == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := ctrl.GetInfo(ctx, "version")
	return err == nil && v != ""
}

func (t *torRotator) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctrl != nil {
		t.ctrl.Close()
		t.ctrl = nil
	}
	if t.proc != nil {
		err := t.proc.Stop()
		t.proc = nil
		return err
	}
	return nil
}

func (t *torRotator) logf(format string, args ...any) {
	if t.log != nil {
		t.log.Infof(format, args...)
	}
}

// externalRotator работает с уже запущенным tor: системным демоном, tor из
// Tor Browser или демоном, поднятым отдельным процессом voidsearchswag.
//
// Собственный tor поднимается 20-25 секунд, и каждый вызов CLI платил эту
// цену заново. Внешний демон позволяет прогреть tor один раз и дальше
// получать готовый транспорт мгновенно.
//
// Процессом этот ротатор НЕ владеет: Close закрывает только control-соединение
// и никогда не убивает чужой tor. Убить демон, который поднял не ты, значит
// сломать все остальные процессы, которые на него рассчитывают.
type externalRotator struct {
	mu         sync.Mutex
	socksAddr  string
	ctrlAddr   string
	ctrl       *tornago.ControlClient
	lastNew    time.Time
	cooldown   time.Duration
	timeout    time.Duration
	log        Logger
	noCtrlOnce sync.Once
}

// dialControl устанавливает и аутентифицирует control-соединение. Вынесено в
// общую функцию: и собственный, и внешний tor подключаются одинаково, а
// расхождение в аутентификации дало бы два разных набора плавающих отказов.
func dialControl(addr string, timeout time.Duration) (*tornago.ControlClient, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("control-адрес tor не задан")
	}
	auth, _, err := tornago.ControlAuthFromTor(addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tor control auth: %w", err)
	}
	ctrl, err := tornago.NewControlClient(addr, auth, timeout)
	if err != nil {
		return nil, fmt.Errorf("tor control client: %w", err)
	}
	if err := ctrl.Authenticate(); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("tor control authenticate: %w", err)
	}
	return ctrl, nil
}

// control возвращает живое control-соединение, переподключаясь при нужде.
//
// Дедлайн на сокете в Go абсолютный: однажды истёк - соединение навсегда
// отвечает i/o timeout на любую операцию. Без переподключения одна медленная
// команда NEWNYM выводила control-канал из строя до конца жизни процесса, и
// все последующие ротации падали с «failed to flush command». Наблюдено вживую
// на общем демоне: первая же долгая ротация ломала канал для всех команд.
func (t *externalRotator) control() *tornago.ControlClient {
	if t.ctrl != nil {
		return t.ctrl
	}
	// Пустой control-адрес - свойство конфигурации процесса, а не событие:
	// он не изменится между ротациями. Этап 178 (смоук I, H): строка
	// «control-адрес tor не задан» писалась в лог каждой ротацией, а
	// следом Rotate дублировал своей - по две идентичные строки на каждый
	// оборот при работающем транспорте. Теперь сообщение выходит один раз
	// за жизнь процесса.
	if strings.TrimSpace(t.ctrlAddr) == "" {
		t.noCtrlOnce.Do(func() {
			t.logf("control-адрес tor не задан: NEWNYM недоступен, цепь не меняется, транспорт работает")
		})
		return nil
	}
	ctrl, err := dialControl(t.ctrlAddr, t.timeout)
	if err != nil {
		t.logf("tor control %s: %v, работаю без NEWNYM", t.ctrlAddr, err)
		return nil
	}
	t.ctrl = ctrl
	return ctrl
}

// dropControl помечает соединение мёртвым, чтобы следующий вызов control()
// установил новое. Старое закрывается: держать битый сокет открытым значит
// удерживать файловый дескриптор и вводить tor в заблуждение о числе клиентов.
func (t *externalRotator) dropControl() {
	if t.ctrl != nil {
		t.ctrl.Close()
		t.ctrl = nil
	}
}

// attachTor подключается к внешнему tor. socks-порт обязан быть живым:
// иначе вместо мгновенного старта получаем молчаливый откат к прямому
// соединению, а для onion это и падение, и утечка в системный DNS.
func attachTor(cfg Config) (Rotator, error) {
	addr := normalizeHostPort(cfg.TorSocksAddr)
	if addr == "" {
		return nil, fmt.Errorf("tor: пустой socks-адрес")
	}
	if err := probeTCP(addr, 3*time.Second); err != nil {
		return nil, fmt.Errorf("внешний tor на %s недоступен: %w", addr, err)
	}

	t := &externalRotator{
		socksAddr: addr,
		ctrlAddr:  normalizeHostPort(cfg.TorControlAddr),
		cooldown:  cfg.MinNewnymInterval,
		timeout:   10 * time.Second,
		log:       cfg.Logger,
	}
	if t.cooldown <= 0 {
		t.cooldown = 11 * time.Second
	}

	// Control-порт необязател. Без него нет NEWNYM и проверки версии, но
	// транспорт полностью рабочий, поэтому отсутствие control не ошибка.
	// Соединение устанавливается сразу, но при любом сбое будет пересоздано
	// лениво: абсолютный дедлайн Go делает битый сокет непригодным навсегда.
	if t.ctrlAddr != "" {
		_ = t.control()
	}

	if t.ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
		defer cancel()
		if err := waitBootstrapOn(ctx, t.ctrl, cfg.timeoutOrDefault(), t.logf); err != nil {
			t.logf("внешний tor ещё строит цепи: %v", err)
		}
	}
	// Статус печатается после проверки круговым запросом, а не по хендлу:
	// иначе строка обещает рабочее управление там, где команды не проходят.
	t.logf("внешний tor подключён: socks=%s control=%s", addr, t.controlStatus())
	return t, nil
}

// ControlStatus возвращает проверенное состояние control-канала внешнего tor.
//
// Метод экспортирован, потому что его вызывает диагностика в CLI: onioncheck
// обязан показывать состояние транспорта, от которого зависит здоровье
// движков. Вызов через опциональный интерфейс, поэтому прочие ротаторы
// реализовывать его не обязаны.
func (t *externalRotator) ControlStatus() string {
	return t.controlStatus()
}

// controlStatus возвращает проверенное состояние control-канала.
//
// Прежняя строка печатала `control=подключён` по признаку t.ctrl != nil, то
// есть по самому факту существования хендла. Это утверждало состояние, которое
// никогда не проверялось, и в живых прогонах давало прямое противоречие:
// «control=подключён» при шести подряд таймаутах NEWNYM. Пользователь не мог
// отличить «канал живой» от «хендл создан, но команды не проходят» и искал
// причину не там.
//
// Хендл создаётся одним dial, а пригодность канала определяется только
// круговым запросом: дедлайн на сокете в Go абсолютный, поэтому однажды
// истёкшее соединение навсегда отвечает i/o timeout, оставаясь при этом
// ненулевым. Проверка идёт тем же GetInfo("version"), что и в Healthy(), -
// это единственная команда, которая одновременно cheap и доказывает, что канал
// принимает и возвращает данные.
//
// Возвращает одну из трёх строк:
//   - "не задан" - адрес control-порта не настроен, NEWNYM недоступен по
//     конфигурации, а не из-за сбоя;
//   - "не отвечает (причина)" - адрес есть, но круговой запрос не прошёл:
//     соединение не установлено, аутентификация отвергнута, GETINFO вернул
//     ошибку или ключ не найден в ответе;
//   - "подключён, tor <версия>" - канал проверен и версия получена.
func (t *externalRotator) controlStatus() string {
	t.mu.Lock()
	addr := t.ctrlAddr
	ctrl := t.control()
	t.mu.Unlock()

	if addr == "" {
		return "не задан"
	}
	if ctrl == nil {
		return "не отвечает (соединение не установлено)"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := ctrl.GetInfo(ctx, "version")
	if err != nil {
		// Канал бит: сбрасываем, чтобы следующий вызов переподключился.
		// Без этого мёртвый сокет пережил бы отчёт и следующая команда
		// NEWNYM упала бы мгновенно на уже непригодном соединении.
		t.mu.Lock()
		t.dropControl()
		t.mu.Unlock()
		return "не отвечает (" + err.Error() + ")"
	}
	// Отдельная проверка на пустую версию не нужна и была бы недостижимым
	// кодом: tornago.getInfo возвращает ошибку «key not found in GETINFO
	// response» всякий раз, когда разобранный результат пуст, поэтому
	// комбинация (пустая строка, nil-ошибка) со стороны зависимости
	// невозможна. Пустой ответ уходит в ветку err выше.
	return "подключён, tor " + strings.TrimSpace(v)
}

func (t *externalRotator) Kind() string { return "tor" }

func (t *externalRotator) TransportSpec() string { return "socks5://" + t.socksAddr }

func (t *externalRotator) Rotate(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := time.Since(t.lastNew); d < t.cooldown {
		wait := t.cooldown - d
		t.logf("newnym cooldown: ждём %v", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	ctrl := t.control()
	if ctrl == nil {
		// Без control-порта цепь не сменить. Это не ошибка транспорта:
		// запросы продолжают ходить, просто выход остаётся прежним.
		// Причина уже названа control() (один раз за жизнь процесса),
		// повторять её на каждом обороте нечем новым.
		return nil
	}
	err := ctrl.NewIdentity(ctx)
	if err != nil {
		// Одна неудачная команда оставляет сокет с истёкшим абсолютным
		// дедлайном: все дальнейшие вызовы падали бы мгновенно. Сбрасываем
		// соединение и пробуем свежим ровно один раз.
		t.logf("tor newnym: %v, переподключаю control", err)
		t.dropControl()
		if ctrl = t.control(); ctrl == nil {
			return fmt.Errorf("tor newnym: %w", err)
		}
		if err2 := ctrl.NewIdentity(ctx); err2 != nil {
			t.dropControl()
			return fmt.Errorf("tor newnym (повтор): %w", err2)
		}
	}
	t.lastNew = time.Now()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (t *externalRotator) Healthy() bool {
	t.mu.Lock()
	ctrl := t.control()
	addr := t.socksAddr
	t.mu.Unlock()
	if ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		v, err := ctrl.GetInfo(ctx, "version")
		cancel()
		if err == nil && v != "" {
			return true
		}
		// Канал бит - сбрасываем, чтобы следующий вызов переподключился.
		t.mu.Lock()
		t.dropControl()
		t.mu.Unlock()
	}
	// Без control здоровье проверяется доступностью socks-порта: это слабее,
	// чем GetInfo, но достаточно, чтобы не считать транспорт живым после
	// падения демона.
	return probeTCP(addr, 3*time.Second) == nil
}

func (t *externalRotator) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctrl != nil {
		t.ctrl.Close()
		t.ctrl = nil
	}
	// Чужой процесс не останавливаем намеренно.
	return nil
}

func (t *externalRotator) logf(format string, args ...any) {
	if t.log != nil {
		t.log.Infof(format, args...)
	}
}

// normalizeHostPort приводит адрес к host:port, добавляя порт по умолчанию
// для tor, если он не задан: 9050 для socks и 9051 для control.
func normalizeHostPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	addr = strings.TrimPrefix(addr, "socks5://")
	addr = strings.TrimPrefix(addr, "socks5h://")
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimSuffix(addr, "/")
	return addr
}

func probeTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// IsolateSpec возвращает копию socks-спецификации с учётными данными, которые
// заставляют tor построить ОТДЕЛЬНУЮ цепь.
//
// Tor по умолчанию изолирует потоки по SOCKS-логину (IsolateSOCKSAuth), поэтому
// разные пары логин/пароль на одном socks-порту дают разные выходы. Без этого
// весь параллелизм проб фиктивный: восемь воркеров стоят в очередь к одной
// цепи, и волна из 10 адресов идёт минуту вместо десяти секунд.
//
// Для не-socks спецификаций (обычный http-прокси) изоляция не применяется и
// возвращается исходное значение: чужой прокси сам решает, как разделять
// потоки, и подделывать там учётные данные значит ломать авторизацию.
func IsolateSpec(spec, key string) string {
	spec = strings.TrimSpace(spec)
	key = strings.TrimSpace(key)
	if spec == "" || key == "" {
		return spec
	}
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" {
		return spec
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
	default:
		return spec
	}
	u.User = url.UserPassword(key, key)
	return u.String()
}

type proxyRotator struct {
	mu      sync.Mutex
	proxies []string
	i       int
}

func NewProxyRotator(list []string) (Rotator, error) {
	out := make([]string, 0, len(list))
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "://") {
			p = "socks5://" + p
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("пустой список прокси")
	}
	return &proxyRotator{proxies: out}, nil
}

func (p *proxyRotator) Kind() string { return "proxy" }

func (p *proxyRotator) TransportSpec() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proxies[p.i%len(p.proxies)]
}

func (p *proxyRotator) Rotate(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.i = (p.i + 1) % len(p.proxies)
	return nil
}

func (p *proxyRotator) Healthy() bool { return true }
func (p *proxyRotator) Close() error  { return nil }

type directRotator struct{ n int }

func (d *directRotator) Kind() string          { return "direct" }
func (d *directRotator) TransportSpec() string { return "" }
func (d *directRotator) Rotate(context.Context) error {
	d.n++
	return nil
}
func (d *directRotator) Healthy() bool { return true }
func (d *directRotator) Close() error  { return nil }

func NewRotator(ctx context.Context, cfg Config) (Rotator, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case "tor", "":
		return StartTor(cfg)
	case "proxy":
		if len(cfg.Proxies) > 0 {
			return NewProxyRotator(cfg.Proxies)
		}
		if prov := providerFromConfig(cfg); prov != nil {
			return newPoolRotatorFromConfig(ctx, cfg, prov)
		}
		return nil, fmt.Errorf("транспорт proxy: не задан ни список прокси, ни провайдер")
	case "pool", "proxyscrape":
		return newPoolRotatorFromConfig(ctx, cfg, ProviderForPool(cfg))
	case "direct":
		return &directRotator{}, nil
	default:
		return nil, fmt.Errorf("неизвестный транспорт %q (tor|proxy|pool|direct)", cfg.Transport)
	}
}

func providerFromConfig(cfg Config) Provider {
	name := strings.ToLower(strings.TrimSpace(cfg.ProxyProvider))
	if name == "" {
		return nil
	}
	switch name {
	case "proxyscrape", "proxy-scrape", "scrape":
		return ProxyScrape{
			Protocol:  cfg.ProxyScrapeProtocol,
			Country:   cfg.ProxyScrapeCountry,
			TimeoutMS: cfg.ProxyScrapeTimeoutMS,
			Anonymity: cfg.ProxyScrapeAnonymity,
			SSL:       cfg.ProxyScrapeSSL,
			Endpoint:  cfg.ProxyProviderURL,
		}
	default:
		return nil
	}
}

// cleanList отбрасывает пустые и пробельные элементы: список вида
// ["", " "] не должен превращаться в провайдера без единого адреса.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// defaultPoolProvider собирает провайдера для транспорта pool, когда имя
// провайдера не задано. Порядок важен:
//
//  1. Явный список прокси - пользователь задал адреса сам, значит ходить на
//     публичный API не нужно вовсе.
//  2. Явный endpoint или параметры scrape - они обязаны попасть в провайдера.
//     Раньше здесь подставлялся DefaultProxyScrape() с пустым Endpoint, и
//     ProxyProviderURL молча выбрасывался: запрос уходил на публичный
//     api.proxyscrape.com вместо указанного адреса.
//  3. Полный дефолт - публичный ProxyScrape с его endpoint.
func defaultPoolProvider(cfg Config) Provider {
	if list := cleanList(cfg.Proxies); len(list) > 0 {
		return StaticProvider(list)
	}
	ps := DefaultProxyScrape()
	if ep := strings.TrimSpace(cfg.ProxyProviderURL); ep != "" {
		ps.Endpoint = ep
	}
	if v := strings.TrimSpace(cfg.ProxyScrapeProtocol); v != "" {
		ps.Protocol = v
	}
	if v := strings.TrimSpace(cfg.ProxyScrapeCountry); v != "" {
		ps.Country = v
	}
	if v := strings.TrimSpace(cfg.ProxyScrapeAnonymity); v != "" {
		ps.Anonymity = v
	}
	if v := strings.TrimSpace(cfg.ProxyScrapeSSL); v != "" {
		ps.SSL = v
	}
	if cfg.ProxyScrapeTimeoutMS > 0 {
		ps.TimeoutMS = cfg.ProxyScrapeTimeoutMS
	}
	return ps
}

// ProviderForPool выбирает поставщика прокси-пула по тому же правилу, что и
// транспорт pool: явное имя провайдера, затем список адресов или endpoint с
// параметрами scrape из конфига, затем публичный дефолт. Экспортирован ради
// прогрева пула из CLI: poolcheck и setup --warm-pool строили netx.ProxyScrape
// из двух строк своими руками и молча выбрасывали ProxyProviderURL,
// ProxyScrapeAnonymity, ProxyScrapeSSL и ProxyScrapeTimeoutMS, поэтому запрос
// уходил на публичный api.proxyscrape.com даже при явно заданном endpoint.
func ProviderForPool(cfg Config) Provider {
	if prov := providerFromConfig(cfg); prov != nil {
		return prov
	}
	return defaultPoolProvider(cfg)
}

// PoolConfigFrom превращает конфиг в параметры пула. Единственное место, где
// это делается: транспорт pool и прогрев пула из CLI обязаны получать один и
// тот же ProbeTimeout, Concurrency, MinLive и StatePath.
func PoolConfigFrom(cfg Config, prov Provider) PoolConfig {
	pc := DefaultPoolConfig(prov)
	pc.Verify = !cfg.ProxySkipVerify
	if cfg.ProxyProbeTimeout > 0 {
		pc.ProbeTimeout = cfg.ProxyProbeTimeout
	}
	if cfg.ProxyProbeConcurrency > 0 {
		pc.Concurrency = cfg.ProxyProbeConcurrency
	}
	if cfg.ProxyFetchLimit > 0 {
		pc.FetchLimit = cfg.ProxyFetchLimit
	}
	if cfg.ProxyPoolSize > 0 {
		pc.MaxLive = cfg.ProxyPoolSize
	}
	if cfg.ProxyMinLive > 0 {
		pc.MinLive = cfg.ProxyMinLive
	}
	if pc.MinLive > pc.MaxLive/4 {
		pc.MinLive = max(1, pc.MaxLive/4)
	}
	// Ключевые слова off/none/- здесь тоже регистронезависимы, как TorEnabled,
	// Transport и имя провайдера: иначе VOIDSEARCH_PROXY_STATE_PATH=OFF не
	// выключал запись, а молча создавал файл с именем «OFF». Сам путь в default
	// остаётся как задан: регистр в имени файла - часть пути, а не ключевое слово.
	sp := strings.TrimSpace(cfg.ProxyStatePath)
	switch strings.ToLower(sp) {
	case "":
		pc.StatePath = DefaultStatePath()
	case "off", "none", "-":
		pc.StatePath = ""
	default:
		pc.StatePath = sp
	}
	return pc
}

func newPoolRotatorFromConfig(ctx context.Context, cfg Config, prov Provider) (Rotator, error) {
	pool, err := NewProxyPool(ctx, PoolConfigFrom(cfg, prov), cfg.Logger)
	if err != nil {
		return nil, err
	}
	if lg := cfg.Logger; lg != nil {
		live, total, _ := pool.Stats()
		lg.Infof("пул прокси: %s, живых %d из %d записей", pool.ProviderName(), live, total)
	}
	return NewPoolRotator(pool)
}
