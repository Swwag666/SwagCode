package httpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// silentProxy поднимает TCP-слушатель, который принимает соединение и не
// отвечает: ровно так ведёт себя socks-прокси, у которого пропал выход или
// который просто не слушается отмену контекста.
func silentProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

// Отмена контекста обязана возвращать управление вызывающему, даже когда
// транспорт её игнорирует.
//
// Живой замер ДО на копии базы %TEMP%\vss\livedata103: hunt watch --id 2
// --timeout 5s при транспорте static с прокси 192.0.2.1:9999 длился 12147 мс и
// вернул elapsed=8.935s вместо пяти секунд, а одиночный search в тех же
// условиях - 17184 мс при REQUEST_TIMEOUT=12s. Причина: socks-диалер, который
// net/http и tls-client используют для схемы socks5, не слушает ctx.Done() и
// висит до собственного таймаута, поэтому дедлайн ожидания охоты не прерывал
// прогон.
func TestFetchReturnsOnContextCancel(t *testing.T) {
	c, err := NewClient(context.Background(), Options{
		Transport: "static",
		Proxies:   []string{silentProxy(t)},
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	before, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = c.Fetch(ctx, Request{URL: "https://example.com/", Method: "GET"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("запрос через молчащий прокси удался")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("отмена вернула %v, хочу context.DeadlineExceeded", err)
	}
	// Порог в 450 мс различает один оборванный запрос и два: без проверки
	// истёкшего контекста Fetch ушёл бы в эскалацию и потратил ещё 300 мс на
	// DoStd, а вместе с ним - лишнюю сессию и лишнее зависшее рукопожатие.
	if elapsed > 450*time.Millisecond {
		t.Errorf("Fetch длился %s при отмене через 300ms: транспорт не слушает ctx", elapsed)
	}
	// Эскалация на истёкшем контексте стоит не времени, а побочных эффектов:
	// ротация закрывает прежнюю сессию и создаёт новую, то есть плодит ещё одно
	// зависшее socks-рукопожатие, которое никто уже не ждёт.
	if c.sess != before {
		t.Error("эскалация на истёкшем контексте пересоздала сессию")
	}
}

// Прямой путь DoStd проверяется отдельно и на https-адресе: он уходит через
// impersonate, а не через net/http, и отмена там своя. На http:// запрос пошёл
// бы через plain-клиент, который слушает контекст сам, и обёртка оказалась бы
// неразличимой.
//
// Дедлайн в 50ms выбран замером, а не на глаз. Исходные 300ms совпадали по порядку
// с собственным сроком socks-рукопожатия, и под нагрузкой транспорт успевал
// отказаться раньше: из двадцати повторов полного набора один вернул «Get
// "https://example.com/": socks connect tcp ... i/o timeout» вместо ошибки
// контекста, хотя обёртка вела себя правильно - контекст к тому моменту ещё не
// истёк, и подменять настоящую причину отказа она не имела права. Замер на HEAD
// f5fca08, одиночный прогон, по двадцать повторов на каждый дедлайн:
//
//	дедлайн 300ms: самый долгий прогон 300.8755ms
//	дедлайн 100ms: самый долгий прогон 100.971ms
//	дедлайн 50ms:  самый долгий прогон 51.2832ms
//
// На 50ms отмена всегда приходит первой, а на 300ms - как повезёт. Повторов
// двадцать, чтобы редкая гонка выпадала внутри одного прогона, а не раз в сутки.
func TestDoStdReturnsOnContextCancel(t *testing.T) {
	for i := 0; i < 20; i++ {
		c, err := NewClient(context.Background(), Options{
			Transport: "static",
			Proxies:   []string{silentProxy(t)},
			Timeout:   3 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}

		sess, err := c.ensure(context.Background())
		if err != nil {
			c.Close()
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		_, err = sess.DoStd(ctx, Request{URL: "https://example.com/", Method: "GET"})
		elapsed := time.Since(start)
		cancel()
		c.Close()

		if err == nil {
			t.Fatalf("прогон %d: запрос через молчащий прокси удался", i+1)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("прогон %d: отмена вернула %v, хочу context.DeadlineExceeded", i+1, err)
		}
		if elapsed > 1500*time.Millisecond {
			t.Fatalf("прогон %d: DoStd длился %s при отмене через 50ms", i+1, elapsed)
		}
	}
}
