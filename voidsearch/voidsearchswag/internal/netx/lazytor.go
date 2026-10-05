package netx

import (
	"context"
	"sync"
)

// lazyRotator поднимает tor при первом настоящем использовании, а не при
// построении ядра.
//
// Причина в том, что быстрый режим не нуждается в tor. DuckDuckGo и SearXNG
// ходят через прямой клиент (search.Build создаёт его отдельно, когда задан
// ротатор), а onion-движки в быстром режиме не вызываются вовсе. Ротатор
// дёргается только в searchDeep: там создаются сессии tor-клиента и вызывается
// Rotate. Значит при явном -mode fast tor запускался, проходил bootstrap
// 20-25 секунд и не использовался ни разу.
//
// Просто не запускать tor в быстром режиме нельзя: FallbacksFor(ModeFast)
// возвращает {ModeStealth, ModeDeep}, то есть быстрый поиск, не нашедший
// ничего, проваливается в deep, которому tor нужен. Ленивый запуск сохраняет
// эту возможность и убирает стоимость, когда она не нужна: фолбэк до deep
// поднимет tor в момент, когда он действительно потребуется.
//
// Методы состояния (Kind, TransportSpec, Healthy, Rotate, ControlStatus)
// запускают tor, потому что вызываются ровно тогда, когда транспорт
// действительно нужен: TransportSpec - при создании сессии tor-клиента, Rotate -
// при смене личности в deep-поиске, ControlStatus - из команды onioncheck,
// которая существует чтобы tor проверять.
//
// Close не запускает tor: закрывать нечего, а поднимать процесс ради того чтобы
// его тут же закрыть, бессмысленно.
type lazyRotator struct {
	mu    sync.Mutex
	cfg   Config
	inner Rotator
	err   error
	tried bool
	log   Logger
}

// LazyTor возвращает ротатор, который поднимет tor по конфигурации cfg при
// первом обращении к транспорту.
//
// Функция не возвращает ошибку намеренно: ошибка запуска tor становится
// известна только в момент, когда транспорт действительно понадобился, и
// сообщается через Healthy и Rotate. Это согласуется с прежним поведением, где
// неудачный StartTor в buildEngine приводил к nil-ротатору и прямому клиенту:
// onion-запросы уходили в clearnet и падали, а поиск деградировал.
func LazyTor(cfg Config) Rotator {
	return &lazyRotator{cfg: cfg, log: cfg.Logger}
}

// ensure поднимает tor один раз и запоминает результат, включая ошибку.
//
// Повторная попытка после ошибки не делается: StartTor стоит 20-25 секунд, и
// повторный запуск на каждый запрос превратил бы быстрый поиск в медленный.
// Ошибка запоминается, а вызывающий получает её через Healthy и Rotate.
func (l *lazyRotator) ensure() (Rotator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tried {
		return l.inner, l.err
	}
	l.tried = true
	if l.log != nil {
		l.log.Infof("поднимаю tor по требованию поиска")
	}
	r, err := StartTor(l.cfg)
	if err != nil {
		l.err = err
		return nil, err
	}
	l.inner = r
	return r, nil
}

func (l *lazyRotator) Kind() string {
	r, err := l.ensure()
	if err != nil {
		return ""
	}
	return r.Kind()
}

func (l *lazyRotator) TransportSpec() string {
	r, err := l.ensure()
	if err != nil {
		return ""
	}
	return r.TransportSpec()
}

func (l *lazyRotator) Rotate(ctx context.Context) error {
	r, err := l.ensure()
	if err != nil {
		return err
	}
	return r.Rotate(ctx)
}

// Healthy сообщает о состоянии tor. При неудачном запуске возвращает false,
// потому что транспортом пользоваться нельзя.
func (l *lazyRotator) Healthy() bool {
	r, err := l.ensure()
	if err != nil {
		return false
	}
	return r.Healthy()
}

// Close не поднимает tor: закрывать нечего. Если tor уже поднят, он
// закрывается; если нет, вызов ничего не делает и не возвращает ошибку,
// потому что отсутствие процесса - не сбой.
func (l *lazyRotator) Close() error {
	l.mu.Lock()
	inner := l.inner
	l.mu.Unlock()
	if inner == nil {
		return nil
	}
	return inner.Close()
}

// Started сообщает, поднимался ли tor. Используется тестами и диагностикой,
// чтобы отличить «tor не нужен» от «tor поднят».
func (l *lazyRotator) Started() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tried
}

// ControlStatus реализует тот же опциональный интерфейс, что torRotator и
// externalRotator. Без него torStatusLine в CLI показал бы только адрес или
// «control не задан», хотя ленивый ротатор способен дать проверенный статус.
//
// Вызов поднимает tor. Это правильно для onioncheck, единственного потребителя
// метода: команда существует, чтобы проверять tor, и статус без запущенного
// tor был бы бессмысленным.
func (l *lazyRotator) ControlStatus() string {
	r, err := l.ensure()
	if err != nil {
		return "не поднят: " + err.Error()
	}
	if cs, ok := r.(interface{ ControlStatus() string }); ok {
		return cs.ControlStatus()
	}
	return "подключён"
}

// ControlAddr реализует опциональный интерфейс, который torStatusLine
// использует как запасной источник адреса.
func (l *lazyRotator) ControlAddr() string {
	l.mu.Lock()
	inner := l.inner
	l.mu.Unlock()
	if inner == nil {
		return ""
	}
	if ca, ok := inner.(interface{ ControlAddr() string }); ok {
		return ca.ControlAddr()
	}
	return ""
}
