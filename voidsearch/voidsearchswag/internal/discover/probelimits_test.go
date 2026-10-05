package discover

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Живой замер до правки на HEAD 45c45db, пересобранный бинарник, копия боевой
// базы, тор выключен, транспорт direct:
//
//	probe --limit 3 --json
//	    rc=1, ключи: dead, elapsed, live, rejected, results, skipped, total
//	    total=3 live=0 skipped=3 limit_hit=''
//	probe --limit 3
//	    пробы: живых 0 из 3 (пропущено 3: нужен tor или прокси) за 1ms
//
// Отчёт называл итог волны, но не называл ни запрошенную выборку, ни
// параллельность, ни таймаут, ни паузу, поэтому «живых 0 из 3» не отличалось от
// «живых 0 из 3 при выборке три на очередь в одиннадцать тысяч адресов».
func TestProbeReportNamesAppliedLimits(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{
		Concurrency: 4, Timeout: time.Second, Delay: time.Millisecond,
	})
	rep := p.Probe(context.Background(), []string{v2a + ".onion"})
	if rep.Concurrency != 4 {
		t.Errorf("отчёт назвал concurrency=%d, хочу 4 из конфига", rep.Concurrency)
	}
	if rep.TimeoutMS != 1000 {
		t.Errorf("отчёт назвал timeout_ms=%d, хочу 1000", rep.TimeoutMS)
	}
	if rep.DelayMS != 1 {
		t.Errorf("отчёт назвал delay_ms=%d, хочу 1", rep.DelayMS)
	}
}

// Отчёт обязан называть применённое значение, а не запрошенное: withDefaults
// подменяет неположительные параллельность и таймаут, и именно эту подмену
// оператор не видел.
func TestProbeReportNamesSubstitutedDefaults(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{})
	rep := p.Probe(context.Background(), []string{v2a + ".onion"})
	want := ProbeConfig{}.withDefaults()
	if rep.Concurrency != want.Concurrency {
		t.Errorf("отчёт назвал concurrency=%d, хочу %d из withDefaults", rep.Concurrency, want.Concurrency)
	}
	if rep.TimeoutMS != want.Timeout.Milliseconds() {
		t.Errorf("отчёт назвал timeout_ms=%d, хочу %d из withDefaults", rep.TimeoutMS, want.Timeout.Milliseconds())
	}
	if rep.DelayMS != 0 {
		t.Errorf("отчёт назвал delay_ms=%d, хочу 0: пауза по умолчанию не задана", rep.DelayMS)
	}
}

// Пределы заполняются до раннего возврата, то есть и тогда, когда вся выборка
// отброшена и волна не состоялась. Именно там «живых 0 из 0» без пределов
// читалось как пустая очередь.
func TestProbeReportNamesLimitsWhenAllRejected(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{Concurrency: 2, Timeout: time.Second, Delay: time.Millisecond})
	rep := p.Probe(context.Background(), []string{"", "example.com"})
	if rep.Total != 0 {
		t.Fatalf("total=%d, хочу 0: волна не состоялась", rep.Total)
	}
	if rep.Rejected != 2 {
		t.Errorf("rejected=%d, хочу 2", rep.Rejected)
	}
	if rep.Concurrency != 2 || rep.TimeoutMS != 1000 || rep.DelayMS != 1 {
		t.Errorf("на отброшенной выборке пределы не названы: concurrency=%d timeout_ms=%d delay_ms=%d",
			rep.Concurrency, rep.TimeoutMS, rep.DelayMS)
	}
}

// Запрошенная выборка обязана доезжать до отчёта и на пустой очереди, и на
// состоявшейся волне: без неё «живых 0 из 0» не отличить от «лимит ноль».
func TestProbeWaveReportNamesRequestedLimit(t *testing.T) {
	st := newStore(t)
	p := NewProber(nil, st, nil, ProbeConfig{})

	rep, err := p.ProbeWave(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RequestedLimit != 7 {
		t.Errorf("на пустой очереди requested_limit=%d, хочу 7", rep.RequestedLimit)
	}

	ctx := context.Background()
	putOnion(t, st, "http://"+v2a+".onion/", "unknown", 0, 0)
	putOnion(t, st, "http://"+v2b+".onion/", "unknown", 0, 0)
	wave, err := p.ProbeWave(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if wave.RequestedLimit != 3 {
		t.Errorf("на состоявшейся волне requested_limit=%d, хочу 3", wave.RequestedLimit)
	}
	if wave.Total != 2 {
		t.Errorf("total=%d, хочу 2: в очереди два непроверенных адреса", wave.Total)
	}
	if wave.Concurrency != 8 {
		t.Errorf("concurrency=%d, хочу 8 из withDefaults", wave.Concurrency)
	}
}

// Поля обязаны доезжать до JSON под своими именами: отчёт читают машины, и ключ
// без тега или с другим именем исчез бы из ответа.
func TestProbeReportJSONCarriesAppliedLimits(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{Concurrency: 4, Timeout: 2 * time.Second, Delay: 5 * time.Millisecond})
	rep := p.Probe(context.Background(), []string{v2a + ".onion"})
	rep.RequestedLimit = 9
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("отчёт не сериализуется: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		`"requested_limit":9`, `"concurrency":4`, `"timeout_ms":2000`, `"delay_ms":5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в JSON отчёта нет %s, фактически: %s", want, firstNRunesProbe(body, 400))
		}
	}
}

func firstNRunesProbe(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Этап 178 (смоук-H): волна с пределами вызова обязана применять их, а не
// серверные. Копия пробера наследует клиента, базу и лог, но конфиг и
// лимитер - свои: новый лимитер не должен тратить прогрев родителя на паузы
// волны вызова.
func TestWithWaveLimitsAppliesCallLimits(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{
		Concurrency: 8, Timeout: 20 * time.Second, Delay: 2 * time.Second,
	})
	w := p.WithWaveLimits(2000, 100, 3)
	if w.Cfg.Timeout != 2*time.Second {
		t.Errorf("timeout волны = %v, хочу 2s из вызова", w.Cfg.Timeout)
	}
	if w.Cfg.Delay != 100*time.Millisecond {
		t.Errorf("delay волны = %v, хочу 100ms из вызова", w.Cfg.Delay)
	}
	if w.Cfg.Concurrency != 3 {
		t.Errorf("concurrency волны = %d, хочу 3 из вызова", w.Cfg.Concurrency)
	}
	if w == p {
		t.Fatal("копия не создана: волна подменила бы пределы серверу для всех")
	}
	if p.Cfg.Timeout != 20*time.Second || p.Cfg.Concurrency != 8 {
		t.Errorf("пределы родителя подменены: %+v", p.Cfg)
	}
}

// Нулевые и отрицательные пределы остаются серверными: пустой timeout_ms
// в вызове - не команда отключить таймаут, а «не задан».
func TestWithWaveLimitsKeepsServerDefaultsWhenUnset(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{
		Concurrency: 8, Timeout: 20 * time.Second, Delay: 2 * time.Second,
	})
	w := p.WithWaveLimits(0, -1, 0)
	if w.Cfg.Timeout != 20*time.Second {
		t.Errorf("timeout = %v, хочу серверные 20s", w.Cfg.Timeout)
	}
	if w.Cfg.Delay != 2*time.Second {
		t.Errorf("delay = %v, хочу серверные 2s", w.Cfg.Delay)
	}
	if w.Cfg.Concurrency != 8 {
		t.Errorf("concurrency = %d, хочу серверные 8", w.Cfg.Concurrency)
	}
}

// Отчёт копии называет пределы волны, а не серверные: именно их смоук
// сверял с переданными timeout_ms/delay_ms.
func TestWithWaveLimitsReportCarriesCallLimits(t *testing.T) {
	p := NewProber(nil, nil, nil, ProbeConfig{
		Concurrency: 8, Timeout: 20 * time.Second, Delay: 2 * time.Second,
	})
	w := p.WithWaveLimits(2000, 100, 4)
	rep := w.Probe(context.Background(), []string{v2a + ".onion"})
	if rep.TimeoutMS != 2000 || rep.DelayMS != 100 || rep.Concurrency != 4 {
		t.Errorf("отчёт волны не называет пределы вызова: timeout_ms=%d delay_ms=%d concurrency=%d",
			rep.TimeoutMS, rep.DelayMS, rep.Concurrency)
	}
}

// Лимитер копии стартует с нуля: пауза первой же пробы волны вызова не
// тратится на «прогретое ожидание» родителя, иначе timeout_ms=2000 при
// серверной паузе 2s выглядел бы как мгновенный отказ всех хостов.
func TestWithWaveLimitsFreshLimiter(t *testing.T) {
	base := ProbeConfig{Concurrency: 2, Timeout: 2 * time.Second, Delay: time.Hour}
	p := NewProber(nil, nil, nil, base)
	w := p.WithWaveLimits(2000, 10, 2)
	// Первая пауза свежего лимитера обязана быть мала; берём запасной
	// порог в 50ms против 10ms номинала, чтобы не мерить ровно.
	start := time.Now()
	_ = w.Limiter.Wait(context.Background(), "x.onion")
	d := time.Since(start)
	if d > 500*time.Millisecond {
		t.Errorf("лимитер копии ждал %v: похоже на наследование паузы %v родителя", d, base.Delay)
	}
}
