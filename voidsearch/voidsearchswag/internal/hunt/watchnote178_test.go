package hunt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Этап 178, смоук-раунд 4 (K, флаг 2): id-режим WatchDetailed не
// инкрементировал Checked на успешных опросах - поле двигали только отказы и
// режим «все охоты». Смешанный цикл (2 успеха, 1 отказ) давал Checked=0, и
// Note() через ветку «Checked == 0» заявлял «поиск не выполнился ни разу»
// при last_count=19. Здесь фиксируются обе половины правки: инкремент
// успешного опроса id-режима и формулировка Note о смешанном прогоне.

func TestWatchIdModeCountsSuccessfulPolls(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	// Первый опрос - успех (базовая линия), все последующие - отказы:
	// смешанная картина без коротких окон.
	calls := 0
	r := &Runner{Store: st, Search: SearchFunc(func(context.Context, string, string, int) (SearchOutcome, error) {
		calls++
		if calls > 1 {
			return SearchOutcome{}, fmt.Errorf("tor мёртв")
		}
		return SearchOutcome{URLs: []string{"http://a.onion", "http://b.onion"}}, nil
	})}
	id, err := r.Create(ctx, "leak", "deep", 0)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := r.WatchDetailed(ctx, id, 1200*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("WatchDetailed: %v", err)
	}
	if rep.Polls < 2 {
		t.Fatalf("опросов %d, ожидала не меньше 2: тест не успел смешать успех и отказ", rep.Polls)
	}
	if rep.Checked < 1 {
		t.Errorf("Checked = %d: успешный опрос id-режима не посчитан", rep.Checked)
	}
	if rep.Failed < 1 {
		t.Errorf("Failed = %d: отказы поиска потеряны в id-режиме", rep.Failed)
	}
	if rep.LastCount != 2 {
		t.Errorf("LastCount = %d, ожидала 2: размер выдачи не запомнен", rep.LastCount)
	}
}

// Смешанный прогон обязан называться смешанным, а не «поиск не выполнился
// ни разу»: формулировка Note при Checked>0 и Failed>0.
func TestWatchNoteNamesMixedRuns(t *testing.T) {
	rep := WatchReport{Checked: 2, Failed: 1, Polls: 3, LastCount: 19, LastError: "движки лежат"}
	note := rep.Note()
	if strings.Contains(note, "не выполнился ни разу") {
		t.Errorf("Note() объявляет поиск невыполнившимся при 2 успешных прогонах: %q", note)
	}
	if !strings.Contains(note, "успешных прогонов 2") {
		t.Errorf("Note() не называет успешные прогоны: %q", note)
	}
	if !strings.Contains(note, "отказов поиска 1") {
		t.Errorf("Note() не называет число отказов: %q", note)
	}
	// Чистый отказ остаётся честным: Checked=0 - поиска не было вовсе.
	allFail := WatchReport{Checked: 0, Failed: 3, Polls: 3, LastError: "движки лежат"}
	if note := allFail.Note(); !strings.Contains(note, "не выполнился ни разу") {
		t.Errorf("Note() чистого отказа потерял прежний смысл: %q", note)
	}
	// Пустая охота с живым поиском остаётся «пустой» и при положительном
	// Checked (условие Checked==0 из ветки убрано): успешные опросы не
	// превращают пустую выдачу в «стабильную».
	empty := WatchReport{Checked: 3, Failed: 0, Polls: 3, LastCount: 0, Timeout: true}
	if note := empty.Note(); !strings.Contains(note, "пуста") {
		t.Errorf("Note() пустой охоты с успешным поиском: %q", note)
	}
}
