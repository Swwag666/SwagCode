package classifier

// Поиск маркеров одним проходом по тексту.
//
// Прежняя версия прогоняла strings.Contains по каждому маркеру отдельно: до 34
// полных проходов по телу страницы. На 12 МБ это 8 мс, когда маркеров денег нет
// и scamCheck выходит рано, и 170 мс, когда маркер есть и проверяются все
// списки (замер BenchmarkClassifyLargePageWithCrypto). Автомат Ахо-Корасик
// делает один проход независимо от числа маркеров, поэтому стоимость перестаёт
// зависеть от того, какие именно слова попались в тексте.

// markerGroup - полуинтервал [lo, hi) в allMarkers.
type markerGroup struct {
	lo, hi int
}

// Списки маркеров. Порядок внутри списка значим: причина вердикта печатает
// первый найденный маркер, поэтому обход идёт по индексам группы, а не по
// порядку обнаружения в тексте.
var (
	moneyMarkers = []string{"crypto", "bitcoin", "investment", "инвестиц", "доход", "заработок", "крипт"}

	promiseMarkers = []string{
		"double your", "удвоим", "гарантированная прибыль", "guaranteed profit",
		"100% profit", "вернем x2", "send 0.1 get 0.2", "удвоение депозита",
	}

	pharmaMarkers = []string{"viagra", "cialis", "виагра", "купить диплом", "продам паспорт"}

	loginMarkers = []string{
		`type="password"`, `type='password'`, "forgot password",
		"sign in to", "log in to", "войти в аккаунт", "введите пароль",
	}

	paywallMarkers = []string{
		"paywall", "subscribe to read", "premium content", "платная подписка",
		"оформите подписку", "материал доступен по подписке", "unlock full access",
	}

	captchaMarkers = []string{
		"captcha", "cf-challenge", "just a moment", "attention required",
		"datadome", "prove you are human", "подтвердите, что вы не робот",
	}
)

var (
	allMarkers []string

	groupMoney   markerGroup
	groupPromise markerGroup
	groupPharma  markerGroup
	groupLogin   markerGroup
	groupPaywall markerGroup
	groupCaptcha markerGroup

	markerAC = newACScanner(allMarkerList())
)

// allMarkerList раскладывает группы в один плоский список и запоминает границы.
// Вызывается один раз при инициализации пакета.
func allMarkerList() []string {
	groups := []struct {
		list []string
		dst  *markerGroup
	}{
		{moneyMarkers, &groupMoney},
		{promiseMarkers, &groupPromise},
		{pharmaMarkers, &groupPharma},
		{loginMarkers, &groupLogin},
		{paywallMarkers, &groupPaywall},
		{captchaMarkers, &groupCaptcha},
	}
	var flat []string
	for _, g := range groups {
		lo := len(flat)
		flat = append(flat, g.list...)
		*g.dst = markerGroup{lo: lo, hi: len(flat)}
	}
	allMarkers = flat
	return flat
}

// markerHits - какие маркеры найдены в тексте. Индекс совпадает с позицией в
// allMarkers, поэтому проверка группы - это обход её полуинтервала.
type markerHits []bool

func (h markerHits) any(g markerGroup) bool {
	for i := g.lo; i < g.hi; i++ {
		if h[i] {
			return true
		}
	}
	return false
}

// first возвращает первый найденный маркер группы в порядке списка. Порядок
// важен для текста причины: он должен совпадать с прежней реализацией, которая
// обходила срез и брала первое совпадение.
func (h markerHits) first(g markerGroup) (string, bool) {
	for i := g.lo; i < g.hi; i++ {
		if h[i] {
			return allMarkers[i], true
		}
	}
	return "", false
}

func (h markerHits) count(g markerGroup) int {
	n := 0
	for i := g.lo; i < g.hi; i++ {
		if h[i] {
			n++
		}
	}
	return n
}

// scanMarkers возвращает набор маркеров, встречающихся в text. Текст должен
// быть в нижнем регистре: маркеры заданы строчными.
func scanMarkers(text string) markerHits {
	hits := make(markerHits, len(allMarkers))
	markerAC.scan(text, hits)
	return hits
}

// noEdge - часовое значение ребра префиксного дерева. Ноль использовать
// нельзя: он совпадает с идентификатором корня, и тогда «ребра нет» и «ребро
// ведёт в корень» становятся неразличимы.
const noEdge int32 = -1

// acScanner - детерминированный автомат Ахо-Корасик. Переходы по fail-ссылкам
// заполнены на этапе построения, поэтому сканирование текста - это один индекс
// на байт без обхода суффиксных ссылок.
type acScanner struct {
	dfa      []int32 // state*256 + byte -> следующее состояние
	terminal []bool  // завершается ли в состоянии хотя бы один маркер
	out      [][]int32
	states   int
}

func newACScanner(patterns []string) *acScanner {
	sc := &acScanner{}
	sc.grow() // состояние 0 - корень
	for b := 0; b < 256; b++ {
		sc.dfa[b] = noEdge
	}

	for id, p := range patterns {
		if p == "" {
			continue
		}
		state := int32(0)
		for i := 0; i < len(p); i++ {
			b := int(p[i])
			next := sc.dfa[int(state)*256+b]
			if next == noEdge {
				next = sc.grow()
				sc.dfa[int(state)*256+b] = next
			}
			state = next
		}
		sc.out[state] = append(sc.out[state], int32(id))
		sc.terminal[state] = true
	}

	sc.buildFail()
	return sc
}

// grow создаёт состояние и возвращает его идентификатор.
func (sc *acScanner) grow() int32 {
	id := int32(sc.states)
	sc.states++
	edge := make([]int32, 256)
	for b := range edge {
		edge[b] = noEdge
	}
	sc.dfa = append(sc.dfa, edge...)
	sc.terminal = append(sc.terminal, false)
	sc.out = append(sc.out, nil)
	return id
}

// buildFail достраивает fail-ссылки обходом в ширину, заменяет отсутствующие
// рёбра на переходы состояния-предка и сливает выводы: если один маркер
// является суффиксом другого, найдены должны быть оба.
//
// Обход в ширину гарантирует, что fail-ссылка состояния уже полностью
// построена к моменту, когда она используется как запасной переход.
func (sc *acScanner) buildFail() {
	fail := make([]int32, sc.states)
	queue := make([]int32, 0, sc.states)

	for b := 0; b < 256; b++ {
		if child := sc.dfa[b]; child != noEdge {
			fail[child] = 0
			queue = append(queue, child)
		} else {
			sc.dfa[b] = 0
		}
	}

	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		base := int(state) * 256
		failBase := int(fail[state]) * 256
		for b := 0; b < 256; b++ {
			child := sc.dfa[base+b]
			if child == noEdge {
				sc.dfa[base+b] = sc.dfa[failBase+b]
				continue
			}
			fail[child] = sc.dfa[failBase+b]
			if f := fail[child]; len(sc.out[f]) > 0 {
				sc.out[child] = append(sc.out[child], sc.out[f]...)
				sc.terminal[child] = true
			}
			queue = append(queue, child)
		}
	}
}

// scan проходит по тексту один раз и отмечает найденные маркеры.
func (sc *acScanner) scan(text string, hits markerHits) {
	state := int32(0)
	for i := 0; i < len(text); i++ {
		state = sc.dfa[int(state)*256+int(text[i])]
		if sc.terminal[state] {
			for _, id := range sc.out[state] {
				hits[id] = true
			}
		}
	}
}
