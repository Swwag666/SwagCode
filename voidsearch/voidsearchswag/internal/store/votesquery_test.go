package store

import (
	"context"
	"testing"
)

// VotesForQuery считает строки и разных авторов по одному запросу: judge_submit
// показывает клиенту фактический объём хранилища, а не только число обработанных
// элементов пачки.
func TestVotesForQueryCountsRowsAndAuthors(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hash := QueryHash("votes probe", "judge")
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: hash, URL: "https://a.example/1", Score: 0.9, Author: "first"},
		{QueryHash: hash, URL: "https://a.example/2", Score: 0.4, Author: "first"},
		{QueryHash: hash, URL: "https://a.example/1", Score: 0.2, Author: "second"},
	}); err != nil {
		t.Fatalf("запись голосов: %v", err)
	}

	rows, authors, err := st.VotesForQuery(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Errorf("строк = %d, хочу 3: голоса разных авторов не схлопываются", rows)
	}
	if authors != 2 {
		t.Errorf("авторов = %d, хочу 2", authors)
	}
}

// Повторная оценка того же url тем же автором обновляет строку: счётчик строк не
// растёт, и именно эту разницу judge_submit показывает как new_rows.
func TestVotesForQueryRepeatDoesNotGrowRows(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hash := QueryHash("repeat probe", "judge")
	batch := []Vote{{QueryHash: hash, URL: "https://a.example/1", Score: 0.9, Author: "judge"}}
	if _, err := st.SubmitVotes(ctx, batch); err != nil {
		t.Fatal(err)
	}
	before, _, err := st.VotesForQuery(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SubmitVotes(ctx, batch); err != nil {
		t.Fatal(err)
	}
	after, authors, err := st.VotesForQuery(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("строк было %d, стало %d: повторная отправка выросла", before, after)
	}
	if authors != 1 {
		t.Errorf("авторов = %d, хочу 1", authors)
	}
}

// Пустой и неизвестный хеш дают нули без ошибки: судейская база по запросу,
// которого никто не оценивал, - нормальное состояние.
func TestVotesForQueryEmptyAndUnknownHash(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SubmitVotes(ctx, []Vote{
		{QueryHash: QueryHash("known", "judge"), URL: "https://a.example/1", Score: 0.5, Author: "judge"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, hash := range []string{"", "   ", QueryHash("unknown", "judge")} {
		rows, authors, err := st.VotesForQuery(ctx, hash)
		if err != nil {
			t.Fatalf("хеш %q: %v", hash, err)
		}
		if rows != 0 || authors != 0 {
			t.Errorf("хеш %q: строк %d, авторов %d, хочу нули", hash, rows, authors)
		}
	}

	// Соседний запрос не попадает в счёт: хеш обязателен как граница.
	rows, authors, err := st.VotesForQuery(ctx, QueryHash("known", "judge"))
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || authors != 1 {
		t.Errorf("известный запрос: строк %d, авторов %d, хочу 1 и 1", rows, authors)
	}
}
