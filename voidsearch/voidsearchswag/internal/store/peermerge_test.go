package store

import (
	"context"
	"testing"
)

// TestMergePeerOnionPreservesProbeStats закрывает дефект почасового обмена с
// пирами.
//
// Экспорт пира не несёт latency_avg, success_rate, fail_streak и last_probe, а
// SyncPeer вызывал UpsertOnion, который пишет эти колонки безусловно. Каждый
// обмен обнулял статистику до 2000 строк пула и проставлял им last_probe =
// сейчас. ORDER BY success_rate DESC, latency_avg ASC в ListOnions вырождался
// в алфавитный, и discover, collect и promote начинали обходить случайное
// подмножество; NextProbeWave сортирует по last_probe ASC, поэтому обнулённые
// строки выглядели свежепроверенными и пул переставал перепроверяться.
func TestMergePeerOnionPreservesProbeStats(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Собственное измерение: адрес живой, с накопленной статистикой.
	if err := st.UpsertOnion(ctx, Onion{URL: "http://mine.onion", Status: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, "http://mine.onion", true, 120); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetOnion(ctx, "http://mine.onion")
	if err != nil {
		t.Fatal(err)
	}
	if before.SuccessRate == 0 || before.LatencyAvg == 0 {
		t.Fatalf("статистика не записалась до обмена: %+v", before)
	}
	if before.Status != "live" {
		t.Fatalf("статус до обмена %q, ожидала live", before.Status)
	}

	// Пир присылает тот же адрес с устаревшими данными: статус dead, метаданные
	// свои, статистики нет вовсе.
	if err := st.MergePeerOnion(ctx, "http://mine.onion", "dead", "категория пира", "заголовок пира"); err != nil {
		t.Fatal(err)
	}

	after, err := st.GetOnion(ctx, "http://mine.onion")
	if err != nil {
		t.Fatal(err)
	}
	if after.SuccessRate != before.SuccessRate {
		t.Errorf("success_rate сброшен пиром: было %v, стало %v", before.SuccessRate, after.SuccessRate)
	}
	if after.LatencyAvg != before.LatencyAvg {
		t.Errorf("latency_avg сброшен пиром: было %d, стало %d", before.LatencyAvg, after.LatencyAvg)
	}
	if after.FailStreak != before.FailStreak {
		t.Errorf("fail_streak изменён пиром: было %d, стало %d", before.FailStreak, after.FailStreak)
	}
	if after.Status != "live" {
		t.Errorf("статус понижен пиром до %q: собственное измерение важнее чужого мнения", after.Status)
	}
	// last_probe не должен быть перезаписан «сейчас»: иначе NextProbeWave,
	// который сортирует по last_probe ASC, отодвинет адрес в конец очереди, и
	// пул перестанет перепроверяться.
	if !after.LastProbe.Equal(before.LastProbe) {
		t.Errorf("last_probe перезаписан обменом: было %v, стало %v", before.LastProbe, after.LastProbe)
	}
	// Метаданные пира при этом принимаются: в этом и смысл обмена.
	if after.Category != "категория пира" {
		t.Errorf("категория пира не принята: %q", after.Category)
	}
	if after.Title != "заголовок пира" {
		t.Errorf("заголовок пира не принят: %q", after.Title)
	}
}

func TestMergePeerOnionAddsNewAddress(t *testing.T) {
	// Новый адрес от пира обязан появиться в пуле - иначе обмен бесполезен.
	st := newStore(t)
	ctx := context.Background()
	if err := st.MergePeerOnion(ctx, "http://new.onion", "live", "кат", "заголовок"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://new.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != "кат" || got.Title != "заголовок" {
		t.Errorf("метаданные не записаны: %+v", got)
	}
}

func TestMergePeerOnionFillsUnknownStatus(t *testing.T) {
	// Для строки без собственного измерения статус пира принимается: unknown
	// означает «мы ещё не проверяли», и чужие данные лучше ничего.
	st := newStore(t)
	ctx := context.Background()
	if err := st.UpsertOnion(ctx, Onion{URL: "http://unprobed.onion", Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MergePeerOnion(ctx, "http://unprobed.onion", "live", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://unprobed.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "live" {
		t.Errorf("статус не дополнен: %q, ожидала live", got.Status)
	}
}

func TestMergePeerOnionKeepsExistingMetadata(t *testing.T) {
	// Пустые поля пира не должны затирать уже имеющиеся: CASE WHEN
	// excluded.x<>'' защищает и категорию, и заголовок.
	st := newStore(t)
	ctx := context.Background()
	if err := st.MergePeerOnion(ctx, "http://m.onion", "unknown", "моя категория", "мой заголовок"); err != nil {
		t.Fatal(err)
	}
	if err := st.MergePeerOnion(ctx, "http://m.onion", "unknown", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOnion(ctx, "http://m.onion")
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != "моя категория" || got.Title != "мой заголовок" {
		t.Errorf("метаданные затёрты пустыми: %+v", got)
	}
}

func TestMergePeerOnionRejectsEmptyURL(t *testing.T) {
	st := newStore(t)
	if err := st.MergePeerOnion(context.Background(), "   ", "live", "", ""); err == nil {
		t.Error("пустой url принят")
	}
}

func TestMergePeerOnionRankingSurvivesSync(t *testing.T) {
	// Конечная цель: порядок выдачи ListOnions обязан остаться содержательным
	// после обмена. Именно его потеря была практическим последствием дефекта -
	// discover и collect начинали обходить пул по алфавиту.
	st := newStore(t)
	ctx := context.Background()
	for i, u := range []string{"http://aaa.onion", "http://bbb.onion", "http://ccc.onion"} {
		if err := st.UpsertOnion(ctx, Onion{URL: u, Status: "live"}); err != nil {
			t.Fatal(err)
		}
		// Разная статистика: ccc лучший, aaa худший.
		for j := 0; j < i+1; j++ {
			if err := st.RecordProbe(ctx, u, true, int64(300-i*100)); err != nil {
				t.Fatal(err)
			}
		}
	}
	before, err := st.ListOnions(ctx, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("до обмена %d адресов", len(before))
	}

	// Пир присылает все три адреса без статистики.
	for _, u := range []string{"http://aaa.onion", "http://bbb.onion", "http://ccc.onion"} {
		if err := st.MergePeerOnion(ctx, u, "dead", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	after, err := st.ListOnions(ctx, "live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("после обмена %d адресов, было %d: статусы потеряны", len(after), len(before))
	}
	for i := range before {
		if before[i].URL != after[i].URL {
			t.Errorf("порядок изменился на позиции %d: было %s, стало %s",
				i, before[i].URL, after[i].URL)
		}
		if before[i].SuccessRate != after[i].SuccessRate {
			t.Errorf("%s: success_rate изменён обменом (%v -> %v)",
				after[i].URL, before[i].SuccessRate, after[i].SuccessRate)
		}
	}
}
