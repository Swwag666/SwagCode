// Package promote - автопромоут поисковиков из плана "следующий уровень".
//
// Зашитые сиды дохнут, а пул живых сервисов растёт сам разведкой. Промоут
// ищет среди живых сервисов страницы с поисковой формой, проверяет их
// пробным запросом и поднимает прошедшие в onion-каталог: deep становится
// самовосстанавливающимся вместо медленно умирающего на четырёх сидах.
package promote

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/searchers"
)

// Candidate - распознанный кандидат в поисковики.
type Candidate struct {
	Name     string `json:"name"`
	Base     string `json:"base"`
	Path     string `json:"path"`
	Selector string `json:"selector"`
}

type Promoter struct {
	Client       *httpc.Client
	Log          netx.Logger
	MinOnionHits int
	Timeout      time.Duration

	// Budget ограничивает весь прогон промоута по времени. Без него limit=50
	// превращается в полчаса: каждая проверка это fetch страницы плюс
	// верификационный поиск, а ротация цепи tor ждёт NEWNYM-cooldown 11 секунд.
	Budget time.Duration
}

// DefaultBudget - потолок прогона промоута, если Budget не задан.
const DefaultBudget = 3 * time.Minute

func (p *Promoter) budget() time.Duration {
	if p.Budget > 0 {
		return p.Budget
	}
	return DefaultBudget
}

func (p *Promoter) minHits() int {
	if p.MinOnionHits > 0 {
		return p.MinOnionHits
	}
	return 3
}

func (p *Promoter) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 60 * time.Second
}

// Progress ведёт учёт прогона: сколько кандидатов проверено и не исчерпан ли
// бюджет. Один тип на все три точки вызова (CLI, MCP, фоновый тик), потому что
// расхождение в ограничении времени дало бы три разных поведения промоута.
type Progress struct {
	start   time.Time
	budget  time.Duration
	Checked int
	// Failed - сколько проверок кандидатов не состоялось: tor мёртв, адрес не
	// отвечает, контекст истёк. Checked к этому моменту уже увеличен, поэтому
	// без Failed прогон «проверено 6, поднято 0» неотличим от «все шесть
	// проверок упали», - а это противоположные выводы о состоянии пула.
	Failed int
	// SaveFailed - сколько найденных сидов не удалось сохранить в базу.
	// Счётчик отдельный: отказ записи не означает, что движок не найден, и
	// смешивать его с отказом проверки значит потерять оба факта.
	SaveFailed int
	// LastError - первая причина отказа. Первая, а не последняя: при массовой
	// поломке (tor лёг, сеть недоступна) именно первая объясняет, что
	// случилось, а остальные N ей одинаковы.
	LastError string
	// StoppedReason объясняет досрочную остановку. Пустая строка означает, что
	// прогон дошёл до конца списка кандидатов штатно.
	StoppedReason string
}

func NewProgress(budget time.Duration) *Progress {
	if budget <= 0 {
		budget = DefaultBudget
	}
	return &Progress{start: time.Now(), budget: budget}
}

// Fail учитывает несостоявшуюся проверку кандидата. Метод, а не прямой инкремент
// в трёх вызывающих: CLI, MCP-инструмент и фоновый тик обязаны считать отказы
// одинаково, иначе расхождение вернуло бы тот же дефект в одном из трёх мест.
func (pr *Progress) Fail(err error) {
	if pr == nil || err == nil {
		return
	}
	pr.Failed++
	if pr.LastError == "" {
		pr.LastError = err.Error()
	}
}

// FailSave учитывает найденный, но не сохранённый сид.
func (pr *Progress) FailSave(err error) {
	if pr == nil || err == nil {
		return
	}
	pr.SaveFailed++
	if pr.LastError == "" {
		pr.LastError = err.Error()
	}
}

// Note возвращает одну строку об отказах прогона или пустую строку, если их не
// было. Нужна фоновому тику и логам, где печать полей по отдельности
// бессмысленна.
func (pr *Progress) Note() string {
	if pr == nil || (pr.Failed == 0 && pr.SaveFailed == 0) {
		return ""
	}
	note := fmt.Sprintf("отказов %d (проверок %d, сохранений %d) из %d проверенных",
		pr.Failed+pr.SaveFailed, pr.Failed, pr.SaveFailed, pr.Checked)
	if pr.LastError != "" {
		note += ": " + pr.LastError
	}
	return note
}

// Exceeded сообщает, что бюджет исчерпан, и фиксирует причину один раз:
// повторные вызовы не должны перезаписывать уже найденную причину.
func (pr *Progress) Exceeded() bool {
	if pr == nil {
		return false
	}
	if pr.StoppedReason != "" {
		return true
	}
	if time.Since(pr.start) >= pr.budget {
		pr.StoppedReason = fmt.Sprintf("бюджет времени %v исчерпан после %d проверок",
			pr.budget.Round(time.Second), pr.Checked)
		return true
	}
	return false
}

// Remaining отдаёт остаток бюджета. Используется, чтобы ограничить контекст
// отдельной проверки: кандидат не должен съедать весь бюджет и оставлять
// остальных непроверенными.
func (pr *Progress) Remaining() time.Duration {
	if pr == nil {
		return 0
	}
	left := pr.budget - time.Since(pr.start)
	if left < 0 {
		return 0
	}
	return left
}

// Elapsed возвращает длительность прогона для отчёта.
func (pr *Progress) Elapsed() time.Duration {
	if pr == nil {
		return 0
	}
	return time.Since(pr.start).Round(time.Millisecond)
}

// Detect ищет поисковую форму в разметке страницы. Формы входа отсекаются
// по password-полю: иначе каждый форум с логином признавался бы поисковиком.
// Путь собирается из action формы и имени текстового поля.
func Detect(body []byte, pageURL string) (Candidate, bool) {
	base, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || base.Host == "" {
		return Candidate{}, false
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return Candidate{}, false
	}
	var cand Candidate
	found := false
	doc.Find("form").EachWithBreak(func(_ int, form *goquery.Selection) bool {
		if form.Find(`input[type="password"]`).Length() > 0 {
			return true
		}
		var field string
		form.Find("input[name]").EachWithBreak(func(_ int, inp *goquery.Selection) bool {
			t, _ := inp.Attr("type")
			t = strings.ToLower(strings.TrimSpace(t))
			name, _ := inp.Attr("name")
			name = strings.TrimSpace(name)
			if name == "" {
				return true
			}
			// Текстовое поле - сразу кандидат; скрытые поля пропускаем,
			// иначе возьмём csrf-токен вместо запроса.
			if t == "" || t == "text" || t == "search" {
				field = name
				return false
			}
			return true
		})
		if field == "" {
			return true
		}
		action, _ := form.Attr("action")
		target := resolveAction(base, strings.TrimSpace(action))
		if target == nil {
			return true
		}
		cand = Candidate{
			Name:     engineName(base.Host),
			Base:     base.Scheme + "://" + base.Host,
			Path:     target.Path + "?" + url.QueryEscape(field) + "={q}",
			Selector: "a[href*='.onion']",
		}
		found = true
		return false
	})
	return cand, found
}

func resolveAction(base *url.URL, action string) *url.URL {
	if action == "" {
		return &url.URL{Path: base.Path}
	}
	u, err := url.Parse(action)
	if err != nil {
		return nil
	}
	abs := base.ResolveReference(u)
	abs.RawQuery = ""
	abs.Fragment = ""
	// Форма обязана вести на тот же хост: иначе кандидат уводит поиск
	// на чужой сервис, а проверяли мы этот.
	if !strings.EqualFold(abs.Host, base.Host) {
		return nil
	}
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return nil
	}
	return abs
}

// engineName даёт имя из хоста: первая метка onion-адреса. Движки с одним
// именем перетирают друг друга в пуле здоровья, поэтому имя обязано быть
// непустым и детерминированным.
func engineName(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

// Verify прогоняет пробный запрос через кандидат и считает onion-ссылки.
// Порог - MinOnionHits: страница с одной ссылкой - не поисковик, а подборка.
func (p *Promoter) Verify(ctx context.Context, cand Candidate) (int, error) {
	if p.Client == nil {
		return 0, fmt.Errorf("promote: клиент не задан")
	}
	eng := &searchers.OnionEngine{
		Name_:    cand.Name,
		Base:     cand.Base,
		Path:     cand.Path,
		Selector: cand.Selector,
		Client:   p.Client,
	}
	vctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	res, err := eng.Search(vctx, "test", 20)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range res {
		if r.Onion {
			n++
		}
	}
	return n, nil
}

// Check забирает страницу сервиса и прогоняет detect+verify целиком.
// pageURL нормализуется: вызывающие передают store.Onion.URL, а пул хранит
// голые хосты без схемы. Без нормализации url.Parse не находит схему и запрос
// падает с «invalid URL scheme: []» - живой поисковик выглядит мёртвым и
// никогда не промоутится.
func (p *Promoter) Check(ctx context.Context, pageURL string) (Candidate, int, error) {
	if p.Client == nil {
		return Candidate{}, 0, fmt.Errorf("promote: клиент не задан")
	}
	pageURL = filex.NormalizeBase(pageURL)
	if pageURL == "" {
		return Candidate{}, 0, fmt.Errorf("promote: пустой адрес")
	}
	resp, err := p.Client.Fetch(ctx, httpc.Request{URL: pageURL, Method: http.MethodGet})
	if err != nil {
		return Candidate{}, 0, fmt.Errorf("promote: %w", err)
	}
	if !resp.OK() {
		return Candidate{}, 0, fmt.Errorf("promote: HTTP %d", resp.Status)
	}
	cand, ok := Detect(resp.Body, pageURL)
	if !ok {
		return Candidate{}, 0, fmt.Errorf("promote: формы поиска нет")
	}
	n, err := p.Verify(ctx, cand)
	if err != nil {
		return cand, 0, err
	}
	if n < p.minHits() {
		return cand, n, fmt.Errorf("promote: onion-ссылок %d, минимум %d", n, p.minHits())
	}
	return cand, n, nil
}

func (p *Promoter) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Infof(format, args...)
	}
}

// Attach поднимает кандидата в живой каталог: дописывает движок с клиентом
// и регистрирует в пуле здоровья. Дубликат по имени не дублируется.
// Возвращает false, если цеплять нечего или уже есть.
//
// Проверка дубликата и добавление делегированы catalog.AttachEngine, который
// выполняет их под одной блокировкой. Раньше они были раздельны: append в
// Engines шёл без мьютекса, пока поисковые запросы из goroutine MCP-сессий
// обходили тот же срез, и два одновременных Attach могли оба пройти проверку
// и оба добавить один движок.
func Attach(catalog *searchers.OnionCatalog, health *searchers.HealthPool, client *httpc.Client, cand Candidate) bool {
	if catalog == nil || cand.Name == "" || cand.Base == "" {
		return false
	}
	eng := &searchers.OnionEngine{
		Name_:    cand.Name,
		Base:     cand.Base,
		Path:     cand.Path,
		Selector: cand.Selector,
		Client:   client,
	}
	if !catalog.AttachEngine(eng) {
		return false
	}
	if health != nil {
		health.Register(eng)
	}
	return true
}
