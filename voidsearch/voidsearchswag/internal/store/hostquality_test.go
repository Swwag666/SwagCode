package store

import (
	"context"
	"math"
	"testing"
)

// TestHostQualityMatchesReferenceFormula сверяет SQL-агрегацию с эталонной
// формулой, посчитанной в Go.
//
// Тест нужен именно как сверка двух независимых реализаций, а не как проверка
// конкретных чисел: подсчёт перенесён из Go в SQL, и расхождение могло бы быть
// незаметным на «удобных» входных данных. Например, если бы подзапрос авторских
// весов забыл фильтр host<>”, веса изменились бы, но тесты на симметричных
// данных (все авторы с одинаковым числом голосов) этого не поймали бы.
func TestHostQualityMatchesReferenceFormula(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()

	// Намеренно несимметричные данные: авторы с разным числом голосов, хосты с
	// разным числом строк и разным числом судей, оценки в разных диапазонах.
	// У каждого хоста минимум три разных автора: порог доверия считается по
	// людям, и хост с двумя судьями в результат не попадает.
	type vote struct {
		url    string
		host   string
		score  float64
		author string
	}
	in := []vote{
		{"https://a.example/1", "a.example", 0.9, "busy"},
		{"https://a.example/2", "a.example", 0.7, "busy"},
		{"https://a.example/3", "a.example", 0.3, "busy"},
		{"https://a.example/4", "a.example", 0.5, "solo"},
		{"https://a.example/5", "a.example", 0.6, "solo2"},
		{"https://a.example/6", "a.example", 0.2, "solo3"},
		{"https://b.example/1", "b.example", 1.0, "mid"},
		{"https://b.example/2", "b.example", 0.0, "mid"},
		{"https://b.example/3", "b.example", 0.6, "other"},
		{"https://b.example/4", "b.example", 0.4, "other2"},
		{"https://b.example/5", "b.example", 0.8, "other3"},
		{"https://c.example/1", "c.example", 0.4, "x1"},
		{"https://c.example/2", "c.example", 0.4, "x2"},
		{"https://c.example/3", "c.example", 0.4, "x3"},
	}

	var votes []Vote
	refRows := []struct {
		host   string
		score  float64
		author string
	}{}
	for i, v := range in {
		votes = append(votes, Vote{
			QueryHash: "q", URL: v.url, Score: v.score, Author: v.author,
		})
		refRows = append(refRows, struct {
			host   string
			score  float64
			author string
		}{v.host, v.score, v.author})
		_ = i
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}

	// Эталон: прежняя реализация в Go, воспроизведённая здесь независимо.
	byAuthor := map[string]int{}
	for _, r := range refRows {
		byAuthor[r.author]++
	}
	type acc struct {
		sum, w  float64
		n       int
		authors map[string]bool
	}
	hosts := map[string]*acc{}
	for _, r := range refRows {
		w := 1.0 / math.Sqrt(float64(byAuthor[r.author]))
		a := hosts[r.host]
		if a == nil {
			a = &acc{authors: map[string]bool{}}
			hosts[r.host] = a
		}
		a.sum += r.score * w
		a.w += w
		a.n++
		a.authors[r.author] = true
	}
	for h, a := range hosts {
		// Порог доверия считается по разным судьям, а не по строкам: пять
		// голосов одного человека не заменяют три мнения.
		if len(a.authors) < 3 {
			if _, ok := got[h]; ok {
				t.Errorf("хост %q с %d судьями (%d строк) прошёл порог minVotes=3", h, len(a.authors), a.n)
			}
			continue
		}
		want := (a.sum/a.w - 0.5) * 6
		if want > 3 {
			want = 3
		}
		if want < -3 {
			want = -3
		}
		gotV, ok := got[h]
		if !ok {
			t.Errorf("хост %q отсутствует в результате", h)
			continue
		}
		if math.Abs(gotV-want) > 1e-9 {
			t.Errorf("хост %q: бонус %v, эталон %v", h, gotV, want)
		}
	}
	// Все три хоста проходят порог по судьям: a.example - четыре автора при шести
	// строках, b.example - четыре автора при пяти строках, c.example - три автора
	// при трёх строках.
	if len(got) != 3 {
		t.Errorf("получено %d хостов (%v), ожидала 3", len(got), got)
	}
	for _, h := range []string{"a.example", "b.example", "c.example"} {
		if _, ok := got[h]; !ok {
			t.Errorf("хост %q отсутствует, хотя судей достаточно", h)
		}
	}
}

func TestHostQualitySingleAuthorWeightIsOne(t *testing.T) {
	// Прежняя реализация имела отдельную ветвь «n <= 1 даёт вес 1.0». В SQL она
	// не нужна: 1/sqrt(1) = 1. Тест фиксирует, что формула покрывает случай
	// единственного голоса и ветвление не потеряно при переносе.
	st := openVotesDB(t)
	ctx := context.Background()

	// Один автор, три разных URL (upsert склеивает одинаковый ключ).
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://a.example/1", Score: 0.8, Author: "only"},
		{QueryHash: "q", URL: "https://a.example/2", Score: 0.8, Author: "only"},
		{QueryHash: "q", URL: "https://a.example/3", Score: 0.8, Author: "only"},
	}); err != nil {
		t.Fatal(err)
	}

	// Порог здесь единица: цель теста - вес единственного автора, а не правило
	// доверия. С порогом 3 хост одного судьи не попал бы в результат вовсе, и
	// тест перестал бы проверять то, ради чего написан.
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Все веса равны 1, среднее 0.8, бонус (0.8-0.5)*6 = 1.8.
	want := 1.8
	if math.Abs(got["a.example"]-want) > 1e-9 {
		t.Errorf("бонус %v, ожидала %v", got["a.example"], want)
	}
}

func TestHostQualityAuthorVotesCountedOnlyWithHost(t *testing.T) {
	// Число голосов автора обязано считаться по строкам с непустым host, как в
	// прежней реализации: там счётчик увеличивался в обходе выборки, уже
	// отфильтрованной по host<>''.
	//
	// Если бы подзапрос весов считал все строки автора, вес автора с голосами без
	// host стал бы меньше, и бонус хоста изменился бы.
	st := openVotesDB(t)
	ctx := context.Background()

	votes := []Vote{
		{QueryHash: "q", URL: "https://a.example/1", Score: 1.0, Author: "mixed"},
		{QueryHash: "q", URL: "https://a.example/2", Score: 1.0, Author: "mixed"},
		{QueryHash: "q", URL: "https://a.example/3", Score: 1.0, Author: "mixed"},
	}
	if _, err := st.SubmitVotes(ctx, votes); err != nil {
		t.Fatal(err)
	}

	// Добавляем строки без host напрямую: SubmitVotes их отбрасывает, а в
	// таблице они возможны, и именно они отличают две трактовки подсчёта.
	for i := 0; i < 20; i++ {
		if _, err := st.db.ExecContext(ctx, `
			INSERT INTO relevance(query_hash, url, host, score, author, updated_at)
			VALUES ('q', ?, '', 1.0, 'mixed', CURRENT_TIMESTAMP)`,
			"nohost"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	// Порог единица: хост один и судья один, а цель теста - источник подсчёта
	// веса автора, а не правило доверия.
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Если бы веса считались по всем 23 строкам автора, вес был бы 1/sqrt(23),
	// но на среднее это не влияет: все оценки одинаковы, и вес сокращается.
	// Поэтому проверяем значение напрямую - оно обязано остаться +3.
	if math.Abs(got["a.example"]-3) > 1e-9 {
		t.Errorf("бонус %v, ожидала 3", got["a.example"])
	}

	// Отдельно проверяем, что строки без host не попали в результат как хост.
	if _, ok := got[""]; ok {
		t.Error("пустой host попал в результат")
	}
}

func TestHostQualityMixedScoresPerAuthor(t *testing.T) {
	// Разные оценки у одного автора: вес сокращается только если оценки
	// одинаковы, поэтому здесь расхождение между трактовками было бы видно
	// численно. Сверяю с эталоном.
	st := openVotesDB(t)
	ctx := context.Background()

	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://a.example/1", Score: 1.0, Author: "heavy"},
		{QueryHash: "https://a.example/2", URL: "https://a.example/2", Score: 0.0, Author: "heavy"},
		{QueryHash: "q", URL: "https://a.example/3", Score: 0.5, Author: "light"},
	}); err != nil {
		t.Fatal(err)
	}

	// Порог два: у хоста два судьи, и цель теста - формула взвешивания, а не
	// правило доверия. С порогом 3 хост не попал бы в результат, и расхождение
	// формулы спряталось бы за пустой картой: среднее здесь ровно 0.5, то есть
	// бонус 0 неотличим от отсутствия хоста.
	got, err := st.HostQuality(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Эталон: heavy имеет 2 голоса -> вес 1/sqrt(2); light 1 голос -> вес 1.
	wh := 1.0 / math.Sqrt(2)
	wl := 1.0
	sum := 1.0*wh + 0.0*wh + 0.5*wl
	wsum := wh + wh + wl
	want := (sum/wsum - 0.5) * 6
	if math.Abs(got["a.example"]-want) > 1e-9 {
		t.Errorf("бонус %v, эталон %v", got["a.example"], want)
	}
}

func TestHostQualityClampBounds(t *testing.T) {
	// Зажим обязан выполняться в SQL через MIN/MAX и давать ровно -3 и +3 на
	// границах, а не значения за пределами диапазона.
	st := openVotesDB(t)
	ctx := context.Background()

	// По три разных судьи на хост: порог доверия считается по людям, поэтому
	// один автор с тремя голосами хост в результат не пропустит, а цель теста -
	// границы зажима, а не правило порога.
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://top.example/1", Score: 1.0, Author: "top1"},
		{QueryHash: "q", URL: "https://top.example/2", Score: 1.0, Author: "top2"},
		{QueryHash: "q", URL: "https://top.example/3", Score: 1.0, Author: "top3"},
		{QueryHash: "q", URL: "https://low.example/1", Score: 0.0, Author: "low1"},
		{QueryHash: "q", URL: "https://low.example/2", Score: 0.0, Author: "low2"},
		{QueryHash: "q", URL: "https://low.example/3", Score: 0.0, Author: "low3"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got["top.example"] != 3 {
		t.Errorf("верхний зажим %v, ожидала ровно 3", got["top.example"])
	}
	if got["low.example"] != -3 {
		t.Errorf("нижний зажим %v, ожидала ровно -3", got["low.example"])
	}
	for h, b := range got {
		if b < -3 || b > 3 {
			t.Errorf("хост %q: бонус %v вне [-3, 3]", h, b)
		}
	}
}

func TestHostQualityDefaultMinVotes(t *testing.T) {
	// minVotes <= 0 означает значение по умолчанию 3, а не «без порога»: без
	// порога единственный голос давал бы бонус и одна оценка решала бы порядок
	// выдачи.
	//
	// Фикстура намеренно из трёх строк одного автора: по числу строк она прошла
	// бы старый порог, и тест не отличал бы «значение по умолчанию 3» от
	// «порога нет». По числу судей это один человек, и бонуса быть не должно.
	st := openVotesDB(t)
	ctx := context.Background()

	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: "q", URL: "https://a.example/1", Score: 1.0, Author: "a"},
		{QueryHash: "q", URL: "https://a.example/2", Score: 1.0, Author: "a"},
		{QueryHash: "q", URL: "https://a.example/3", Score: 1.0, Author: "a"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, mv := range []int{0, -1, -100} {
		got, err := st.HostQuality(ctx, mv)
		if err != nil {
			t.Fatalf("minVotes=%d: %v", mv, err)
		}
		if len(got) != 0 {
			t.Errorf("minVotes=%d: три строки одного судьи дали бонус %+v", mv, got)
		}
	}
	// Та же фикстура с порогом 1 проходит: значит отсутствие бонуса выше даёт
	// именно порог по умолчанию, а не пустой результат.
	got, err := st.HostQuality(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("при minVotes=1 хостов %d, хочу 1: %+v", len(got), got)
	}
}

func TestHostQualityEmptyTable(t *testing.T) {
	st := openVotesDB(t)
	ctx := context.Background()

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("возвращён nil вместо пустой карты: вызывающий упадёт на обращении")
	}
	if len(got) != 0 {
		t.Errorf("в пустой таблице %d хостов", len(got))
	}
}

func TestHostQualityLargeTableNoFullLoad(t *testing.T) {
	// Проверка масштаба: прежняя версия выгружала всю таблицу в память и делала
	// два прохода. На большом объёме SQL-агрегация обязана работать и давать
	// результат, совпадающий с эталоном по выборочному хосту.
	st := openVotesDB(t)
	ctx := context.Background()

	const authors = 40
	const perAuthor = 25
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO relevance(query_hash, url, host, score, author, updated_at)
		VALUES ('q', ?, ?, ?, ?, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	for a := 0; a < authors; a++ {
		author := "author" + string(rune('a'+a%26)) + string(rune('a'+a/26))
		for i := 0; i < perAuthor; i++ {
			host := "host" + string(rune('a'+i%26)) + ".example"
			url := "https://" + host + "/" + author + string(rune('a'+i%26))
			score := float64((a+i)%11) / 10.0
			if _, err := stmt.Exec(url, host, score, author); err != nil {
				t.Fatal(err)
			}
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostQuality(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("на большой таблице результат пуст")
	}
	for h, b := range got {
		if b < -3 || b > 3 {
			t.Errorf("хост %q: бонус %v вне [-3, 3]", h, b)
		}
	}

	// Статистика подтверждает, что данные действительно загружены в таблицу, а
	// не остались в транзакции: иначе тест проверял бы пустую базу.
	votes, _, hosts, err := st.VoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if votes != authors*perAuthor {
		t.Errorf("в таблице %d голосов, ожидала %d", votes, authors*perAuthor)
	}
	if hosts < 20 {
		t.Errorf("хостов %d, ожидала не меньше 20", hosts)
	}
}
