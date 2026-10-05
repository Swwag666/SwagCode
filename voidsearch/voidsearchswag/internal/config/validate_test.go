package config

import (
	"math"
	"testing"
	"time"
)

func TestValidateClampsOverflowingLimits(t *testing.T) {
	// Переполнение было самым опасным случаем. VOIDSEARCH_RESULT_LIMIT на
	// пределе int64 превращал выражение limit*2 в search.Engine в
	// отрицательную границу, из-за чего окно добора открывалось после первого
	// же результата и каждый поиск молча возвращал выдачу одного движка.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.ResultLimit = math.MaxInt64
	c.PromoteLimit = math.MaxInt64
	c.DiscoverBGProbe = math.MaxInt64
	if err := c.Validate(); err != nil {
		t.Fatalf("переполнение должно клампиться, а не отклоняться: %v", err)
	}
	if c.ResultLimit*2 < 0 {
		t.Errorf("ResultLimit=%d всё ещё переполняет limit*2", c.ResultLimit)
	}
	if c.PromoteLimit*3 < 0 {
		t.Errorf("PromoteLimit=%d всё ещё переполняет limit*3", c.PromoteLimit)
	}
	if c.DiscoverBGProbe <= 0 {
		t.Errorf("DiscoverBGProbe=%d", c.DiscoverBGProbe)
	}
}

func TestValidateClampsHugeAllocation(t *testing.T) {
	// Значения доходили до make([]T, 0, limit) в хранилище: PROMOTE_LIMIT в
	// миллиард означал запрос на сотни гигабайт и OOM через час после старта,
	// в фоновой горутине.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.PromoteLimit = 1000000000
	c.DiscoverBGProbe = 9000000000000000000
	c.DiscoverMaxHosts = 1000000000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PromoteLimit > 200 {
		t.Errorf("PromoteLimit=%d, ожидала потолок 200", c.PromoteLimit)
	}
	if c.DiscoverBGProbe > 5000 {
		t.Errorf("DiscoverBGProbe=%d, ожидала потолок 5000", c.DiscoverBGProbe)
	}
	if c.DiscoverMaxHosts > 5000 {
		t.Errorf("DiscoverMaxHosts=%d, ожидала потолок 5000", c.DiscoverMaxHosts)
	}
}

func TestValidateRejectsNegativeDurations(t *testing.T) {
	// Отрицательная длительность почти всегда ошибка в значении, и тихая
	// подстановка дефолта спрятала бы её. Контекст с отрицательным таймаутом
	// истекает немедленно, поэтому каждая операция падала бы сразу.
	for _, env := range []string{
		"VOIDSEARCH_CACHE_TTL", "VOIDSEARCH_REQUEST_TIMEOUT",
		"VOIDSEARCH_PROBE_TIMEOUT", "VOIDSEARCH_HUNT_INTERVAL",
		"VOIDSEARCH_BACKUP_INTERVAL", "VOIDSEARCH_NEWNYM_INTERVAL",
	} {
		t.Run(env, func(t *testing.T) {
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			switch env {
			case "VOIDSEARCH_CACHE_TTL":
				c.CacheTTL = -time.Hour
			case "VOIDSEARCH_REQUEST_TIMEOUT":
				c.RequestTimeout = -time.Second
			case "VOIDSEARCH_PROBE_TIMEOUT":
				c.ProbeTimeout = -time.Second
			case "VOIDSEARCH_HUNT_INTERVAL":
				c.HuntInterval = -time.Minute
			case "VOIDSEARCH_BACKUP_INTERVAL":
				c.BackupInterval = -time.Minute
			case "VOIDSEARCH_NEWNYM_INTERVAL":
				c.MinNewnymInterval = -time.Second
			}
			if err := c.Validate(); err == nil {
				t.Error("отрицательная длительность принята")
			}
		})
	}
}

func TestValidateZeroDurationsGetDefaults(t *testing.T) {
	// Ноль отличается от отрицательного: это «не задано», и дефолт уместен.
	// Отдельно важен CACHE_TTL=0 - он писал в кэш уже истёкшие строки:
	// попаданий не было никогда, а таблица cache росла бесконечно.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.CacheTTL = 0
	c.RequestTimeout = 0
	c.ProbeTimeout = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("нулевые длительности должны получать дефолт: %v", err)
	}
	if c.CacheTTL <= 0 {
		t.Errorf("CacheTTL=%v, ожидала положительный дефолт", c.CacheTTL)
	}
	if c.RequestTimeout <= 0 {
		t.Errorf("RequestTimeout=%v", c.RequestTimeout)
	}
	if c.ProbeTimeout <= 0 {
		t.Errorf("ProbeTimeout=%v", c.ProbeTimeout)
	}
}

func TestValidateRejectsOverflowingCacheTTL(t *testing.T) {
	// Очень большой TTL переполнял time.Now().Add(ttl) в отрицательную метку
	// истечения, и кэш умирал навсегда без единого сообщения.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Литерал 3000000 * time.Hour не компилируется: константа переполняет int64
	// на этапе сборки, поэтому максимальное значение берётся напрямую.
	c.CacheTTL = time.Duration(math.MaxInt64)
	if err := c.Validate(); err == nil {
		t.Error("переполняющий CacheTTL принят")
	}
	// Отдельный случай за пределом 30 суток, но без переполнения: тоже обязан
	// отклоняться, иначе кэш разрастается навсегда.
	c.CacheTTL = 60 * 24 * time.Hour
	if err := c.Validate(); err == nil {
		t.Error("CacheTTL в 60 суток принят при пределе 30")
	}
}

func TestValidateZeroAndNegativeIntsGetDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.ResultLimit = 0
	c.PromoteLimit = -5
	c.BackupKeep = 0
	c.DiscoverConcurrency = -1
	c.ProbeConcurrency = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit <= 0 || c.PromoteLimit <= 0 || c.BackupKeep <= 0 {
		t.Errorf("неположительные значения остались: result=%d promote=%d keep=%d",
			c.ResultLimit, c.PromoteLimit, c.BackupKeep)
	}
	if c.DiscoverConcurrency <= 0 || c.ProbeConcurrency <= 0 {
		t.Errorf("конкурентность не восстановлена: discover=%d probe=%d",
			c.DiscoverConcurrency, c.ProbeConcurrency)
	}
}

func TestValidateProxyMinLiveWithinPool(t *testing.T) {
	// Минимум живых прокси не может превышать размер пула: иначе пул никогда
	// не считался бы пригодным.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.ProxyPoolSize = 2
	c.ProxyMinLive = 100
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ProxyMinLive > c.ProxyPoolSize {
		t.Errorf("ProxyMinLive=%d больше ProxyPoolSize=%d", c.ProxyMinLive, c.ProxyPoolSize)
	}
}

func TestLoadAppliesValidation(t *testing.T) {
	// Validate обязан вызываться из Load, иначе защита существует только в
	// тестах: реальный процесс читает конфиг через Load.
	//
	// Значение берётся большое, но разбираемое. Предельный int64 сюда не
	// подходит: env.Parse отклоняет его сам с ошибкой strconv «value out of
	// range», и Load не доходит до Validate. Это правильное поведение, но оно
	// проверяется отдельно в TestLoadRejectsUnparseableLimit.
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "1000000000")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit > 1000 {
		t.Errorf("ResultLimit=%d не ограничен при загрузке", c.ResultLimit)
	}
	if c.ResultLimit*2 < 0 {
		t.Error("limit*2 всё ещё переполняется после Load")
	}
}

func TestLoadRejectsUnparseableLimit(t *testing.T) {
	// Предел int64 env.Parse не принимает, и Load обязан вернуть ошибку, а не
	// молча подставить дефолт. Тихая подстановка спрятала бы опечатку в
	// настройках.
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "9223372036854775807")
	if _, err := Load(); err == nil {
		t.Error("неразбираемое значение принято без ошибки")
	}
}

func TestLoadRejectsNegativeDuration(t *testing.T) {
	// Отрицательная длительность разбирается env.Parse нормально, поэтому
	// отклонять её обязан именно Validate - и это видно только через Load.
	t.Setenv("VOIDSEARCH_CACHE_TTL", "-1h")
	if _, err := Load(); err == nil {
		t.Error("отрицательный CacheTTL принят")
	}
}

func TestValidateKeepsSaneValuesUntouched(t *testing.T) {
	// Обратная сторона: валидация не должна портить нормальную конфигурацию.
	c := Config{
		ResultLimit:          20,
		PromoteLimit:         10,
		DiscoverBGProbe:      100,
		DiscoverDepth:        2,
		DiscoverMaxHosts:     50,
		DiscoverConcurrency:  4,
		ProbeConcurrency:     16,
		BackupKeep:           7,
		ProxyPoolSize:        40,
		ProxyMinLive:         3,
		ProxyFetchLimit:      400,
		ProxyProbeConc:       48,
		CacheTTL:             time.Hour,
		RequestTimeout:       45 * time.Second,
		ProbeTimeout:         20 * time.Second,
		MinNewnymInterval:    11 * time.Second,
		HuntInterval:         10 * time.Minute,
		DiscoverBGInterval:   24 * time.Hour,
		BackupInterval:       24 * time.Hour,
		DiscoverPerHostDelay: 2 * time.Second,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("нормальная конфигурация отклонена: %v", err)
	}
	if c.ResultLimit != 20 || c.PromoteLimit != 10 || c.ProbeConcurrency != 16 {
		t.Errorf("дефолты изменены: result=%d promote=%d probe=%d",
			c.ResultLimit, c.PromoteLimit, c.ProbeConcurrency)
	}
	if c.CacheTTL != time.Hour || c.ProbeTimeout != 20*time.Second {
		t.Errorf("длительности изменены: ttl=%v probe=%v", c.CacheTTL, c.ProbeTimeout)
	}
}
