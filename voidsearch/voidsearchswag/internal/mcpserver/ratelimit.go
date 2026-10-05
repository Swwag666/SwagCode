package mcpserver

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimitPerMinDefault - молчаливый лимит запросов в минуту с одного IP.
// 120/мин хватает MCP-кгенту (initialize, tools/list, tools/call - единицы
// запросов за ход), а сканер или цикл перебора упирается в 429, не доходя
// ни до auth-сверки, ни до обработчиков.
const RateLimitPerMinDefault = 120

// maxTrackedIPs - потолок карты IP. Открытый стенд видит сканеры с тысяч
// адресов; без потолка карта росла бы всю жизнь процесса. При превышении
// карта чистится от неактивных, а если неактивных нет - выкидывается
// произвольная жертва: лимит - защита сервера, а не гарантия справедливости.
const maxTrackedIPs = 8192

// rateLimiter - скользящее окно по IP без внешних зависимостей. На каждый
// запрос в окно IP дописывается отметка времени; отметки старше окна
// отрезаются. Запрос сверх лимита в окне получает 429.
//
// X-Forwarded-For сознательно не читается: за реверс-прокси без
// доверенной конфигурации заголовок подделывается клиентом, и подделанный
// адрес обходил бы лимит бесплатно. Дефолт честный: все клиенты за прокси
// делят квоту его адреса.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events map[string][]time.Time
	// now нужен для тестов: окно скользится по часам, а тест подменяет
	// их, чтобы не спать по секунде на каждый случай.
	now func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	if limit < 0 {
		limit = 0
	}
	if window <= 0 {
		window = time.Minute
	}
	return &rateLimiter{
		limit:  limit,
		window: window,
		events: make(map[string][]time.Time),
		now:    time.Now,
	}
}

// request пропускает запрос и запоминает его либо отказывает, если окно IP
// переполнено. Второе значение - сколько ждать до ближайшего свободного
// слота (ноль, если пропустил). Один вызов - один лок: карта IP и окно
// атомарны относительно конкурентных запросов.
func (l *rateLimiter) request(ip string, at time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 0 - лимит выключен: молчаливый проход, отказ «ноль запросов» никому
	// не нужен.
	if l.limit == 0 {
		return true, 0
	}
	_ = ip

	cutoff := at.Add(-l.window)
	evs, known := l.events[ip]
	if known {
		// Отрезаем протухшие отметки на месте: слайс общий, хвост жив.
		keep := 0
		for keep < len(evs) && !evs[keep].After(cutoff) {
			keep++
		}
		evs = evs[keep:]
	} else if len(l.events) >= maxTrackedIPs {
		l.evictIPs(cutoff)
		if len(l.events) >= maxTrackedIPs {
			// Неактивных нет - жертвуем произвольной записью: карта не
			// должна расти бесконечно, лимит защищает сервер.
			for victim := range l.events {
				delete(l.events, victim)
				break
			}
		}
	}
	if len(evs) < l.limit {
		l.events[ip] = append(evs, at)
		return true, 0
	}
	// Отказ: свободный слот появится, когда самая старая отметка окна
	// выйдет за границу.
	retry := evs[0].Add(l.window).Sub(at)
	if retry < 0 {
		retry = 0
	}
	l.events[ip] = evs
	return false, retry
}

// evictIPs выкидывает IP, чьи окна давно пусты. Вызывается под локом
// request при появлении нового IP у полной карты.
func (l *rateLimiter) evictIPs(cutoff time.Time) {
	for ip, evs := range l.events {
		if len(evs) == 0 || !evs[len(evs)-1].After(cutoff) {
			delete(l.events, ip)
		}
	}
}

// clientIP достаёт адрес клиента из RemoteAddr без порта. Неопознанный
// адрес складывается в общий бакет «unknown»: ошибиться в сторону общего
// ключа безопаснее, чем пропустить запрос без учёта.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}

// rateLimitMiddleware оборачивает обработчик лимитом. Отказ отдаётся до
// auth и до чтения тела: флуд не должен тратить CPU на сверку Bearer и
// MaxBytesReader. Retry-After честный - до ближайшего свободного слота.
func rateLimitMiddleware(l *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l == nil || l.limit == 0 {
			next.ServeHTTP(w, r)
			return
		}
		ok, retry := l.request(clientIP(r), l.now())
		if !ok {
			secs := int(retry.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			http.Error(w, "слишком много запросов", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
