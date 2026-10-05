package httpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Замер до правки на HEAD a73f53e. Таймаут сессии передавался транспортам -
// tlsclient.WithTimeoutSeconds, impersonate.WithTimeout, http.Client.Timeout, - но
// socks-рукопожатие через прокси, который принял соединение и молчит, не слушает ни
// их, ни контекст вызывающего. Сессия с таймаутом 1s против молчащего слушателя на
// 127.0.0.1 держала запрос полные 20s, весь срок контекста, и вернула context
// deadline exceeded только по его истечении.
//
// В живом прогоне это стоило трёх минут: search с VOIDSEARCH_REQUEST_TIMEOUT=3s и
// VOIDSEARCH_PROXIES=socks5://127.0.0.1:18999 длился 3m0s, 3m0s и 3m0s в трёх
// повторениях, упёршись в потолок команды, и вернул отчёт с engines:null - оператор
// не увидел ни одной причины отказа.

func TestSessionContextNarrowsLongCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	got, gotCancel := sessionContext(ctx, time.Second)
	defer gotCancel()

	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("дедлайн пропал")
	}
	if left := time.Until(dl); left > 5*time.Second {
		t.Errorf("таймаут сессии 1s не сузил часовой срок вызывающего: осталось %v", left)
	}
}

func TestSessionContextKeepsShorterCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	got, gotCancel := sessionContext(ctx, 30*time.Second)
	defer gotCancel()

	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("дедлайн пропал")
	}
	if left := time.Until(dl); left > 5*time.Second {
		t.Errorf("срок вызывающего удлинён до %v: сессия не имеет права продлевать чужой дедлайн", left)
	}
	// context.WithTimeout и сам не продлил бы чужой срок, поэтому здесь важна
	// идентичность: когда дедлайн вызывающего ближе таймаута сессии, лишний
	// контекст не создаётся, и отмена вызывающего доходит без прослойки.
	if got != ctx {
		t.Error("срок вызывающего ближе таймаута сессии, а создан лишний производный контекст")
	}
}

func TestSessionContextAppliesTimeoutWithoutDeadline(t *testing.T) {
	got, cancel := sessionContext(context.Background(), 2*time.Second)
	defer cancel()

	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("контекст без срока так и остался без срока")
	}
	if left := time.Until(dl); left > 5*time.Second || left < time.Second {
		t.Errorf("дедлайн выставлен неверно: осталось %v при таймауте 2s", left)
	}
}

// Нулевой таймаут означает «срок задаёт вызывающий»: подменять его дефолтом на
// этом уровне нельзя, иначе отмена перестала бы работать у тех, кто сознательно
// передал context.Background().
func TestSessionContextWithoutTimeoutKeepsCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, gotCancel := sessionContext(ctx, 0)
	defer gotCancel()

	if _, ok := got.Deadline(); ok {
		t.Error("нулевой таймаут выставил дедлайн")
	}
	cancel()
	select {
	case <-got.Done():
	case <-time.After(time.Second):
		t.Error("отмена вызывающего не дошла до производного контекста")
	}
}

func TestNewSessionStoresTimeout(t *testing.T) {
	s, err := NewSession(mustFP(t), "", "direct", 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.timeout != 7*time.Second {
		t.Errorf("таймаут сессии %v, хочу 7s: без него запрос нечем ограничить", s.timeout)
	}

	z, err := NewSession(mustFP(t), "", "direct", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if z.timeout != 30*time.Second {
		t.Errorf("нулевой таймаут не нормализован: %v вместо 30s", z.timeout)
	}
}

// Интеграция: заявленный таймаут ограничивает запрос, даже когда транспорт его
// игнорирует. До правки этот же прогон длился весь срок контекста.
func TestSessionTimeoutStopsRequestAgainstSilentProxy(t *testing.T) {
	s, err := NewSession(mustFP(t), "socks5://"+silentProxy(t), "tor", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err = s.Get(ctx, "https://example.com/")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("молчащий прокси вернул успех")
	}
	if elapsed > 5*time.Second {
		t.Errorf("запрос с таймаутом сессии 1s длился %v: потолок запроса не применён", elapsed.Round(10*time.Millisecond))
	}
}

func TestSessionTimeoutStopsStdRequestAgainstSilentProxy(t *testing.T) {
	s, err := NewSession(mustFP(t), "socks5://"+silentProxy(t), "tor", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err = s.DoStd(ctx, Request{URL: "https://example.com/", Method: http.MethodGet})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("молчащий прокси вернул успех")
	}
	if elapsed > 5*time.Second {
		t.Errorf("std-запрос с таймаутом сессии 1s длился %v", elapsed.Round(10*time.Millisecond))
	}
}

// Сужение срока не должно ломать штатный запрос: ответ, который приходит быстрее
// таймаута, доходит до вызывающего целиком.
func TestSessionNarrowingKeepsSuccessfulRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ответ пришел"))
	}))
	defer srv.Close()

	s, err := NewSession(mustFP(t), "", "direct", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	got, err := s.Get(ctx, srv.URL+"/")
	if err != nil {
		t.Fatalf("штатный запрос упал: %v", err)
	}
	if got.Status != 200 || string(got.Body) != "ответ пришел" {
		t.Errorf("ответ искажён: status=%d body=%q", got.Status, got.Body)
	}
}

// Просроченный контекст вызывающего не получает второго шанса: запрос обязан
// оборваться сразу, а не жить ещё один таймаут сессии.
func TestSessionContextDoesNotResurrectExpiredCallerContext(t *testing.T) {
	s, err := NewSession(mustFP(t), "", "direct", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	_, err = s.Get(ctx, "https://example.com/")
	if err == nil {
		t.Fatal("просроченный контекст вернул успех")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("просроченный контекст прожил %v", elapsed.Round(10*time.Millisecond))
	}
}
