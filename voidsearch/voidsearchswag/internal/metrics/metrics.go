// Package metrics - счётчики долгоживущего сервера. Один глобальный реестр
// (Default) плюс возможность подменить свой в тестах. Только инкременты:
// кардинальность ключей ограничена (режимы, имена движков), утечки памяти
// по произвольным строкам исключены конструкцией вызовов.
package metrics

import (
	"sort"
	"strings"
	"sync"
)

type Counters struct {
	mu sync.Mutex
	m  map[string]int64
}

var Default = New()

func New() *Counters { return &Counters{m: map[string]int64{}} }

// Inc добавляет 1 к счётчику key. Пустой ключ игнорируется.
func (c *Counters) Inc(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	c.m[key]++
	c.mu.Unlock()
}

// Add добавляет n к счётчику key. Неположительное n игнорируется.
func (c *Counters) Add(key string, n int64) {
	if c == nil || key == "" || n <= 0 {
		return
	}
	c.mu.Lock()
	c.m[key] += n
	c.mu.Unlock()
}

// Snapshot возвращает копию всех счётчиков. Копия, а не ссылка: вызывающий
// не сможет испортить реестр Marshal'ом или сортировкой.
func (c *Counters) Snapshot() map[string]int64 {
	if c == nil {
		return map[string]int64{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// Key склеивает части ключа через подчёркивание, выкидывая мусор:
// имена движков идут в ключи, и произвольные символы там не нужны.
func Key(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		var b strings.Builder
		for _, r := range p {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r == '-' || r == '_' || r == ' ' || r == '.':
				b.WriteRune('_')
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return strings.Join(out, "_")
}

// Names возвращает отсортированные имена счётчиков: стабильный порядок
// для /metrics и детерминированные тесты.
func (c *Counters) Names() []string {
	snap := c.Snapshot()
	names := make([]string, 0, len(snap))
	for k := range snap {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
