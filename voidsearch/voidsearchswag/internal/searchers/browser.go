package searchers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
)

// cloakUAs - ротация десктопных User-Agent на каждый поиск. Один зашитый UA
// на все запросы - палевный паттерн: антибот связывает сессии по нему.
var cloakUAs = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
}

// browserRetryCooldown - пауза перед повторной попыткой поднять Chromium после
// переходного сбоя.
//
// Нужна потому, что прежняя версия защёлкивала dead навсегда: один сбой запуска
// (Chromium занят, нехватка памяти, гонка при первом скачивании) выключал
// stealth-режим до рестарта процесса. Для долгоживущего MCP-сервера это означало
// потерю браузера из-за одной случайности, а пользователь видел «браузер
// недоступен» на всех последующих запросах.
//
// Пауза защищает и от обратной крайности: без неё каждый поиск дёргал бы запуск
// Chromium заново и платил секунды на попытку, которая заведомо провалится, пока
// причина не устранена.
const browserRetryCooldown = 2 * time.Minute

// renderSettleDelay - пауза после WaitLoad, за которую поисковик дорисовывает
// результаты скриптами.
//
// WaitLoad дожидается события load, но DDG и Bing подгружают выдачу позже, и без
// паузы page.HTML() возвращал каркас страницы без результатов. Значение прежнее
// (1500 мс), вынесено в константу, потому что теперь оно участвует в select с
// контекстом и встречается в одном месте.
const renderSettleDelay = 1500 * time.Millisecond

// Browser - cloaked rod-браузер, замена вырезанному Brave API и ответ на
// вопрос "почему не Playwright": playwright-go тянет Node-драйвер внешним
// скачиванием (~десятки МБ, свой рантайм) и ломает главный контракт проекта -
// один статический бинарь без внешних зависимостей. rod говорит с Chromium
// по CDP из чистого Go и собирается тем же CGO_ENABLED=0.
//
// Cloak-профиль: stealth.Page (чистит webdriver-флаги), ротация UA на поиск,
// окно 1920x1080, --disable-blink-features=AutomationControlled, no-first-run,
// прокси из пула. Этого хватает против DDG/Bing; против Cloudflare grade
// защиты браузер - тоже лишь попытка, и отчёт честно покажет fail.
//
// Все поля состояния читаются и пишутся только под mu. Прежняя версия писала
// dead под мьютексом, а читала в Search и Warmup без него - это гонка данных,
// которую ловит -race в CI, и на практике чтение могло увидеть значение,
// которое другой поток ещё не закончил писать.
type Browser struct {
	mu       sync.Mutex
	browser  *rod.Browser
	launcher *launcher.Launcher
	headless bool
	proxy    string
	binPath  string
	timeout  time.Duration
	log      netx.Logger

	// permanent означает, что браузер не поднимется никогда: нет бинаря
	// Chromium и не найдено пути к нему. Такой отказ не сбрасывается, потому что
	// повторная проверка launcher.LookPath на каждый поиск только жгла бы время.
	permanent bool
	// deadUntil - момент, до которого повторная попытка бессмысленна после
	// переходного сбоя запуска или подключения. Нулевое значение означает
	// «браузер пригоден».
	deadUntil time.Time
	// lastErr хранит причину последнего отказа, чтобы вызывающий получал не
	// безличное «браузер недоступен», а конкретную причину.
	lastErr error
}

func NewBrowser(proxy, binPath string, headless bool, log netx.Logger) *Browser {
	return &Browser{
		headless: headless,
		proxy:    proxy,
		binPath:  binPath,
		timeout:  45 * time.Second,
		log:      log,
	}
}

func (b *Browser) Name() string { return "rod-browser" }

// available сообщает, есть ли на машине Chromium, который можно запустить.
//
// Явно заданный binPath проверяется на существование, а не просто на непустоту.
// Прежняя версия возвращала true для любого непустого пути, поэтому неверно
// заданный VOIDSEARCH_CHROME_PATH приводил к попытке запуска, которая падала как
// переходный сбой и повторялась каждые две минуты бесконечно. Ошибка конфигурации
// сама не проходит: она должна определяться как постоянный отказ сразу, с
// понятным сообщением, а не как «ресурс занят».
func (b *Browser) available() bool {
	if b.binPath != "" {
		_, err := os.Stat(b.binPath)
		return err == nil
	}
	_, ok := launcher.LookPath()
	return ok
}

// blockedLocked возвращает ошибку, если браузер сейчас непригоден.
//
// Вызывается только под mu. Различает постоянный отказ и переходный: постоянный
// возвращается всегда, переходный - только пока не истёк cooldown, после чего
// попытка разрешена снова и состояние сбрасывается.
//
// Сброс при истечении cooldown важен: без него поле deadUntil оставалось бы
// заполненным и следующая неудача продлила бы его от старого момента, то есть
// браузер мог бы остаться заблокированным дольше, чем задумано.
func (b *Browser) blockedLocked() error {
	if b.permanent {
		if b.lastErr != nil {
			return b.lastErr
		}
		return errors.New("браузер недоступен")
	}
	if b.deadUntil.IsZero() {
		return nil
	}
	if time.Now().Before(b.deadUntil) {
		if b.lastErr != nil {
			return fmt.Errorf("браузер временно недоступен (повтор через %s): %w",
				time.Until(b.deadUntil).Round(time.Second), b.lastErr)
		}
		return errors.New("браузер временно недоступен")
	}
	// Cooldown истёк - разрешаем новую попытку.
	b.deadUntil = time.Time{}
	b.lastErr = nil
	return nil
}

// blocked - безопасная обёртка для вызова без удержания mu.
func (b *Browser) blocked() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blockedLocked()
}

// markTransient фиксирует переходный сбой и откладывает следующую попытку.
//
// Вызывается только под mu.
func (b *Browser) markTransientLocked(err error) {
	b.deadUntil = time.Now().Add(browserRetryCooldown)
	b.lastErr = err
}

// markPermanent фиксирует отказ, который не пройдёт сам.
//
// Вызывается только под mu.
func (b *Browser) markPermanentLocked(err error) {
	b.permanent = true
	b.lastErr = err
}

func (b *Browser) ensure() (*rod.Browser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.browser != nil {
		return b.browser, nil
	}
	if err := b.blockedLocked(); err != nil {
		return nil, err
	}
	if !b.available() {
		err := errors.New("chromium не найден на этой машине")
		b.markPermanentLocked(err)
		return nil, err
	}
	l := launcher.New().
		Headless(b.headless).
		NoSandbox(true).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-dev-shm-usage").
		Set("no-first-run").
		Set("disable-gpu").
		Set("disable-background-timer-throttling").
		Set("disable-renderer-backgrounding").
		Set("window-size", "1920,1080")
	if b.binPath != "" {
		l = l.Bin(b.binPath)
	}
	if b.proxy != "" {
		if host := httpc.ProxyHostPort(b.proxy); host != "" {
			l = l.Proxy(host)
		}
	}
	// Запуск и подключение идут под мьютексом намеренно: без него N
	// конкурентных поисков подняли бы N процессов Chromium. Цена - вызывающие
	// ждут холодный старт, но это ровно та сериализация, которая нужна, а
	// повторный запуск стоил бы сотни мегабайт и секунды на каждый поток.
	controlURL, err := l.Launch()
	if err != nil {
		wrapped := fmt.Errorf("запуск chromium: %w", err)
		b.markTransientLocked(wrapped)
		return nil, wrapped
	}
	br := rod.New().ControlURL(controlURL)
	if err := br.Connect(); err != nil {
		l.Kill()
		wrapped := fmt.Errorf("подключение к chromium: %w", err)
		b.markTransientLocked(wrapped)
		return nil, wrapped
	}
	b.browser = br
	b.launcher = l
	// Успешный запуск снимает отметку об отказе: прежняя версия оставляла
	// lastErr заполненным, и последующая проверка могла сообщить устаревшую
	// причину.
	b.deadUntil = time.Time{}
	b.lastErr = nil
	b.logf("браузер поднят (headless=%v)", b.headless)
	return br, nil
}

func (b *Browser) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if err := b.blocked(); err != nil {
		return nil, err
	}
	br, err := b.ensure()
	if err != nil {
		return nil, err
	}
	// Два источника подряд: DDG первым (лёгкий, редко блочит), Bing вторым
	// (тяжёлый, но с другим индексом). Второй дёргается только если первый
	// отдал пусто - два полных рендера на каждый запрос жечь незачем.
	res, err := b.searchPage(ctx, br, "https://duckduckgo.com/?q="+url.QueryEscape(query)+"&ia=web", true)
	if err != nil {
		b.logf("браузер ddg: %v", err)
	}
	if len(res) == 0 {
		bing, berr := b.searchPage(ctx, br, "https://www.bing.com/search?q="+url.QueryEscape(query), false)
		if berr != nil {
			b.logf("браузер bing: %v", berr)
		}
		res, err = combineSourceResults(res, err, bing, berr)
		if err != nil {
			return nil, err
		}
	}
	if limit > 0 && len(res) > limit {
		res = res[:limit]
	}
	return res, nil
}

// combineSourceResults сводит результаты двух источников и решает, что вернуть
// вызывающему: выдачу или ошибку.
//
// Вынесено в чистую функцию, потому что логика нетривиальна, а проверить её на
// живом Search нельзя без настоящего Chromium. Чистая функция тестируется на всех
// комбинациях исходов обоих источников.
//
// Ключевое правило: ошибка первого источника не теряется, если второй ответил
// успешно, но пусто. Прежняя версия возвращала (пусто, nil) в ситуации «DDG упал,
// Bing ответил без результатов», и вызывающий видел «результатов нет» вместо
// «источник не сработал». Для отчёта о движках это принципиально: fail и пустой
// ответ ведут себя по-разному - fail учитывается в статистике здоровья и
// запускает повтор со сменой цепи, а пустой ответ считается нормальным исходом.
//
// Первый источник здесь DDG, второй Bing, но функция от имён не зависит: важна
// только пара (результаты, ошибка) для каждого.
func combineSourceResults(first []Result, firstErr error, second []Result, secondErr error) ([]Result, error) {
	switch {
	case len(first) > 0:
		// Первый источник дал выдачу - второй не нужен.
		return first, nil
	case firstErr != nil && secondErr != nil:
		return nil, fmt.Errorf("браузер: ddg: %v; bing: %w", firstErr, secondErr)
	case firstErr != nil:
		if len(second) == 0 {
			// Второй ответил успешно, но пусто: ошибка первого не должна
			// исчезнуть, иначе сбой источника выглядит как отсутствие
			// результатов.
			return nil, fmt.Errorf("браузер: ddg: %v; bing ответил, но без результатов", firstErr)
		}
		return second, nil
	case secondErr != nil:
		return nil, fmt.Errorf("браузер: %w", secondErr)
	default:
		// Оба источника ответили успешно и оба пусты. Это честный пустой
		// результат, а не ошибка: браузер сработал, выдача действительно пуста.
		return second, nil
	}
}

func (b *Browser) searchPage(ctx context.Context, br *rod.Browser, target string, isDDG bool) ([]Result, error) {
	page, err := stealth.Page(br)
	if err != nil {
		return nil, fmt.Errorf("stealth-страница: %w", err)
	}
	defer page.Close()

	ectx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	page = page.Context(ectx)

	ua := cloakUAs[rand.Intn(len(cloakUAs))]
	_ = page.SetUserAgent(&proto.NetworkSetUserAgentOverride{UserAgent: ua})

	if err := page.Navigate(target); err != nil {
		return nil, fmt.Errorf("навигация: %w", err)
	}
	_ = page.WaitLoad()

	// Пауза на дорендер JS обязана уважать контекст. Прежний time.Sleep(1500ms)
	// игнорировал отмену и таймаут: при прерывании поиска или истечении срока
	// поток всё равно спал полторы секунды, а при двух источниках подряд - три.
	// На долгоживущем MCP-сервере это задерживало ответ на отменённый запрос и
	// держало страницу открытой дольше нужного.
	select {
	case <-ectx.Done():
		return nil, fmt.Errorf("рендер прерван: %w", ectx.Err())
	case <-time.After(renderSettleDelay):
	}

	html, err := page.HTML()
	if err != nil {
		return nil, fmt.Errorf("чтение DOM: %w", err)
	}
	if isDDG {
		return parseDDG([]byte(html), b.Name())
	}
	return parseBing([]byte(html), b.Name())
}

// Warmup готовит cloaked-браузер заранее: скачивает Chromium (первый запуск),
// поднимает его, открывает пустую stealth-страницу и закрывает её. Вызывается
// из setup, чтобы первый stealth-поиск не ждал скачивание ~150МБ и не падал
// с ошибкой запуска посреди задачи агента.
func (b *Browser) Warmup(ctx context.Context) error {
	if err := b.blocked(); err != nil {
		return err
	}
	br, err := b.ensure()
	if err != nil {
		return err
	}
	page, err := stealth.Page(br)
	if err != nil {
		return fmt.Errorf("stealth-страница: %w", err)
	}
	defer page.Close()

	ectx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	page = page.Context(ectx)

	ua := cloakUAs[rand.Intn(len(cloakUAs))]
	_ = page.SetUserAgent(&proto.NetworkSetUserAgentOverride{UserAgent: ua})

	if err := page.Navigate("about:blank"); err != nil {
		return fmt.Errorf("навигация: %w", err)
	}
	_ = page.WaitLoad()
	return nil
}

func (b *Browser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.browser != nil {
		_ = b.browser.Close()
		b.browser = nil
	}
	if b.launcher != nil {
		b.launcher.Cleanup()
		b.launcher = nil
	}
	return nil
}

func (b *Browser) logf(format string, args ...any) {
	if b.log != nil {
		b.log.Infof(format, args...)
	}
}
