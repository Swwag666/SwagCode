package promote

import (
	"strings"
	"testing"
	"time"
)

func TestNewProgressDefaults(t *testing.T) {
	pr := NewProgress(0)
	if pr.budget != DefaultBudget {
		t.Errorf("нулевой бюджет не заменён дефолтом: %v", pr.budget)
	}
	pr = NewProgress(-time.Second)
	if pr.budget != DefaultBudget {
		t.Errorf("отрицательный бюджет не заменён дефолтом: %v", pr.budget)
	}
	if pr.Checked != 0 || pr.StoppedReason != "" {
		t.Errorf("новое состояние не нулевое: %+v", pr)
	}
}

func TestProgressNotExceededWithinBudget(t *testing.T) {
	pr := NewProgress(time.Minute)
	if pr.Exceeded() {
		t.Error("бюджет исчерпан сразу после создания")
	}
	if pr.StoppedReason != "" {
		t.Errorf("причина остановки заполнена раньше времени: %q", pr.StoppedReason)
	}
}

func TestProgressExceededAfterBudget(t *testing.T) {
	pr := NewProgress(20 * time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if !pr.Exceeded() {
		t.Fatal("просроченный бюджет не обнаружен")
	}
	if pr.StoppedReason == "" {
		t.Error("причина остановки не зафиксирована")
	}
	// Повторный вызов обязан вернуть тот же результат и не перезаписать
	// причину: иначе в отчёте окажется текст про другое число проверок.
	first := pr.StoppedReason
	pr.Checked += 5
	if !pr.Exceeded() {
		t.Error("второй вызов снял флаг превышения")
	}
	if pr.StoppedReason != first {
		t.Errorf("причина перезаписана: %q -> %q", first, pr.StoppedReason)
	}
}

func TestProgressReasonMentionsCheckedCount(t *testing.T) {
	pr := NewProgress(10 * time.Millisecond)
	pr.Checked = 7
	time.Sleep(25 * time.Millisecond)
	pr.Exceeded()
	if pr.StoppedReason == "" {
		t.Fatal("нет причины")
	}
	if !strings.Contains(pr.StoppedReason, "7") {
		t.Errorf("в причине нет числа проверок: %q", pr.StoppedReason)
	}
}

func TestProgressRemainingShrinks(t *testing.T) {
	pr := NewProgress(200 * time.Millisecond)
	first := pr.Remaining()
	time.Sleep(50 * time.Millisecond)
	second := pr.Remaining()
	if second >= first {
		t.Errorf("остаток не уменьшается: %v -> %v", first, second)
	}
}

func TestProgressRemainingNeverNegative(t *testing.T) {
	pr := NewProgress(10 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if got := pr.Remaining(); got != 0 {
		t.Errorf("остаток %v, ожидала 0", got)
	}
}

func TestProgressElapsedGrows(t *testing.T) {
	pr := NewProgress(time.Minute)
	time.Sleep(20 * time.Millisecond)
	if pr.Elapsed() <= 0 {
		t.Errorf("elapsed %v", pr.Elapsed())
	}
}

func TestProgressNilSafe(t *testing.T) {
	// nil-прогресс встречается, когда бюджет не настроен и вызывающий не стал
	// создавать учёт. Паника здесь уронила бы весь прогон промоута.
	var pr *Progress
	if pr.Exceeded() {
		t.Error("nil-прогресс считает бюджет исчерпанным")
	}
	if pr.Remaining() != 0 || pr.Elapsed() != 0 {
		t.Error("nil-прогресс вернул ненулевые значения")
	}
}

func TestPromoterBudgetDefault(t *testing.T) {
	p := &Promoter{}
	if p.budget() != DefaultBudget {
		t.Errorf("budget()=%v, ожидала %v", p.budget(), DefaultBudget)
	}
	p.Budget = 30 * time.Second
	if p.budget() != 30*time.Second {
		t.Errorf("явный бюджет проигнорирован: %v", p.budget())
	}
}
