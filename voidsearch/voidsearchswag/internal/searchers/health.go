package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"voidsearchswag/internal/httpc"
)

const (
	healthFailThreshold = 3
	healthReturnScore   = 0.5
)

type EngineHealth struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Live        bool      `json:"live"`
	LatencyAvg  int64     `json:"latency_avg_ms"`
	SuccessRate float64   `json:"success_rate"`
	FailStreak  int       `json:"fail_streak"`
	Probes      int       `json:"probes"`
	Successes   int       `json:"successes"`
	LastProbe   time.Time `json:"last_probe"`
	LastError   string    `json:"last_error,omitempty"`
	Disabled    bool      `json:"disabled"`
}

type HealthPool struct {
	mu      sync.Mutex
	health  map[string]*EngineHealth
	client  *httpc.Client
	timeout time.Duration
	persist func(EngineHealth)
}

// SetPersist задаёт колбэк сохранения снимка после каждого обновления.
// Пишет вызывающий слой (обычно SQLite через store): сам пул про
// хранилище ничего не знает и зависимостей не тянет.
func (h *HealthPool) SetPersist(fn func(EngineHealth)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.persist = fn
}

// Restore подтягивает статистику с прошлого запуска: счётчики проб,
// среднюю латентность и флаг отключения. Неизвестные имена игнорируются:
// список движков мог смениться. Возвращает число восстановленных.
func (h *HealthPool) Restore(list []EngineHealth) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range list {
		st, ok := h.health[e.Name]
		if !ok || e.Name == "" {
			continue
		}
		st.URL = e.URL
		st.Live = e.Live
		st.LatencyAvg = e.LatencyAvg
		st.SuccessRate = e.SuccessRate
		st.FailStreak = e.FailStreak
		st.Probes = e.Probes
		st.Successes = e.Successes
		st.LastProbe = e.LastProbe
		st.Disabled = e.Disabled
		n++
	}
	return n
}

// Register добавляет движок в пул на лету (автопромоут): уже известные
// имена не трогает. Возвращает false, если движок уже был.
func (h *HealthPool) Register(e *OnionEngine) bool {
	if e == nil || e.Name() == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.health[e.Name()]; ok {
		return false
	}
	h.health[e.Name()] = &EngineHealth{Name: e.Name(), URL: e.ProbeURL()}
	return true
}

// под мьютексом тормозила бы весь параллельный опрос.
// save копирует снимок и зовёт persist уже без блокировки: SQLite-запись
// под мьютексом тормозила бы весь параллельный опрос.
func (h *HealthPool) save(name string) {
	h.mu.Lock()
	st, ok := h.health[name]
	fn := h.persist
	var snap EngineHealth
	if ok {
		snap = *st
	}
	h.mu.Unlock()
	if ok && fn != nil {
		fn(snap)
	}
}

func NewHealthPool(client *httpc.Client, engines []*OnionEngine) *HealthPool {
	hp := &HealthPool{
		health:  make(map[string]*EngineHealth, len(engines)),
		client:  client,
		timeout: 45 * time.Second,
	}
	for _, e := range engines {
		hp.health[e.Name()] = &EngineHealth{Name: e.Name(), URL: e.ProbeURL()}
	}
	return hp
}

// probeOutcome - чем закончилась одна проверка движка. Отдельный исход для
// отсутствия транспорта нужен потому, что он не говорит о здоровье движка
// ничего: onion-адрес без tor недостижим в принципе.
type probeOutcome int

const (
	// probeOK - движок ответил.
	probeOK probeOutcome = iota
	// probeFailed - движок не ответил: ошибка сети, HTTP >= 400 или пустое тело.
	probeFailed
	// probeNoTransport - проверка не состоялась, нужен tor или прокси.
	probeNoTransport
)

// ProbeReport - чем закончился обход onion-движков. Отчёт живёт только в
// памяти и в базе не хранится: пропуск проверки не является фактом о движке,
// поэтому писать его в engine_health незачем.
type ProbeReport struct {
	// Total - сколько движков обходилось.
	Total int `json:"total"`
	// Probed - проверки, которые состоялись: движок ответил или отказал.
	Probed int `json:"probed"`
	// Skipped - проверки, которые не состоялись из-за отсутствия tor или прокси.
	Skipped int `json:"skipped"`
	// Failed - движки, которые не ответили.
	Failed int `json:"failed"`
}

// Note поясняет отчёт одной строкой для CLI и MCP. Пустая строка означает, что
// пояснять нечего: все проверки состоялись.
func (r ProbeReport) Note() string {
	if r.Skipped == 0 {
		return ""
	}
	return fmt.Sprintf("пропущено %d из %d проверок: нужен tor или прокси, состояние движков не менялось",
		r.Skipped, r.Total)
}

func (h *HealthPool) ProbeAll(ctx context.Context, engines []*OnionEngine) ProbeReport {
	var wg sync.WaitGroup
	var mu sync.Mutex
	rep := ProbeReport{Total: len(engines)}
	for _, e := range engines {
		wg.Add(1)
		go func(e *OnionEngine) {
			defer wg.Done()
			outcome := h.probeOne(ctx, e)
			mu.Lock()
			switch outcome {
			case probeNoTransport:
				rep.Skipped++
			case probeFailed:
				rep.Failed++
				rep.Probed++
			case probeOK:
				rep.Probed++
			}
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	return rep
}

func (h *HealthPool) probeOne(ctx context.Context, e *OnionEngine) probeOutcome {
	pctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	start := time.Now()
	resp, err := e.Client.Fetch(pctx, httpc.Request{URL: e.ProbeURL(), Method: http.MethodGet})
	latency := time.Since(start).Milliseconds()

	if errors.Is(err, httpc.ErrOnionWithoutTor) {
		// Проверка не состоялась, а не провалилась. Счётчики не трогаются и в
		// базу ничего не пишется: прежний код увеличивал FailStreak и снижал
		// пожизненную SuccessRate, поэтому три вызова onioncheck --no-tor
		// выключали все движки навсегда. Disabled снимается только при Live и
		// SuccessRate >= 0.5, а Live без tor не появится и успех уже не вернуть:
		// у tornet он падал 90% -> 87% -> 84% -> 82% на ровном месте.
		return probeNoTransport
	}

	h.mu.Lock()
	st, ok := h.health[e.Name()]
	if !ok {
		st = &EngineHealth{Name: e.Name(), URL: e.ProbeURL()}
		h.health[e.Name()] = st
	}
	st.Probes++
	st.LastProbe = time.Now()

	if err != nil {
		st.FailStreak++
		st.Live = false
		st.LastError = trimErr(err)
		h.recompute(st)
		name := e.Name()
		h.mu.Unlock()
		h.save(name)
		return probeFailed
	}
	if resp.Status >= 400 || len(resp.Body) == 0 {
		st.FailStreak++
		st.Live = false
		// Числовой код в отчёте важнее текста: 403 и 429 читаются как
		// «доступ закрыт» и «квота», а текстовые названия разных версий
		// net/http расходятся. Формат совпадает с OnionEngine.Search.
		st.LastError = fmt.Sprintf("HTTP %d", resp.Status)
		h.recompute(st)
		name := e.Name()
		h.mu.Unlock()
		h.save(name)
		return probeFailed
	}
	st.Successes++
	st.FailStreak = 0
	st.Live = true
	st.LastError = ""
	if st.LatencyAvg == 0 {
		st.LatencyAvg = latency
	} else {
		st.LatencyAvg = (st.LatencyAvg*3 + latency) / 4
	}
	h.recompute(st)
	name := e.Name()
	h.mu.Unlock()
	h.save(name)
	return probeOK
}

func (h *HealthPool) recompute(st *EngineHealth) {
	if st.Probes > 0 {
		st.SuccessRate = float64(st.Successes) / float64(st.Probes)
	}
	if st.FailStreak >= healthFailThreshold {
		st.Disabled = true
	}
	if st.Live && st.SuccessRate >= healthReturnScore {
		st.Disabled = false
	}
}

func (h *HealthPool) Live() []EngineHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]EngineHealth, 0, len(h.health))
	for _, st := range h.health {
		if st.Live && !st.Disabled {
			out = append(out, *st)
		}
	}
	sortByScore(out)
	return out
}

func (h *HealthPool) All() []EngineHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]EngineHealth, 0, len(h.health))
	for _, st := range h.health {
		out = append(out, *st)
	}
	sortByScore(out)
	return out
}

func (h *HealthPool) Get(name string) (EngineHealth, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.health[name]
	if !ok {
		return EngineHealth{}, false
	}
	return *st, true
}

func (h *HealthPool) MarkResult(name string, ok bool, latency time.Duration) {
	h.mu.Lock()
	st, exists := h.health[name]
	if !exists {
		h.mu.Unlock()
		return
	}
	st.Probes++
	st.LastProbe = time.Now()
	if ok {
		st.Successes++
		st.FailStreak = 0
		st.Live = true
		ms := latency.Milliseconds()
		if st.LatencyAvg == 0 {
			st.LatencyAvg = ms
		} else {
			st.LatencyAvg = (st.LatencyAvg*3 + ms) / 4
		}
	} else {
		st.FailStreak++
		if st.FailStreak >= healthFailThreshold {
			st.Live = false
			st.Disabled = true
		}
	}
	h.recompute(st)
	h.mu.Unlock()
	h.save(name)
}

func (h *HealthPool) HealthyCount() (live, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range h.health {
		total++
		if st.Live && !st.Disabled {
			live++
		}
	}
	return live, total
}

func sortByScore(list []EngineHealth) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if b.SuccessRate > a.SuccessRate ||
				(b.SuccessRate == a.SuccessRate && b.LatencyAvg < a.LatencyAvg) {
				list[j-1], list[j] = b, a
				continue
			}
			break
		}
	}
}

func trimErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120])
	}
	return s
}
