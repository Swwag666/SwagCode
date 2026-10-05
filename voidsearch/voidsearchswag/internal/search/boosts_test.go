package search

import (
	"context"
	"fmt"
	"testing"
	"time"

	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/store"
)

// TestBoostsSurviveStoreFailure закрывает дефект, при котором одна переходная
// ошибка базы молча отключала выученное ранжирование на целый час.
//
// Прежняя версия писала `if err == nil { m = q }`, поэтому при ошибке m
// оставалась пустой картой, и она же записывалась в hostBoost вместе с
// boostAt = time.Now(). Следующий час каждый поиск работал без бонусов, а
// RetuneBoosts возвращал ноль, как будто голосов просто нет. Отличить
// «голосов нет» от «база временно недоступна» было невозможно.
func TestBoostsSurviveStoreFailure(t *testing.T) {
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{URL: "https://loved.example/x", Title: "Leak", Source: "a"}}},
		&fixedSearcher{name: "b", res: []searchers.Result{{URL: "https://other.example/y", Title: "Leak files", Source: "b"}}},
	)
	ctx := context.Background()

	// Накапливаем голоса, чтобы бонусы были непустыми.
	for _, author := range []string{"j1", "j2", "j3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("leak database", "judge"),
			URL:       "https://loved.example/x",
			Score:     1.0,
			Author:    author,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Fatal("пересчёт не нашёл хостов")
	}
	hash := store.QueryHash("leak database", "judge")
	before := e.boosts(ctx, hash)
	if len(before) == 0 {
		t.Fatal("бонусы пусты до сбоя")
	}

	// Имитируем переходный сбой базы: закрытое соединение даёт ошибку на
	// каждый запрос.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Сбрасываем срок годности, чтобы boosts действительно сходил в базу и
	// получил ошибку, а не вернул закэшированное значение.
	e.muBoost.Lock()
	e.hostBoost = map[string]boostEntry{hash: {m: before, at: time.Now().Add(-2 * time.Hour)}}
	e.muBoost.Unlock()

	after := e.boosts(ctx, hash)
	if len(after) == 0 {
		t.Error("бонусы обнулены переходной ошибкой базы")
	}
	if len(after) != len(before) {
		t.Errorf("бонусов %d, было %d", len(after), len(before))
	}
	for host, b := range before {
		if a, ok := after[host]; !ok || a != b {
			t.Errorf("хост %s: было %v, стало %v (ok=%v)", host, b, a, ok)
		}
	}
}

func TestRetuneBoostsKeepsPreviousOnFailure(t *testing.T) {
	// Ночной тик на временно недоступной базе не должен обнулять выученные
	// веса до следующего успешного пересчёта.
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{URL: "https://loved.example/x", Title: "Leak", Source: "a"}}},
	)
	ctx := context.Background()
	for _, author := range []string{"j1", "j2", "j3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("leak database", "judge"),
			URL:       "https://loved.example/x",
			Score:     1.0,
			Author:    author,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Fatal("первый пересчёт пуст")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if n := e.RetuneBoosts(ctx); n == 0 {
		t.Error("пересчёт на недоступной базе обнулил веса")
	}
}

func TestBoostsWithoutStoreReturnsEmpty(t *testing.T) {
	// Движок без хранилища - нормальная конфигурация: реранк работает на
	// базовых весах, и это не должно приводить к ошибке или панике.
	e := &Engine{}
	if got := e.boosts(context.Background(), store.QueryHash("q", "judge")); got == nil {
		t.Error("boosts вернул nil вместо пустой карты")
	} else if len(got) != 0 {
		t.Errorf("boosts вернул %v без хранилища", got)
	}
	if n := e.RetuneBoosts(context.Background()); n != 0 {
		t.Errorf("RetuneBoosts вернул %d без хранилища", n)
	}
}

func TestBoostsCachedWithinTTL(t *testing.T) {
	// Внутри часа повторный вызов обязан отдавать кэш, а не ходить в базу
	// каждый раз: чтение relevance на каждый поиск при MaxOpenConns(1) значит
	// регулярно подвешивать выдачу.
	//
	// Проверка построена на изменении голосов, а не на закрытой базе: закрытая
	// база не отличает «сработал кэш» от «база недоступна, поэтому возвращены
	// прежние бонусы», - обе ветки вернули бы одно и то же.
	//
	// Голосов три, от разных авторов: порог доверия равен трём, и с одним
	// голосом карта оказалась бы пуста, а сравнение «пусто равно пусто»
	// проходило бы и без кэша вовсе.
	e, st := newEngine(t,
		&fixedSearcher{name: "a", res: []searchers.Result{{URL: "https://x.example/1", Source: "a"}}},
	)
	ctx := context.Background()
	hash := store.QueryHash("q", "judge")
	vote := func(score float64) {
		t.Helper()
		for _, author := range []string{"a", "b", "c"} {
			if _, err := st.SubmitVotes(ctx, []store.Vote{{
				QueryHash: hash, URL: "https://x.example/1", Score: score, Author: author,
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	vote(1)

	first := e.boosts(ctx, hash)
	if len(first) == 0 {
		t.Fatal("бонусы пусты: кэш сравнивался бы с пустотой")
	}
	if first["x.example"] != 3 {
		t.Fatalf("бонус %v, хочу 3", first["x.example"])
	}

	// Те же три автора перевыставили оценки в ноль: база доступна и ответ уже
	// другой. Кэш обязан удержать прежнее значение до истечения срока.
	vote(0)
	if second := e.boosts(ctx, hash); second["x.example"] != first["x.example"] {
		t.Errorf("кэш не удержал значение внутри TTL: %v против %v", second, first)
	}

	// Срок годности сдвинут в прошлое - теперь чтение из базы обязательно.
	// Без этой половины теста «кэш» мог бы быть вечным, и это тоже сломало бы
	// ночной пересчёт.
	e.muBoost.Lock()
	if ent, ok := e.hostBoost[hash]; ok {
		ent.at = time.Now().Add(-2 * time.Hour)
		e.hostBoost[hash] = ent
	}
	e.muBoost.Unlock()

	if third := e.boosts(ctx, hash); third["x.example"] != -3 {
		t.Errorf("после истечения TTL бонус %v, хочу -3: база не перечитана", third["x.example"])
	}
}

// Пустой query_hash из поиска не приходит: QueryHash всегда возвращает sha256.
// Договор store тем не менее проверяется и здесь, на стороне потребителя, -
// именно его нарушение и переносило чужие голоса в чужую выдачу.
func TestBoostsWithoutQueryHashReturnsEmpty(t *testing.T) {
	e, st := newEngine(t)
	ctx := context.Background()
	for _, author := range []string{"j1", "j2", "j3"} {
		if _, err := st.SubmitVotes(ctx, []store.Vote{{
			QueryHash: store.QueryHash("leak database", "judge"),
			URL:       "https://loved.example/1", Score: 1, Author: author,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	if got := e.boosts(ctx, ""); len(got) != 0 {
		t.Errorf("пустой query_hash вернул бонусы %v: применились чужие голоса", got)
	}
	if got := e.boosts(ctx, "   "); len(got) != 0 {
		t.Errorf("query_hash из пробелов вернул бонусы %v: применились чужие голоса", got)
	}
}

// Кэш бонусов хранится по запросам, а MCP-сервер живёт долго и видит их много.
// Без предела карта росла бы пропорционально истории обращений, поэтому старые
// записи выкидываются.
func TestBoostsCacheIsBounded(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()
	for i := 0; i < boostCacheLimit+50; i++ {
		e.boosts(ctx, store.QueryHash(fmt.Sprintf("запрос %d", i), "judge"))
	}

	// Запрос после переполнения обязан остаться в кэше: выкидывать свежие
	// записи значило бы читать базу на каждый поиск.
	fresh := store.QueryHash("после переполнения", "judge")
	e.boosts(ctx, fresh)

	e.muBoost.Lock()
	n := len(e.hostBoost)
	_, hasFresh := e.hostBoost[fresh]
	e.muBoost.Unlock()

	if n > boostCacheLimit {
		t.Errorf("кэш разросся до %d записей при пределе %d", n, boostCacheLimit)
	}
	if n == 0 {
		t.Error("кэш пуст: выкинуты все записи подряд")
	}
	if !hasFresh {
		t.Error("свежая запись выкинута вместе со старыми")
	}
}
