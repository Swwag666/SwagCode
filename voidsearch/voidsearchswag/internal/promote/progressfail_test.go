package promote

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProgressCountsFailedChecks(t *testing.T) {
	// Checked растёт до проверки, поэтому без отдельного счётчика отказов
	// «проверено 6» означало и «шесть проверок состоялись», и «шесть попыток
	// упали». Это противоположные выводы о состоянии пула.
	pr := NewProgress(time.Minute)
	pr.Checked++
	pr.Fail(errors.New("tor мёртв"))
	pr.Checked++
	pr.Fail(errors.New("адрес не отвечает"))

	if pr.Failed != 2 {
		t.Errorf("Failed = %d, ожидала 2", pr.Failed)
	}
	if pr.LastError != "tor мёртв" {
		t.Errorf("LastError = %q, ожидала первую причину", pr.LastError)
	}
}

func TestProgressCountsSaveFailuresSeparately(t *testing.T) {
	// Отказ записи не означает, что движок не найден: сид мог быть поднят, но
	// не сохранён. Смешанные в одном счётчике, оба факта потерялись бы.
	pr := NewProgress(time.Minute)
	pr.Checked = 2
	pr.Fail(errors.New("проверка упала"))
	pr.FailSave(errors.New("база заблокирована"))

	if pr.Failed != 1 {
		t.Errorf("Failed = %d, ожидала 1", pr.Failed)
	}
	if pr.SaveFailed != 1 {
		t.Errorf("SaveFailed = %d, ожидала 1", pr.SaveFailed)
	}
	note := pr.Note()
	if !strings.Contains(note, "проверок 1") || !strings.Contains(note, "сохранений 1") {
		t.Errorf("строка об отказах не разделяет источники: %q", note)
	}
	// Причина в строке - первая: здесь это отказ проверки, а не отказ записи.
	if !strings.Contains(note, "проверка упала") {
		t.Errorf("в строке нет первой причины: %q", note)
	}
}

func TestProgressNoteEmptyWhenHealthy(t *testing.T) {
	pr := NewProgress(time.Minute)
	pr.Checked = 5
	if note := pr.Note(); note != "" {
		t.Errorf("строка об отказах при здоровом прогоне: %q", note)
	}
}

func TestProgressNoteNamesCheckedTotal(t *testing.T) {
	// Число проверенных в строке обязательно: «отказов 3» без него не даёт
	// понять, весь прогон упал или его часть.
	pr := NewProgress(time.Minute)
	pr.Checked = 10
	pr.Fail(errors.New("нет сети"))
	pr.Fail(errors.New("нет сети"))
	pr.Fail(errors.New("нет сети"))

	note := pr.Note()
	if !strings.Contains(note, "из 10 проверенных") {
		t.Errorf("в строке нет доли отказов: %q", note)
	}
	if !strings.Contains(note, "отказов 3") {
		t.Errorf("в строке нет числа отказов: %q", note)
	}
}

func TestProgressFailIgnoresNilError(t *testing.T) {
	pr := NewProgress(time.Minute)
	pr.Fail(nil)
	pr.FailSave(nil)
	if pr.Failed != 0 || pr.SaveFailed != 0 || pr.LastError != "" {
		t.Errorf("nil-ошибка учтена как отказ: %+v", pr)
	}
}

func TestProgressNilReceiverSafe(t *testing.T) {
	// Методы Progress уже допускают nil (Exceeded, Remaining), и новые обязаны
	// быть такими же: вызывающий, который не завёл прогресс, не должен падать.
	var pr *Progress
	pr.Fail(errors.New("отказ"))
	pr.FailSave(errors.New("отказ"))
	if note := pr.Note(); note != "" {
		t.Errorf("nil-прогресс вернул строку %q", note)
	}
}
