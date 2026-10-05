package netx

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestEnsureControlLockedReconnectsAfterDrop - регрессия на постоянную защёлку
// в torRotator.Rotate.
//
// Прежняя Rotate при t.ctrl == nil немедленно возвращала «tor control не
// подключён», а ветка повтора вызывала dropControl(), обнуляя хендл. После
// одного сбоя NEWNYM каждая последующая ротация до конца жизни процесса падала
// мгновенно, даже не пытаясь установить соединение. В живом прогоне это дало
// шесть подряд «ротация не удалась: tor control не подключён» при живом демоне,
// чей control-порт отвечал на GETINFO.
func TestEnsureControlLockedReconnectsAfterDrop(t *testing.T) {
	f := newFakeControl(t, "0.4.9.12", true, true)
	tr := &torRotator{proxyURL: "socks5://" + f.addr, cooldown: time.Second, timeout: 3 * time.Second}
	defer tr.Close()

	// Первое подключение.
	tr.mu.Lock()
	err := tr.ensureControlLocked(f.addr)
	tr.mu.Unlock()
	if err != nil {
		t.Fatalf("первое подключение: %v", err)
	}
	tr.mu.Lock()
	first := tr.ctrl
	tr.mu.Unlock()
	if first == nil {
		t.Fatal("хендл не установлен")
	}

	// Имитация сбоя: хендл сброшен, как это делает ветка повтора в Rotate.
	tr.mu.Lock()
	tr.dropControl()
	tr.mu.Unlock()
	tr.mu.Lock()
	if tr.ctrl != nil {
		tr.mu.Unlock()
		t.Fatal("dropControl не обнулил хендл")
	}
	tr.mu.Unlock()

	// Ключевое утверждение: после сброса переподключение обязано состояться,
	// а не вернуть ошибку навсегда.
	tr.mu.Lock()
	err = tr.ensureControlLocked(f.addr)
	tr.mu.Unlock()
	if err != nil {
		t.Fatalf("переподключение после dropControl: %v", err)
	}
	tr.mu.Lock()
	second := tr.ctrl
	tr.mu.Unlock()
	if second == nil {
		t.Fatal("хендл не восстановлен после сбоя - защёлка осталась")
	}
	if first == second {
		t.Error("вернули тот же хендл: dropControl обязан закрывать старый сокет")
	}
}

func TestEnsureControlLockedKeepsLiveHandle(t *testing.T) {
	// Живой хендл не должен пересоздаваться: лишнее рукопожатие на каждую
	// ротацию замедлило бы смену цепи и плодило бы соединения в tor.
	f := newFakeControl(t, "0.4.9.12", true, true)
	tr := &torRotator{proxyURL: "socks5://" + f.addr, cooldown: time.Second, timeout: 3 * time.Second}
	defer tr.Close()

	tr.mu.Lock()
	if err := tr.ensureControlLocked(f.addr); err != nil {
		tr.mu.Unlock()
		t.Fatal(err)
	}
	first := tr.ctrl
	err := tr.ensureControlLocked(f.addr)
	second := tr.ctrl
	tr.mu.Unlock()

	if err != nil {
		t.Fatalf("второй вызов: %v", err)
	}
	if first != second {
		t.Error("живой хендл пересоздан")
	}
}

func TestEnsureControlLockedWithoutAddr(t *testing.T) {
	// Пустой адрес означает, что процесса нет. Без этой проверки Rotate
	// паниковал: connectControl разыменовывает t.proc, а rotator создаётся до
	// поднятия tor.
	tr := &torRotator{proxyURL: "socks5://127.0.0.1:9050", cooldown: time.Second, timeout: time.Second}
	tr.mu.Lock()
	err := tr.ensureControlLocked("")
	ctrl := tr.ctrl
	tr.mu.Unlock()

	if err == nil {
		t.Fatal("пустой адрес принят")
	}
	if !strings.Contains(err.Error(), "не поднят") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
	if ctrl != nil {
		t.Error("хендл установлен при пустом адресе")
	}
}

func TestEnsureControlLockedUnreachableAddr(t *testing.T) {
	// Закрытый порт: ошибка обязана быть внятной, а не паникой и не молчаливым
	// nil-хендлом, который уронил бы следующий вызов NewIdentity.
	tr := &torRotator{proxyURL: "socks5://127.0.0.1:9050", cooldown: time.Second, timeout: 500 * time.Millisecond}
	tr.mu.Lock()
	err := tr.ensureControlLocked("127.0.0.1:1")
	ctrl := tr.ctrl
	tr.mu.Unlock()

	if err == nil {
		t.Fatal("недоступный control принят")
	}
	if ctrl != nil {
		t.Error("хендл установлен при недоступном порте")
	}
}

// TestExternalRotateRecoversAfterChannelBreak - сквозная проверка отсутствия
// защёлки на externalRotator.
//
// Сквозной вариант для torRotator здесь не пишется сознательно: Rotate берёт
// адрес из t.proc.ControlAddr(), а поля tornago.TorProcess неэкспортированы,
// поэтому сконструировать процесс, указывающий на тестовый control-порт,
// невозможно. Логика переподключения torRotator проверяется напрямую в
// TestEnsureControlLockedReconnectsAfterDrop - это ровно тот вызов, который
// делает Rotate.
//
// У externalRotator адрес - обычное строковое поле, поэтому сквозной сценарий
// проверяется честно: первая ротация проходит, канал портится так же, как это
// делает истёкший абсолютный дедлайн, и следующая ротация обязана
// восстановиться сама.
func TestExternalRotateRecoversAfterChannelBreak(t *testing.T) {
	f := newFakeControl(t, "0.4.9.12", true, true)
	tr := &externalRotator{
		socksAddr: f.addr,
		ctrlAddr:  f.addr,
		cooldown:  time.Millisecond,
		timeout:   3 * time.Second,
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := tr.Rotate(ctx); err != nil {
		t.Fatalf("первая ротация на живом канале: %v", err)
	}
	if !f.seen("SIGNAL NEWNYM") {
		t.Fatal("ротация не отправила NEWNYM, тест не проверяет заявленное")
	}

	// Портим канал так же, как это делает истёкший абсолютный дедлайн: сокет
	// остаётся ненулевым, но команды больше не проходят.
	tr.mu.Lock()
	tr.dropControl()
	tr.mu.Unlock()

	// Следующая ротация обязана переподключиться сама, а не защёлкнуться.
	if err := tr.Rotate(ctx); err != nil {
		t.Errorf("ротация после сбоя не восстановилась: %v", err)
	}
}

// TestExternalRotateRepeatedRecovery - шесть ротаций подряд после сбоя.
//
// Число выбрано по живому наблюдению: пользователь видел ровно шесть подряд
// «ротация не удалась: tor control не подключён». Проверка повторяет тот
// сценарий и утверждает, что защёлки больше нет.
func TestExternalRotateRepeatedRecovery(t *testing.T) {
	f := newFakeControl(t, "0.4.9.12", true, true)
	tr := &externalRotator{
		socksAddr: f.addr,
		ctrlAddr:  f.addr,
		cooldown:  time.Millisecond,
		timeout:   3 * time.Second,
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 1; i <= 6; i++ {
		// Каждый цикл начинаем со сброшенного хендла: это худший случай,
		// при котором прежняя реализация падала навсегда.
		tr.mu.Lock()
		tr.dropControl()
		tr.mu.Unlock()

		if err := tr.Rotate(ctx); err != nil {
			t.Fatalf("ротация %d не восстановилась: %v", i, err)
		}
	}
}

func TestRotateWithoutProcessReturnsErrorNotPanic(t *testing.T) {
	// Регрессия на панику, которую я внесла при починке защёлки: connectControl
	// разыменовывает t.proc, и без проверки на nil Rotate падал на ротаторе,
	// созданном до поднятия tor.
	tr := &torRotator{proxyURL: "socks5://127.0.0.1:9050", cooldown: time.Second, timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := tr.Rotate(ctx)
	if err == nil {
		t.Fatal("Rotate без процесса не вернул ошибку")
	}
	if !strings.Contains(err.Error(), "control") {
		t.Errorf("ошибка не называет причину: %v", err)
	}
}

func TestTorRotatorControlStatusWithoutProcess(t *testing.T) {
	// ControlStatus обязан переживать nil-процесс так же, как остальные методы.
	tr := &torRotator{proxyURL: "socks5://127.0.0.1:9050", cooldown: time.Second, timeout: time.Second}
	got := tr.ControlStatus()
	if got != "не задан" {
		t.Errorf("ControlStatus = %q, ожидала %q", got, "не задан")
	}
}

func TestTorRotatorControlStatusUnreachable(t *testing.T) {
	// Адрес есть, но порт закрыт: статус не должен утверждать подключение.
	tr := &torRotator{
		proxyURL: "socks5://127.0.0.1:9050",
		cooldown: time.Second,
		timeout:  500 * time.Millisecond,
	}
	tr.mu.Lock()
	err := tr.ensureControlLocked("127.0.0.1:1")
	tr.mu.Unlock()
	if err == nil {
		t.Fatal("недоступный control принят, тест не проверяет заявленное")
	}
	got := tr.ControlStatus()
	if strings.HasPrefix(got, "подключён") {
		t.Errorf("недоступный control назван подключённым: %q", got)
	}
}
