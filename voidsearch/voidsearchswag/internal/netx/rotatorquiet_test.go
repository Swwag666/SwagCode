package netx

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// countLog считает строки, дошедшие до лога ротатора.
type countLog struct {
	mu   sync.Mutex
	rows []string
}

func (l *countLog) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, "info: "+fmt.Sprintf(format, args...))
}

func (l *countLog) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, "warn: "+fmt.Sprintf(format, args...))
}

func (l *countLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.rows))
	copy(out, l.rows)
	return out
}

// Смоук I-Q10/H-Q11 (этап 178): пустой control-адрес - свойство
// конфигурации, а не событие. Прежде «control-адрес tor не задан» писалась в
// лог каждой ротацией, и следом Rotate дублировал своей - по две идентичные
// строки на каждый оборот при живом транспорте. Теперь причина выходит один
// раз за жизнь процесса, а Rotate без control-порта молча возвращает nil:
// смена цепи невозможна, но транспорт работает.
func TestExternalRotateNoControlQuiet(t *testing.T) {
	lg := &countLog{}
	tor := &externalRotator{
		socksAddr: "127.0.0.1:9050",
		ctrlAddr:  "",
		timeout:   2 * time.Second,
		cooldown:  0,
		log:       lg,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := tor.Rotate(ctx); err != nil {
			t.Fatalf("ротация %d вернула ошибку: %v", i+1, err)
		}
	}
	// Причина названа ровно один раз: noCtrlOnce гасит повторы, Rotate
	// после первого объяснения молчит.
	n := 0
	for _, row := range lg.lines() {
		if strings.Contains(row, "control-адрес tor не задан") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("сообщение о пустом control-адресе вышло %d раз, хочу 1: %v", n, lg.lines())
	}
}

// Второй инстанс ротатора с тем же пустым адресом стартует с чистым
// noCtrlOnce: сообщение выйдет и у него, по одному на процесс.
func TestExternalRotateNoControlOncePerProcess(t *testing.T) {
	for i := 0; i < 2; i++ {
		lg := &countLog{}
		tor := &externalRotator{
			socksAddr: "127.0.0.1:9050", ctrlAddr: "", timeout: time.Second, log: lg,
		}
		if err := tor.Rotate(context.Background()); err != nil {
			t.Fatalf("инстанс %d: %v", i, err)
		}
		if n := len(lg.lines()); n != 1 {
			t.Errorf("инстанс %d: строк лога %d, хочу 1: %v", i, n, lg.lines())
		}
	}
}
