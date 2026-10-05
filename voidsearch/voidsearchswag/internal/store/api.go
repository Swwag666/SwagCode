package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"voidsearchswag/internal/filex"
)

var ErrNotFound = errors.New("запись не найдена")

type Onion struct {
	URL         string
	Status      string
	Category    string
	Title       string
	Description string
	LatencyAvg  int64
	SuccessRate float64
	FailStreak  int
	LastProbe   time.Time
	FirstSeen   time.Time
}

type Source struct {
	URL        string
	Type       string
	Quality    float64
	Popularity float64
	FirstSeen  time.Time
	LastOK     time.Time
}

type Task struct {
	ID        string
	Kind      string
	Status    string
	Progress  int
	Message   string
	ResultRef string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Этап 178 (смоук-G/I): до тегов оба типа сериализовались PascalCase
// (ID, ScheduleMin, TimesSeen) - единственные структуры интерфейса, выпадающие
// из snake_case всех 25 остальных инструментов. Теги же обещаны телом
// описаний hunt_list и hunt_hits: «id, query, mode, schedule_min, last_run,
// last_hash, created_at» и «id, hunt_id, query, mode, url, first_found,
// last_found, times_seen». Расхождение текста и факта увидели оба слепых
// прогона. Имена тегов совпадают с колонками таблиц, SQL не затронут.
type Hunt struct {
	ID          int64     `json:"id"`
	Query       string    `json:"query"`
	Mode        string    `json:"mode"`
	ScheduleMin int       `json:"schedule_min"`
	LastRun     time.Time `json:"last_run"`
	LastHash    string    `json:"last_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

// HuntHit - url из находки охоты вместе с запросом и режимом на момент находки.
//
// Запрос и режим хранятся в строке, а не берутся из hunts: история обязана
// оставаться читаемой, даже если охоту потом переименовали или перевели в
// другой режим. Иначе «находка по запросу leak database» со временем начинала
// читаться как находка по совсем другому запросу.
type HuntHit struct {
	ID         int64     `json:"id"`
	HuntID     int64     `json:"hunt_id"`
	Query      string    `json:"query"`
	Mode       string    `json:"mode"`
	URL        string    `json:"url"`
	FirstFound time.Time `json:"first_found"`
	LastFound  time.Time `json:"last_found"`
	TimesSeen  int       `json:"times_seen"`
}

type FileEntry struct {
	ID         int64
	TaskID     string
	URL        string
	Filename   string
	Ext        string
	Size       int64
	MIME       string
	SourcePage string
	Verdict    string
	FoundAt    time.Time
}

type Selector struct {
	ID         int64
	URLPattern string
	Field      string
	Selector   string
	Strategy   string
	Confidence float64
	UpdatedAt  time.Time
}

// QueryHash - устойчивый идентификатор запроса в пределах области. Областью
// может быть режим поиска или, как в случае голосов, произвольная метка
// ("judge").
//
// Формат намеренно сохранён прежним: хеши запросов уже лежат в таблице
// голосов, и любое изменение схемы обнулило бы накопленные оценки.
func QueryHash(query, scope string) string {
	h := sha256.Sum256([]byte(scope + "\x00" + strings.ToLower(strings.TrimSpace(query))))
	return hex.EncodeToString(h[:])
}

// CacheKey собирает ключ записи кэша поисковой выдачи.
//
// limit в ключ намеренно НЕ входит. Один запрос в одном режиме - это одна
// сущность, а сколько из неё показать, решает чтение: запись на 20 результатов
// законно обслуживает запрос на 5 простой обрезкой, и плодить под каждый limit
// отдельную копию выдачи незачем.
//
// Обратная сторона - недостача. Если в записи меньше результатов, чем просят,
// её нельзя отдавать как полный ответ: именно так одна и та же команда через
// пару секунд возвращала то 5, то 10 результатов, и кэшированный ответ был
// хуже свежего. Защиту от этого держит не ключ, а сравнение limit'ов при
// чтении (см. search.Engine.Search и поле Outcome.Limit).
func CacheKey(query, mode string) string {
	h := sha256.Sum256([]byte(mode + "\x00" + strings.ToLower(strings.TrimSpace(query))))
	return hex.EncodeToString(h[:])
}

func (s *Store) UpsertOnion(ctx context.Context, o Onion) error {
	if strings.TrimSpace(o.URL) == "" {
		return errors.New("onion: пустой url")
	}
	status := o.Status
	if status == "" {
		status = "unknown"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO onion_pool(url, status, category, title, description, latency_avg, success_rate, fail_streak, last_probe)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), CURRENT_TIMESTAMP))
		ON CONFLICT(url) DO UPDATE SET
			status=?,
			category=CASE WHEN excluded.category<>'' THEN excluded.category ELSE onion_pool.category END,
			title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE onion_pool.title END,
			description=CASE WHEN excluded.description<>'' THEN excluded.description ELSE onion_pool.description END,
			latency_avg=excluded.latency_avg,
			success_rate=excluded.success_rate,
			fail_streak=excluded.fail_streak,
			last_probe=excluded.last_probe`,
		o.URL, status, o.Category, o.Title, o.Description,
		o.LatencyAvg, o.SuccessRate, o.FailStreak, tsOrEmpty(o.LastProbe),
		status, o.Category, o.Title, o.Description,
		o.LatencyAvg, o.SuccessRate, o.FailStreak, tsOrEmpty(o.LastProbe))
	if err != nil {
		return fmt.Errorf("upsert onion: %w", err)
	}
	return nil
}

// MergePeerOnion вливает адрес, полученный от пира, не трогая собственную
// статистику probes.
//
// Отдельный метод нужен потому, что экспорт пира не передаёт latency_avg,
// success_rate, fail_streak и last_probe - их просто нет в формате. Прежний
// SyncPeer вызывал UpsertOnion, а тот пишет эти колонки безусловно, поэтому
// каждый почасовой обмен обнулял статистику до 2000 строк пула и проставлял им
// last_probe = сейчас.
//
// Последствия были тихие и накопительные. ORDER BY success_rate DESC,
// latency_avg ASC в ListOnions - а комментарий к нему прямо называет порядок
// семантически значимым - вырождался в алфавитный, и discover, collect и
// promote начинали обходить по сути случайное подмножество. NextProbeWave
// сортирует по last_probe ASC, поэтому только что обнулённые строки выглядели
// свежепроверенными и уходили в конец очереди: пул переставал
// перепроверяться. Status тоже перезаписывался безусловно, так что пир с
// устаревшими данными понижал свежие проверенные live-адреса до dead, и
// SearchOnions их отфильтровывал. Живой адрес набирает success_rate всего 0.1
// за первый успех, поэтому на конкурентные 0.9 нужно около десяти probes - и
// обмен сбрасывал этот счётчик каждый час.
//
// Status принимается только для строк, у которых он ещё unknown: чужое мнение
// о живости не должно перебивать собственное измерение.
func (s *Store) MergePeerOnion(ctx context.Context, url, status, category, title string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("onion: пустой url")
	}
	if status == "" {
		status = "unknown"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO onion_pool(url, status, category, title)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			category=CASE WHEN excluded.category<>'' THEN excluded.category ELSE onion_pool.category END,
			title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE onion_pool.title END,
			status=CASE WHEN onion_pool.status='unknown' THEN excluded.status ELSE onion_pool.status END`,
		url, status, category, title)
	if err != nil {
		return fmt.Errorf("merge peer onion: %w", err)
	}
	return nil
}

func (s *Store) GetOnion(ctx context.Context, url string) (Onion, error) {
	var o Onion
	var lastProbe, firstSeen sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT url, status, category, title, description, latency_avg, success_rate, fail_streak, last_probe, first_seen
		FROM onion_pool WHERE url=?`, url).
		Scan(&o.URL, &o.Status, &o.Category, &o.Title, &o.Description,
			&o.LatencyAvg, &o.SuccessRate, &o.FailStreak, &lastProbe, &firstSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Onion{}, ErrNotFound
	}
	if err != nil {
		return Onion{}, fmt.Errorf("get onion: %w", err)
	}
	o.LastProbe = parseTS(lastProbe)
	o.FirstSeen = parseTS(firstSeen)
	return o, nil
}

// MaxStoreLimit - жёсткий потолок на число строк, которое хранилище готово
// вернуть за один вызов.
//
// Он нужен именно здесь, а не только в обработчиках MCP. Значение limit
// заканчивается в `make([]T, 0, limit)`, поэтому неограниченный ввод из
// внешнего запроса превращается в падение процесса: 9000000000000000000 даёт
// "makeslice: cap out of range", а 1000000000 - выделение на сотни гигабайт и
// OOM. Обработчики свои границы ставят, но защита обязана быть и на последнем
// рубеже, иначе любой новый вызывающий вернёт весь класс дефектов сразу.
//
// Ноль и отрицательные значения означают «не задано» и заменяются дефолтом
// вызывающего, поэтому normLimit принимает оба порога.
//
// Потолок открыт вызывающим, а не спрятан: до правки он был приватным, и два
// места снаружи знали его только из комментария - internal/mcpserver/dbstats.go
// и пояснение рядом с ListFiles в cmd/voidsearchswag/main.go описывали
// «maxStoreLimit = 5000» словами, не имея способа прочитать число и проверить
// ввод до запроса. Живой замер на HEAD 330ebde, копия боевой базы, tor
// выключен, probe --limit N --json:
//
//	3 -> total 3, 20 -> total 20, 0 -> total 50, -5 -> total 50,
//	5001 -> total 5000, 20000 -> total 5000
//
// stderr пуст во всех шести прогонах, rc=1 объясняется пропущенными пробами, а
// не значением флага: подмена дефолтом и обрезка остались невидимыми.
const MaxStoreLimit = 5000

// normLimit приводит limit к рабочему диапазону: def при незаданном значении,
// не выше MaxStoreLimit.
func normLimit(limit, def int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > MaxStoreLimit {
		limit = MaxStoreLimit
	}
	if limit <= 0 {
		limit = 100
	}
	return limit
}

// maxOnionSearchLimit - потолок выдачи поиска по пулу. Он заметно ниже
// MaxStoreLimit, потому что запись пула несёт название, описание и статистику,
// а выдача сортируется по выражению релевантности: на десятках тысяч строк
// неограниченный ответ дорого стоит и мало кому нужен.
//
// Потолок обязан быть видимым вызывающему. Сам по себе он не дефект, дефект -
// когда len(выдачи) печатают как число совпадений: при 1200 совпадениях и
// потолке 500 команда рапортовала «найдено 500», и отличить полный ответ от
// усечённого было нельзя. Рядом с поиском живёт CountOnionSearch, который
// возвращает точное число по тому же условию.
const maxOnionSearchLimit = 500

// OnionSearchLimit отдаёт потолок выдачи поиска по пулу. Вызывающий, который
// просит больше, обязан сказать пользователю, что ответ усечён, и для этого ему
// нужно знать порог, а не угадывать его.
func OnionSearchLimit() int {
	return maxOnionSearchLimit
}

func (s *Store) ListOnions(ctx context.Context, status string, limit int) ([]Onion, error) {
	limit = normLimit(limit, 100)
	q := `SELECT url, status, category, title, description, latency_avg, success_rate, fail_streak, last_probe, first_seen
	      FROM onion_pool`
	args := []any{}
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	// Сортировка задаёт и отбор, и порядок: лимит применяется в SQL, а
	// вызывающий обходит адреса до потолка хостов. Поэтому порядок здесь
	// значим - живые и быстрые идут первыми. Хвост по url нужен, чтобы
	// равные по живости адреса выдавались детерминированно.
	q += ` ORDER BY success_rate DESC, latency_avg ASC, url ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list onion: %w", err)
	}
	defer rows.Close()
	out := make([]Onion, 0, limit)
	for rows.Next() {
		var o Onion
		var lastProbe, firstSeen sql.NullString
		if err := rows.Scan(&o.URL, &o.Status, &o.Category, &o.Title, &o.Description,
			&o.LatencyAvg, &o.SuccessRate, &o.FailStreak, &lastProbe, &firstSeen); err != nil {
			return nil, err
		}
		o.LastProbe = parseTS(lastProbe)
		o.FirstSeen = parseTS(firstSeen)
		out = append(out, o)
	}
	return out, rows.Err()
}

// KnownStatuses возвращает статус каждого адреса, который уже есть в пуле.
// Отсутствующие в базе адреса в ответ не попадают. Запрос идёт порциями:
// у SQLite ограничено число параметров в одном запросе, а список адресов
// приходит из источников целиком и может быть в тысячи строк.
//
// Метод нужен, чтобы отличить свежий адрес от уже проверенного: свежий
// стоит попытки, подтверждённо мёртвый - только остатка бюджета.
func (s *Store) KnownStatuses(ctx context.Context, urls []string) (map[string]string, error) {
	out := make(map[string]string, len(urls))
	const chunk = 500

	for start := 0; start < len(urls); start += chunk {
		end := start + chunk
		if end > len(urls) {
			end = len(urls)
		}
		batch := urls[start:end]

		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, u := range batch {
			ph[i] = "?"
			args[i] = u
		}
		q := `SELECT url, status FROM onion_pool WHERE url IN (` + strings.Join(ph, ",") + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return out, fmt.Errorf("known statuses: %w", err)
		}
		for rows.Next() {
			var url, status string
			if err := rows.Scan(&url, &status); err != nil {
				rows.Close()
				return out, err
			}
			out[url] = status
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

// NextProbeWave отбирает адреса для следующей волны проб - очередь охвата,
// а не только непроверенные. Порядок: сначала ни разу не пробованные, затем
// unknown с историей (меньше отказов - раньше), затем самые устаревшие
// записи пула независимо от статуса - live и dead по старшинству last_probe.
//
// Этап 176: прежний отбор держал WHERE status='unknown', а комментарий
// обещал «проверенные раньше всех» - фильтр вырезал проверенных до
// сортировки, и last_probe-порядок работал только внутри unknown. Волна
// не трогала ни одного проверенного адреса, пока в пуле есть непроверенные
// (на живой базе в тысячи unknown - месяцы): мёртвый сервис оставался live
// навечно, потому что порог смерти не набирается без проб, а латентность
// застывала на первой записи. Витрина этапа 175 с легаси-тысячей у
// live-записей волной не перекрашивалась - только повторным
// discover-обходом задетых хостов. Живой BEFORE-факт (витрина: 5 live
// 2024-года, 2 dead, 3 unknown): limit=5 вернул total=3 - семь записей
// пула вне волны, пул заморожен.
//
// Через ListOnions это делать нельзя: у всех непроверенных адресов
// одинаковый нулевой счёт, и сортировка по живости разрешала ничью по
// алфавиту. Пробы каждый раз брали один и тот же начало списка, а тысячи
// адресов в конце не проверялись никогда - при том что среди них живые.
func (s *Store) NextProbeWave(ctx context.Context, limit int) ([]string, error) {
	limit = normLimit(limit, 50)
	rows, err := s.db.QueryContext(ctx, `
		SELECT url FROM onion_pool
		ORDER BY
			CASE WHEN last_probe IS NULL THEN 0
			     WHEN status='unknown' THEN 1
			     ELSE 2 END,
			fail_streak ASC,
			last_probe ASC,
			url ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("next unprobed: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, limit)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) MarkOnionStatus(ctx context.Context, url, status string, failStreak int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE onion_pool SET status=?, fail_streak=?, last_probe=CURRENT_TIMESTAMP WHERE url=?`,
		status, failStreak, url)
	if err != nil {
		return fmt.Errorf("mark onion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// maxLatencyMS ограничивает латентность, записываемую в пул.
//
// Латентность хранится в int64 миллисекунд и сглаживается как
// (старое*3 + новое)/4. Умножение на три переполняет int64 при значении выше
// 3.07e18 мс, и результат заворачивается в отрицательное число, которое затем
// уходит в базу и в сортировку «быстрые адреса первыми».
//
// Практически латентность так велика не бывает, но испорченная строка или сбой
// таймера отравляли пул навсегда: отрицательное значение не проходит условие
// latency_avg > 0, поэтому ветвь сглаживания отключалась и колонка залипала на
// сыром значении.
//
// Порог - сутки в миллисекундах. Проба с таким временем ответа всё равно
// означает «адрес не отвечает в разумные сроки», а сортировке достаточно
// различать живые адреса между собой.
const maxLatencyMS int64 = 24 * 60 * 60 * 1000

// RecordProbe записывает результат одной пробы живости. Латентность
// сглаживается, доля успехов и серия провалов обновляются накопительно:
// одна случайная неудача не должна выкидывать живой сервис из пула, а
// один удачный ответ не должен возвращать мёртвый.
//
// Вся арифметика выполняется в SQL одним upsert-запросом, а не в Go между
// чтением и записью.
//
// Прежняя версия читала строку через GetOnion, считала новые значения в Go и
// писала их обратно. Это неатомарное чтение-изменение-запись: две конкурентные
// пробы одного адреса читали одно и то же старое состояние, и вклад одной из них
// терялся при последней записи. probe работает с ProbeConcurrency=16, то есть
// конкурентные пробы - штатный режим, а не крайний случай. Разные адреса при этом
// не конфликтовали, поэтому дефект проявлялся редко и выглядел как «статистика
// немного неточна», а не как ошибка.
//
// SQLite сериализует запись, поэтому один upsert делает обновление атомарным без
// явной транзакции и без блокировки единственного соединения на время счёта.
//
// Адреса может не быть в пуле: probe --addr принимает любой валидный onion, а не
// только ранее собранный. Терять результат нельзя - иначе адрес так и не
// появится в пуле, и следующий прогон проверит его заново с нуля. Ветвь INSERT
// обрабатывает этот случай тем же запросом, поэтому отдельное чтение больше не
// нужно и RecordProbe не возвращает ErrNotFound.
func (s *Store) RecordProbe(ctx context.Context, url string, ok bool, latencyMS int64) error {
	_, err := s.RecordProbeStatus(ctx, url, ok, latencyMS)
	return err
}

// RecordProbeStatus записывает пробу и возвращает итоговый статус адреса в пуле
// после обновления: «live», «dead» или «unknown».
//
// Отдельный метод, а не изменение сигнатуры RecordProbe: статус нужен только
// выводу probe, а 35 вызывающих довольствуются ошибкой. Менять сигнатуру значило
// бы править все 35 мест ради одного потребителя.
//
// Статус возвращается из того же атомарного upsert через RETURNING, а не
// повторным чтением строки. Чтение после записи при ProbeConcurrency=16 и одном
// соединении к базе (SetMaxOpenConns(1)) дало бы лишнюю очередь на каждое
// обращение, а главное - могло вернуть состояние, изменённое чужой конкурентной
// пробой между записью и чтением. RETURNING отдаёт значение, которое записала
// именно эта операция.
//
// Исход пробы и статус пула - разные вещи, и вывод обязан их различать. Одна
// неудача не делает адрес мёртвым: порог «dead» срабатывает на третьей подряд,
// поэтому первая неудачная проба нового адреса даёт «unknown». Прежний вывод
// probe печатал «мертв» по флагу r.OK, то есть утверждал состояние пула, которое
// эта проба не установила, и противоречил тому, что показывал poolsearch.
func (s *Store) RecordProbeStatus(ctx context.Context, url string, ok bool, latencyMS int64) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", errors.New("record probe: пустой url")
	}
	// Отрицательная латентность возможна при сбое таймера или переводе часов;
	// без зажима она отравила бы сглаживание так же, как переполнение.
	if latencyMS < 0 {
		latencyMS = 0
	}
	if latencyMS > maxLatencyMS {
		latencyMS = maxLatencyMS
	}

	// Один и тот же запрос обслуживает и успех, и провал: ветвление идёт через
	// CASE по флагу ok. Два отдельных запроса означали бы две копии формул
	// сглаживания, которые разошлись бы при правке одной из них.
	//
	// Плейсхолдеры нумерованные (?1...?4), поэтому порядок аргументов не может
	// разъехаться с порядком появления в тексте: рассогласование связало бы
	// латентность с флагом успеха и молча дало бы неверную статистику без ошибки
	// выполнения. Нумерованная форма - следствие этой правки, в первой версии
	// порядок аргументов не совпадал с порядком плейсхолдеров.
	//
	// status при провале: «dead» начиная с третьей неудачи подряд, иначе
	// прежнее значение. Проверка идёт по fail_streak + 1, потому что SQLite
	// вычисляет все присваивания SET против состояния строки до обновления, и
	// новый fail_streak в этом же выражении ещё не виден.
	//
	// MIN(..., maxLatencyMS) в ветви сглаживания страхует от переполнения на
	// испорченной строке: даже если в базе уже лежит огромное значение,
	// результат не уйдёт выше порога.
	//
	// Ветвь latency_avg > 0 - ЭТАЛОН ПЕРВОЙ ТОЧКИ, а не дыра: у записи без
	// измеренной латентности (latency_avg=0: «unknown» никогда не был
	// успешен, либо INSERT ниже только что создал строку) первая успешная
	// проба становится latency_avg ЦЕЛИКОМ. Сглаживать от нуля нельзя -
	// (0*3+6906)/4 занизило бы первую честную меру вчетверо; вес четверти
	// включается со второй точки (TestRecordProbeSmoothingUnchanged).
	// До этапа 177 договор не был записан нигде, кроме теста
	// TestRecordProbeFirstLatencyNotSmoothed: смоук-агенты читали
	// raw-числа при воскрешении как аномалию EMA. Теперь он назван и
	// здесь, и в описаниях probe_pool/pool_status.
	// Неуспешные пробы латентность не пишут вовсе: ELSE сохраняет прежнее
	// значение - таймаут ничего не говорит о скорости живого ответа.
	var status string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO onion_pool(url, status, latency_avg, success_rate, fail_streak, last_probe)
		VALUES (?1,
			CASE WHEN ?2 THEN 'live' ELSE 'unknown' END,
			CASE WHEN ?2 THEN ?3 ELSE 0 END,
			CASE WHEN ?2 THEN 0.1 ELSE 0 END,
			CASE WHEN ?2 THEN 0 ELSE 1 END,
			CURRENT_TIMESTAMP)
		ON CONFLICT(url) DO UPDATE SET
			status = CASE WHEN ?2 THEN 'live'
			              WHEN onion_pool.fail_streak + 1 >= 3 THEN 'dead'
			              ELSE onion_pool.status END,
			latency_avg = CASE WHEN ?2
			              THEN MIN((CASE WHEN onion_pool.latency_avg > 0
			                             THEN (onion_pool.latency_avg * 3 + ?3) / 4
			                             ELSE ?3 END), ?4)
			              ELSE onion_pool.latency_avg END,
			success_rate = CASE WHEN ?2
			              THEN (onion_pool.success_rate * 9 + 1) / 10.0
			              ELSE onion_pool.success_rate * 9 / 10.0 END,
			fail_streak = CASE WHEN ?2 THEN 0 ELSE onion_pool.fail_streak + 1 END,
			last_probe = CURRENT_TIMESTAMP
		RETURNING status`,
		url,          // ?1 адрес
		ok,           // ?2 флаг успеха, используется во всех ветвлениях
		latencyMS,    // ?3 измеренная латентность
		maxLatencyMS, // ?4 порог зажима
	).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("record probe: %w", err)
	}
	return status, nil
}

func (s *Store) UpsertSource(ctx context.Context, src Source) error {
	if strings.TrimSpace(src.URL) == "" {
		return errors.New("source: пустой url")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sources(url, type, quality, popularity, last_ok)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			type=CASE WHEN excluded.type<>'' THEN excluded.type ELSE sources.type END,
			quality=excluded.quality,
			popularity=excluded.popularity,
			last_ok=COALESCE(excluded.last_ok, sources.last_ok)`,
		src.URL, src.Type, src.Quality, src.Popularity, tsOrEmpty(src.LastOK))
	if err != nil {
		return fmt.Errorf("upsert source: %w", err)
	}
	return nil
}

func (s *Store) GetSource(ctx context.Context, url string) (Source, error) {
	var src Source
	var firstSeen, lastOK sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT url, type, quality, popularity, first_seen, last_ok FROM sources WHERE url=?`, url).
		Scan(&src.URL, &src.Type, &src.Quality, &src.Popularity, &firstSeen, &lastOK)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, fmt.Errorf("get source: %w", err)
	}
	src.FirstSeen = parseTS(firstSeen)
	src.LastOK = parseTS(lastOK)
	return src, nil
}

func (s *Store) SelectorCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM selectors`).Scan(&n); err != nil {
		return 0, fmt.Errorf("selector count: %w", err)
	}
	return n, nil
}

// Backup делает снимок базы через VACUUM INTO на ОТДЕЛЬНОМ соединении.
//
// VACUUM INTO читает всю базу и пишет копию, то есть занимает соединение на всё
// это время. Основной пул открыт с SetMaxOpenConns(1), поэтому прежняя версия,
// выполнявшая снимок на s.db, блокировала единственный коннект: поисковый сервер
// не отвечал ни на один запрос, пока копия не завершится. Комментарий при этом
// обещал «без остановки сервера», что было верно для консистентности файла и
// неверно для доступности.
//
// Отдельный коннект безопасен благодаря WAL: читатели не блокируют писателя, а
// busy_timeout(5000) даёт ждать разблокировки вместо немедленного SQLITE_BUSY.
// Снимок получается консистентным - VACUUM INTO работает внутри читающей
// транзакции, - и основной пул всё это время обслуживает запросы.
//
// Каталог создаётся, одинарные кавычки в пути экранируются: путь приходит из
// конфига и из флагов, то есть извне.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("backup: пустой путь")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}

	// Путь к базе берётся из Store: он сохранён при открытии именно для того,
	// чтобы снимок можно было делать на своём соединении. Если Store создан
	// не через Open (тесты с подменой), путь пуст, и тогда остаётся прежний
	// вариант на общем пуле - лучше заблокировать сервер, чем не сделать снимок.
	if s.path == "" {
		return s.vacuumInto(ctx, s.db, dest)
	}

	bk, err := sql.Open("sqlite", sqliteDSN(s.path))
	if err != nil {
		return fmt.Errorf("backup open: %w", err)
	}
	// Одно соединение: снимок - одиночная операция, пул ей не нужен, а лишний
	// коннект к той же базе создавал бы конкуренцию за блокировку.
	bk.SetMaxOpenConns(1)
	defer bk.Close()

	if err := bk.PingContext(ctx); err != nil {
		return fmt.Errorf("backup ping: %w", err)
	}
	return s.vacuumInto(ctx, bk, dest)
}

// vacuumInto выполняет сам снимок на переданном соединении.
//
// Вынесено отдельно, потому что путь два: основное соединение для Store без
// сохранённого пути и выделенное для штатного случая. Копия запроса в двух
// местах разошлась бы при правке экранирования.
func (s *Store) vacuumInto(ctx context.Context, db *sql.DB, dest string) error {
	safe := strings.ReplaceAll(dest, `'`, `''`)
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+safe+`'`); err != nil {
		return fmt.Errorf("backup vacuum: %w", err)
	}
	return nil
}

// EngineHealth - снимок живости поискового движка для персиста между
// рестартами. Без него каждый запуск сервера начинает со счёта 0:0 и
// заново учит, какие движки мертвы, теряя дни статистики.
type EngineHealth struct {
	Name        string
	URL         string
	Live        bool
	LatencyAvg  int64
	SuccessRate float64
	FailStreak  int
	Probes      int
	Successes   int
	LastProbe   time.Time
	Disabled    bool
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) SaveEngineHealth(ctx context.Context, h EngineHealth) error {
	if strings.TrimSpace(h.Name) == "" {
		return errors.New("engine health: пустое имя")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO engine_health(name, url, live, latency_avg, success_rate, fail_streak, probes, successes, last_probe, disabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			url=excluded.url, live=excluded.live, latency_avg=excluded.latency_avg,
			success_rate=excluded.success_rate, fail_streak=excluded.fail_streak,
			probes=excluded.probes, successes=excluded.successes,
			last_probe=excluded.last_probe, disabled=excluded.disabled`,
		h.Name, h.URL, boolInt(h.Live), h.LatencyAvg, h.SuccessRate,
		h.FailStreak, h.Probes, h.Successes, tsOrEmpty(h.LastProbe), boolInt(h.Disabled))
	if err != nil {
		return fmt.Errorf("save engine health: %w", err)
	}
	return nil
}

func (s *Store) LoadEngineHealth(ctx context.Context) ([]EngineHealth, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, url, live, latency_avg, success_rate, fail_streak, probes, successes, last_probe, disabled
		FROM engine_health ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("load engine health: %w", err)
	}
	defer rows.Close()
	var out []EngineHealth
	for rows.Next() {
		var h EngineHealth
		var live, disabled int
		var lastProbe sql.NullString
		if err := rows.Scan(&h.Name, &h.URL, &live, &h.LatencyAvg, &h.SuccessRate,
			&h.FailStreak, &h.Probes, &h.Successes, &lastProbe, &disabled); err != nil {
			return nil, err
		}
		h.Live = live != 0
		h.Disabled = disabled != 0
		h.LastProbe = parseTS(lastProbe)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) UpsertSelector(ctx context.Context, sel Selector) error {
	if sel.Strategy == "" {
		sel.Strategy = "css"
	}
	if sel.Confidence <= 0 {
		sel.Confidence = 1.0
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO selectors(url_pattern, field, selector, strategy, confidence, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(url_pattern, field) DO UPDATE SET
			selector=excluded.selector,
			strategy=excluded.strategy,
			confidence=excluded.confidence,
			updated_at=CURRENT_TIMESTAMP`,
		sel.URLPattern, sel.Field, sel.Selector, sel.Strategy, sel.Confidence)
	if err != nil {
		return fmt.Errorf("upsert selector: %w", err)
	}
	return nil
}

func (s *Store) SelectorsFor(ctx context.Context, urlPattern string) ([]Selector, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, url_pattern, field, selector, strategy, confidence, updated_at
		 FROM selectors WHERE url_pattern=? ORDER BY confidence DESC`, urlPattern)
	if err != nil {
		return nil, fmt.Errorf("selectors: %w", err)
	}
	defer rows.Close()
	var out []Selector
	for rows.Next() {
		var sel Selector
		var updated sql.NullString
		if err := rows.Scan(&sel.ID, &sel.URLPattern, &sel.Field, &sel.Selector,
			&sel.Strategy, &sel.Confidence, &updated); err != nil {
			return nil, err
		}
		sel.UpdatedAt = parseTS(updated)
		out = append(out, sel)
	}
	return out, rows.Err()
}

func (s *Store) CreateTask(ctx context.Context, t Task) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("task: пустой id")
	}
	status := t.Status
	if status == "" {
		status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks(id, kind, status, progress, message, result_ref)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, status=excluded.status, progress=excluded.progress,
			message=excluded.message, result_ref=excluded.result_ref,
			updated_at=CURRENT_TIMESTAMP`,
		t.ID, t.Kind, status, t.Progress, t.Message, t.ResultRef)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func (s *Store) UpdateTask(ctx context.Context, id, status string, progress int, message, resultRef string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status=?, progress=?, message=?,
			result_ref=CASE WHEN ?<>'' THEN ? ELSE result_ref END,
			updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, status, progress, message, resultRef, resultRef, id)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailRunningTasks переводит все running-задачи в failed с пояснением.
//
// Startup-recovery этапа 180: до него рестарт или жёсткий kill сервера
// посреди обхода оставлял задачу running навсегда - новый процесс обязан
// финализировать чужие записи, потому что живой задачи в момент старта
// процесса не существует по определению. Смоук этапа 179 оставил в базе
// стенда 8 вечных running (финиша не было, status обещал tasks_running
// вечно). Возвращает число переведённых записей.
func (s *Store) FailRunningTasks(ctx context.Context, message string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status='failed', message=?, progress=100,
			updated_at=CURRENT_TIMESTAMP
		WHERE status='running'`, message)
	if err != nil {
		return 0, fmt.Errorf("fail running tasks: %w", err)
	}
	return res.RowsAffected()
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var created, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, status, progress, message, result_ref, created_at, updated_at
		FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.Kind, &t.Status, &t.Progress, &t.Message, &t.ResultRef, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	t.CreatedAt = parseTS(created)
	t.UpdatedAt = parseTS(updated)
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, status string, limit int) ([]Task, error) {
	limit = normLimit(limit, 100)
	q := `SELECT id, kind, status, progress, message, result_ref, created_at, updated_at FROM tasks`
	args := []any{}
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var created, updated sql.NullString
		if err := rows.Scan(&t.ID, &t.Kind, &t.Status, &t.Progress, &t.Message,
			&t.ResultRef, &created, &updated); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTS(created)
		t.UpdatedAt = parseTS(updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) CachePut(ctx context.Context, key, mode, payload string, ttl time.Duration) error {
	expires := time.Now().Add(ttl)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cache(query_hash, mode, payload, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(query_hash) DO UPDATE SET
			mode=excluded.mode, payload=excluded.payload, expires_at=excluded.expires_at`,
		key, mode, payload, expires.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return fmt.Errorf("cache put: %w", err)
	}
	return nil
}

func (s *Store) CacheGet(ctx context.Context, key string) (string, bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM cache WHERE query_hash=? AND expires_at > CURRENT_TIMESTAMP`, key).
		Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cache get: %w", err)
	}
	return payload, true, nil
}

func (s *Store) CreateHunt(ctx context.Context, h Hunt) (int64, error) {
	mode := h.Mode
	if mode == "" {
		mode = "auto"
	}
	sched := h.ScheduleMin
	if sched <= 0 {
		sched = 360
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO hunts(query, mode, schedule_min) VALUES (?, ?, ?)`, h.Query, mode, sched)
	if err != nil {
		return 0, fmt.Errorf("create hunt: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) ListHunts(ctx context.Context) ([]Hunt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, query, mode, schedule_min, last_run, last_hash, created_at
		 FROM hunts ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list hunts: %w", err)
	}
	defer rows.Close()
	var out []Hunt
	for rows.Next() {
		var h Hunt
		var lastRun, created sql.NullString
		if err := rows.Scan(&h.ID, &h.Query, &h.Mode, &h.ScheduleMin,
			&lastRun, &h.LastHash, &created); err != nil {
			return nil, err
		}
		h.LastRun = parseTS(lastRun)
		h.CreatedAt = parseTS(created)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) TouchHunt(ctx context.Context, id int64, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE hunts SET last_run=CURRENT_TIMESTAMP, last_hash=? WHERE id=?`, hash, id)
	if err != nil {
		return fmt.Errorf("touch hunt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveHuntHits записывает находку охоты в историю и возвращает число новых url.
//
// Строка на url, а не на находку целиком: UNIQUE(hunt_id, url) плюс upsert даёт
// ровно то разделение, которое нужно оператору - новый url добавляется строкой,
// уже виденный получает свежую дату и счётчик повторов. По times_seen видно,
// кочует ли адрес из выдачи в выдачу или появился один раз.
//
// Повтор url внутри одной выдачи вторым событием не считается: счётчик растёт
// от находки к находке, а не от дублей в одном списке. Пустые url пропускаются,
// потому что UNIQUE превратил бы их в одну строку на все находки сразу.
//
// Всё пишется одной транзакцией: находка из десятка url - одно событие, и
// наполовину записанная история читалась бы как «появилось три адреса», хотя
// поиск отдал десять.
func (s *Store) SaveHuntHits(ctx context.Context, huntID int64, query, mode string, urls []string) (int, error) {
	if huntID <= 0 {
		return 0, fmt.Errorf("save hunt hits: недопустимый id охоты %d", huntID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("save hunt hits: %w", err)
	}
	defer tx.Rollback()

	exists := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT url FROM hunt_hits WHERE hunt_id=?`, huntID)
	if err != nil {
		return 0, fmt.Errorf("save hunt hits: %w", err)
	}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return 0, fmt.Errorf("save hunt hits: %w", err)
		}
		exists[u] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("save hunt hits: %w", err)
	}
	rows.Close()

	added := 0
	done := make(map[string]bool, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" || done[u] {
			continue
		}
		done[u] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO hunt_hits(hunt_id, query, mode, url) VALUES (?, ?, ?, ?)
			 ON CONFLICT(hunt_id, url) DO UPDATE SET
			   last_found=CURRENT_TIMESTAMP, times_seen=times_seen+1`,
			huntID, query, mode, u); err != nil {
			return 0, fmt.Errorf("save hunt hits: %w", err)
		}
		if !exists[u] {
			exists[u] = true
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("save hunt hits: %w", err)
	}
	return added, nil
}

// MaxHuntHitsLimit - потолок числа строк истории находок за один запрос.
//
// Константа открыта вызывающим, потому что CLI обязан отклонять значения сверх
// неё до прогона: иначе оператор просит 5000 строк, хранилище молча отдаёт 1000,
// а отчёт печатает заявленное число. Живой замер на HEAD e054dc4, копия боевой
// базы с историей в 1500 строк: «hunt hits --limit 5000 --json» вернул rc=0,
// "limit": 5000 и "count": 1000.
const MaxHuntHitsLimit = 1000

// ListHuntHits отдаёт историю находок, свежие раньше. huntID <= 0 означает
// «все охоты».
//
// Ограничение приводится к рабочему диапазону здесь, а не в вызывающем коде:
// историю читают и CLI, и MCP, а потолок нужен обоим. Охота, которая месяцами
// собирает находки, без ограничения отдала бы тысячи строк разом и держала бы
// единственный коннект к базе всё время чтения.
func (s *Store) ListHuntHits(ctx context.Context, huntID int64, limit int) ([]HuntHit, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxHuntHitsLimit {
		limit = MaxHuntHitsLimit
	}
	query := `SELECT id, hunt_id, query, mode, url, first_found, last_found, times_seen
	          FROM hunt_hits `
	args := []any{limit}
	if huntID > 0 {
		query += `WHERE hunt_id=? `
		args = []any{huntID, limit}
	}
	query += `ORDER BY last_found DESC, id DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list hunt hits: %w", err)
	}
	defer rows.Close()
	var out []HuntHit
	for rows.Next() {
		var h HuntHit
		var first, last sql.NullString
		if err := rows.Scan(&h.ID, &h.HuntID, &h.Query, &h.Mode, &h.URL,
			&first, &last, &h.TimesSeen); err != nil {
			return nil, err
		}
		h.FirstFound = parseTS(first)
		h.LastFound = parseTS(last)
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteHuntHits стирает историю находок одной охоты и возвращает число
// удалённых строк.
//
// Охота с несуществующим id даёт ErrNotFound, а не ноль: молчаливый ноль не
// отличил бы опечатку в id от честно пустой истории, и оператор решил бы, что
// чистить нечего там, где он просто назвал не ту охоту.
func (s *Store) DeleteHuntHits(ctx context.Context, huntID int64) (int, error) {
	if huntID <= 0 {
		return 0, fmt.Errorf("delete hunt hits: нужен id охоты")
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM hunts WHERE id=?`, huntID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("delete hunt hits: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM hunt_hits WHERE hunt_id=?`, huntID)
	if err != nil {
		return 0, fmt.Errorf("delete hunt hits: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// CountHuntHits возвращает общее число строк hunt_hits по всем охотам.
// Метрики инструмента metrics обещают «находки охот» из базы: счётчик в памяти
// терялся бы при перезапуске сервера, а таблица - источник истины. Отказ базы
// не подменяется нулём: вызывающий отличает пустую историю от сломанной.
func (s *Store) CountHuntHits(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hunt_hits`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count hunt hits: %w", err)
	}
	return n, nil
}

// CleanFileCatalog удаляет из каталога записи, которые файлами содержимого
// не являются. Проверка идёт по filex.IsContent - тому же списку, по которому
// ссылки вообще попадают в каталог, плюс отсечению служебных файлов (подписи,
// контрольные суммы, сертификаты). Отдельный чёрный список здесь держать
// нельзя: он расходится с filex, и тогда мусор, который перестали собирать,
// продолжает лежать в базе навсегда. Так было с расширением «onion»: из
// KnownExts его убрали, а строка в каталоге осталась.
//
// Дополнительно чистятся записи без имени и URL, заканчивающиеся на «/»:
// это страницы, а не файлы.
//
// Третий класс - служебные имена и эндпоинты API (filex.IsSiteMetadata и
// filex.IsAPIPath). Чистка обязана их убирать, а не только перестать собирать:
// иначе строки, попавшие в базу до введения фильтра, остаются там навсегда.
func (s *Store) CleanFileCatalog(ctx context.Context) (int64, error) {
	var removed int64

	// Обход идёт порциями по ключу, а не одной выборкой всей таблицы.
	//
	// Прежняя версия читала `SELECT id, ext, filename, url FROM file_catalog`
	// целиком и накапливала все мусорные id в срезе до начала удаления. При
	// SetMaxOpenConns(1) это значило, что единственный коннект к базе занят
	// полным сканом на всё его время, а поисковый сервер в этот момент не
	// отвечает ни на один запрос. Память при этом росла пропорционально числу
	// мусорных строк, то есть не была ограничена ничем.
	//
	// Keyset-пагинация (WHERE id > ? ORDER BY id LIMIT ?) ограничивает и время
	// одного обращения к базе, и объём памяти: порция прочитана, сразу удалена,
	// коннект освобождён, и между порциями сервер успевает обслужить поиск.
	// OFFSET здесь не используется намеренно - он заставлял бы SQLite каждый раз
	// заново проматывать уже просмотренные строки, и обход стал бы квадратичным.
	//
	// Удаление внутри порции остаётся одним DELETE ... IN: число параметров
	// ограничено размером порции, поэтому в предел SQLite не упирается, а
	// транзакция держится только на время одной порции, а не на всю базу.
	const batch = 400
	lastID := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}

		rows, err := s.db.QueryContext(ctx, `
			SELECT id, ext, filename, url FROM file_catalog
			WHERE id > ? ORDER BY id LIMIT ?`, lastID, batch)
		if err != nil {
			return removed, fmt.Errorf("clean file catalog: выборка: %w", err)
		}

		var junk []int64
		scanned := 0
		for rows.Next() {
			var id int64
			var ext, name, rawURL string
			if err := rows.Scan(&id, &ext, &name, &rawURL); err != nil {
				rows.Close()
				return removed, fmt.Errorf("clean file catalog: чтение строки: %w", err)
			}
			scanned++
			// Курсор двигается по последней просмотренной строке, а не по
			// последней удалённой: иначе при порции без мусора обход встал бы на
			// месте и цикл не завершился.
			lastID = id
			// Все три критерия отбраковки сведены в filex.CatalogJunkReason,
			// чтобы чистка, коллектор и предпросмотр команды clean не могли
			// разойтись. Прежнее расхождение было живым дефектом: эта функция
			// отклоняла служебные расширения, а probeFile коллектора принимал
			// всё из KnownExts, поэтому подписи попадали в каталог и тут же
			// вычищались.
			if filex.IsCatalogJunk(ext, name, rawURL) {
				junk = append(junk, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return removed, fmt.Errorf("clean file catalog: обход строк: %w", err)
		}
		rows.Close()

		if len(junk) > 0 {
			ph := make([]string, len(junk))
			args := make([]any, len(junk))
			for j, id := range junk {
				ph[j] = "?"
				args[j] = id
			}
			res, err := s.db.ExecContext(ctx,
				`DELETE FROM file_catalog WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
			if err != nil {
				return removed, fmt.Errorf("clean file catalog: удаление: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil {
				removed += n
			}
		}

		// Короткая порция означает конец таблицы: продолжать нечего.
		if scanned < batch {
			break
		}
	}

	// Вторая отбраковка - строки без имени и адреса, заканчивающиеся слэшем.
	//
	// Она тоже идёт порциями: прежний безусловный DELETE по всей таблице держал
	// пишущую транзакцию на весь каталог, и при SetMaxOpenConns(1) поисковый
	// сервер вставал на всё это время.
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM file_catalog WHERE id IN (
				SELECT id FROM file_catalog
				WHERE filename = '' OR url LIKE '%/'
				LIMIT ?
			)`, batch)
		if err != nil {
			return removed, fmt.Errorf("clean file catalog: %w", err)
		}
		n, err := res.RowsAffected()
		if err == nil {
			removed += n
		}
		if n == 0 || n < batch {
			break
		}
	}
	return removed, nil
}

// AddFile записывает файл в каталог. Уникальность здесь по url: один и тот
// же файл, найденный в разных прогонах, это одна запись с уточнённой метой,
// а не новая строка. Иначе каталог быстро заполняется копиями одного файла,
// и поиск выдаёт его пачкой. Поля с пустыми значениями не затирают уже
// известные: размер, найденный при одном прогоне, сохраняется и тогда,
// когда следующий прогон его не узнал.
//
// task_id при повторной находке НЕ перезаписывается.
//
// Прежняя версия переносила файл в последнюю задачу, которая его увидела.
// ListFiles(ctx, "A", ...) после этого не возвращал ничего, и files --task A
// показывал пустой каталог, хотя задача A эти файлы действительно нашла.
// Каждый прогон collect_files подъедал происхождение предыдущего, поэтому
// учёт по задачам становился непригодным уже после второго запуска. Записи
// делались ещё и внутренне противоречивыми: task_id свежий, а found_at -
// самый старый, потому что время первой находки не обновлялось никогда.
//
// Теперь task_id и found_at означают «кто и когда увидел файл впервые». Если
// понадобится «кто видел последним», это отдельная колонка в новой миграции, а
// не перегрузка существующей.
func (s *Store) AddFile(ctx context.Context, f FileEntry) error {
	if strings.TrimSpace(f.URL) == "" {
		return errors.New("file: пустой url")
	}
	// Метка приводится к хранящемуся виду до записи. Фильтр SearchFiles сравнивает
	// нормализованный ввод с колонкой точно, чтобы пользоваться индексом
	// idx_file_verdict, поэтому «EXECUTABLE» в базе была бы ненаходима запросом
	// «-verdict executable» и неотличима от «таких файлов нет».
	f.Verdict = filex.NormalizeVerdict(f.Verdict)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO file_catalog(task_id, url, filename, ext, size, mime, source_page, verdict)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			filename    = CASE WHEN excluded.filename    <> '' THEN excluded.filename    ELSE file_catalog.filename    END,
			ext         = CASE WHEN excluded.ext         <> '' THEN excluded.ext         ELSE file_catalog.ext         END,
			size        = CASE WHEN excluded.size         > 0 THEN excluded.size         ELSE file_catalog.size        END,
			mime        = CASE WHEN excluded.mime        <> '' THEN excluded.mime        ELSE file_catalog.mime        END,
			source_page = CASE WHEN excluded.source_page <> '' THEN excluded.source_page ELSE file_catalog.source_page END,
			verdict     = CASE WHEN excluded.verdict     <> '' THEN excluded.verdict     ELSE file_catalog.verdict     END`,
		f.TaskID, f.URL, f.Filename, f.Ext, f.Size, f.MIME, f.SourcePage, f.Verdict)
	if err != nil {
		return fmt.Errorf("add file: %w", err)
	}
	return nil
}

// FileKnown сообщает, что файл с таким адресом уже лежит в каталоге.
//
// Нужен отчёту сбора: task_id первой находки не перезаписывается, и задача,
// повторно увидевшая известный файл, обязана честно посчитать его
// «повторным», а не «сохранённым». До этого collect_files отчитывался
// saved=2, а file_search по task_id задачи возвращал пустоту - файлы
// доехали, но под происхождением первого прогона, и агент не мог понять,
// куда делась его находка (жалоба смоук-агента этапа 165).
func (s *Store) FileKnown(ctx context.Context, url string) (bool, error) {
	if strings.TrimSpace(url) == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM file_catalog WHERE url = ? LIMIT 1`, url).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("file known: %w", err)
	}
	return true, nil
}

// FileOrigin возвращает task_id задачи, первой записавшей файл в каталог.
//
// Нужен честному saved при параллельном сборе одного хоста (смоук этапа
// 179, Д2): два одновременных collect_files проходят FileKnown до того, как
// сосед вставил файл, и AddFile второго срабатывает как ON CONFLICT-обновление
// без вставки. Отчёт при этом считал файл «новым» у обоих: сумма saved
// вызовов (33) превышала фактический прирост каталога (29), и file_search
// по task_id опровергал собственный ответ. Сравнение происхождения после
// записи закрывает окно: вставкой считается только файл, чей origin равен
// task_id текущего прогона - сосед, выигравший гонку, оставил свой.
func (s *Store) FileOrigin(ctx context.Context, url string) (string, bool, error) {
	if strings.TrimSpace(url) == "" {
		return "", false, nil
	}
	var taskID string
	err := s.db.QueryRowContext(ctx, `SELECT task_id FROM file_catalog WHERE url = ? LIMIT 1`, url).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("file origin: %w", err)
	}
	return taskID, true, nil
}

// FileUnknownCount считает файлы, чей размер неизвестен (size=0 или NULL).
//
// Нужен подсказке в ответе file_search: если размерный фильтр вернул пустоту,
// а в каталоге лежат неизвестные, молчание читается как «файлов нет» - хотя
// они есть, просто фильтр не имеет права их показывать. Подсказка называет
// число и путь к ним (unknown_size=true), и агент не остаётся с пустотой
// без объяснения (жалоба смоук-агента этапа 166).
func (s *Store) FileUnknownCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_catalog WHERE size IS NULL OR size = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("file unknown count: %w", err)
	}
	return n, nil
}

// FileSize возвращает известный каталогу размер файла по адресу.
//
// Нужен замеру по свежести (этап 169): повторная находка обязана нести
// размер, который каталог уже знает с первой записи, - иначе file_refs в
// ответе discover на повторном прогоне показывает size=0 у ссылок, чьи
// размеры когда-то замерили, и собранное выглядит хуже, чем оно есть.
// Второй возврат - есть ли запись: NULL и ноль в size означают «не
// замерено», а не «файла нет», и не подтягиваются.
func (s *Store) FileSize(ctx context.Context, url string) (int64, bool, error) {
	if strings.TrimSpace(url) == "" {
		return 0, false, nil
	}
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT size FROM file_catalog WHERE url = ? LIMIT 1`, url).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("file size: %w", err)
	}
	if !size.Valid || size.Int64 <= 0 {
		return 0, true, nil
	}
	return size.Int64, true, nil
}

// FileSnapshot - каталоговая справка об одном адресе: есть ли запись и
// какой размер за ней числится. Нулевой размер при известной записи
// значит «не мерялся» - та же семантика, что у FileSize.
type FileSnapshot struct {
	Known bool
	Size  int64
}

// FileSnapshots читает каталоговую справку по списку адресов одним
// заходом, порциями по 400.
//
// Прежний путь файловой фазы discover - FileKnown и FileSize по одному
// на каждую ссылку, и ещё FileKnown в фазе записи - на живых 709
// file_refs оборачивался ~2100 одиночными обращениями к единственному
// коннекту базы, и пока фаза гнала этот поток, читатель file_search ждал
// своей очереди к коннекту. Смоук этапа 169 зафиксировал у агента
// зависание вызова на 130s; на стенде оно не воспроизводится даже под
// двумя параллельными discover, но длина очереди обязана падать от
// тысяч обращений к двум срезам. Порция 400 - внутри предела
// параметров SQLite и того же размера, что у чистки каталога.
//
// Отсутствие адреса в карте значит «неизвестен»: отсутствие строки и
// есть весь вердикт, отдельного значения не нужно. Пустые адреса
// пропускаются - их в каталоге не бывает. Ошибка возвращается целиком:
// семантика «ошибка поиска = ссылка свежая» остаётся на вызывающей
// стороне, как у этапа 169.
func (s *Store) FileSnapshots(ctx context.Context, urls []string) (map[string]FileSnapshot, error) {
	out := make(map[string]FileSnapshot, len(urls))
	const batch = 400
	for start := 0; start < len(urls); start += batch {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		end := start + batch
		if end > len(urls) {
			end = len(urls)
		}
		ph := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, u := range urls[start:end] {
			if u == "" {
				continue
			}
			ph = append(ph, "?")
			args = append(args, u)
		}
		if len(ph) == 0 {
			continue
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT url, size FROM file_catalog WHERE url IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return out, fmt.Errorf("file snapshots: выборка: %w", err)
		}
		for rows.Next() {
			var url string
			var size sql.NullInt64
			if err := rows.Scan(&url, &size); err != nil {
				rows.Close()
				return out, fmt.Errorf("file snapshots: чтение строки: %w", err)
			}
			snap := FileSnapshot{Known: true}
			if size.Valid && size.Int64 > 0 {
				snap.Size = size.Int64
			}
			out[url] = snap
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, fmt.Errorf("file snapshots: обход строк: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// SearchOnions ищет по пулу сервисов: подстрока в заголовке, описании или
// адресе. Это поиск по собранной базе onion-сервисов, а не по их
// содержимому. По умолчанию берутся только живые записи: мёртвый сервис
// в выдаче - мусор.
//
// Совпадения ранжируются: заголовок важнее описания, описание важнее адреса.
// Без ранга выдача сортировалась только по success_rate, и совпадение внутри
// onion-адреса оказывалось наравне с настоящим совпадением в названии.
// Адрес onion-сервиса - 56 случайных символов base32, поэтому любая короткая
// подстрока регулярно встречается внутри него просто по случайности: запрос
// «book» находил сервисы, в названии и описании которых книг не было, а буквы
// b-o-o-k выпали в адресе. Пользователь получал выдачу, которая выглядела как
// поломанный поиск.
//
// Совпадение по адресу при этом не убрано: искать сервис по фрагменту адреса -
// законный сценарий, когда название неизвестно. Оно просто опустилось в конец,
// где и должно быть.
func (s *Store) SearchOnions(ctx context.Context, text, status string, limit int, includeDead bool) ([]Onion, error) {
	limit = normLimit(limit, 50)
	if limit > maxOnionSearchLimit {
		limit = maxOnionSearchLimit
	}

	where, args := onionSearchCond(text, status, includeDead)
	// rankExpr - выражение релевантности для ORDER BY. Вне текстового запроса
	// оно постоянно и на порядок не влияет.
	//
	// Его аргументы добавляются отдельным списком и приклеиваются к args уже
	// после всех аргументов WHERE. Порядок связывания в SQL определяется
	// порядком плейсхолдеров в тексте запроса, а ORDER BY стоит после WHERE,
	// поэтому аргумент ранга, добавленный раньше аргумента статуса, съезжал на
	// чужое место: фильтр по статусу получал строку шаблона и не отбирал ничего.
	var rankArgs []any
	rankExpr := "0"
	if t := strings.TrimSpace(text); t != "" {
		pat := "%" + escapeLike(t) + "%"
		rankExpr = `CASE
			WHEN title LIKE ? ESCAPE '\' THEN 0
			WHEN description LIKE ? ESCAPE '\' THEN 1
			ELSE 2
		END`
		rankArgs = append(rankArgs, pat, pat)
	}

	query := `SELECT url, status, category, title, description, latency_avg, success_rate, fail_streak, last_probe, first_seen
	          FROM onion_pool`
	if where != "" {
		query += ` WHERE ` + where
	}
	// Ранг идёт первым ключом сортировки, статистика - вторым: внутри группы
	// одинаковой релевантности порядок прежний, поэтому существующее поведение
	// для запросов без текста не меняется.
	//
	// Без текстового запроса член ранга не добавляется вовсе. Подставлять
	// вместо него константу 0 нельзя: SQLite трактует целочисленный литерал в
	// ORDER BY как номер колонки из списка SELECT, поэтому `ORDER BY 0, ...`
	// падал с «1st ORDER BY term out of range». Обёртка в `(SELECT 0)` тоже
	// работала, но лишний член сортировки без нужды усложнял план запроса.
	// Этап 178: хвост url ASC в обеих ветках - детерминизм выборки. Без него
	// равные по релевантности и статистике записи возвращались в порядке,
	// который выбирает SQLite (обычно rowid, но это не договор), и два
	// одинаковых вызова могли отдавать один и тот же срез в разном порядке.
	if len(rankArgs) > 0 {
		query += ` ORDER BY ` + rankExpr + `, success_rate DESC, latency_avg ASC, url ASC LIMIT ?`
	} else {
		query += ` ORDER BY success_rate DESC, latency_avg ASC, url ASC LIMIT ?`
	}
	args = append(args, rankArgs...)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search onions: %w", err)
	}
	defer rows.Close()

	out := make([]Onion, 0, limit)
	for rows.Next() {
		var o Onion
		var lastProbe, firstSeen sql.NullString
		if err := rows.Scan(&o.URL, &o.Status, &o.Category, &o.Title, &o.Description,
			&o.LatencyAvg, &o.SuccessRate, &o.FailStreak, &lastProbe, &firstSeen); err != nil {
			return nil, err
		}
		o.LastProbe = parseTS(lastProbe)
		o.FirstSeen = parseTS(firstSeen)
		out = append(out, o)
	}
	return out, rows.Err()
}

// onionSearchCond собирает условие WHERE для поиска по пулу и аргументы к нему.
//
// Условие вынесено из SearchOnions, потому что оно обязано совпадать дословно с
// тем, что использует CountOnionSearch: два запроса, построенных независимо,
// разъехались бы на первом же уточнении фильтра, и «найдено N, показано M»
// начало бы врать.
//
// Аргументы ранга сюда не входят - они относятся к ORDER BY и нужны только
// выдаче. Пустая строка условия означает, что фильтров нет вовсе.
func onionSearchCond(text, status string, includeDead bool) (string, []any) {
	cond := []string{}
	args := []any{}
	if t := strings.TrimSpace(text); t != "" {
		pat := "%" + escapeLike(t) + "%"
		cond = append(cond, `(title LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\' OR url LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	if status != "" {
		cond = append(cond, `status = ?`)
		args = append(args, status)
	} else if !includeDead {
		cond = append(cond, `status <> 'dead'`)
	}
	if len(cond) == 0 {
		return "", nil
	}
	return strings.Join(cond, ` AND `), args
}

// CountOnionSearch возвращает точное число совпадений для тех же условий, что и
// SearchOnions, без сортировки и без потолка выдачи.
//
// Метод нужен потому, что выдача поиска ограничена maxOnionSearchLimit, и по
// одному её размеру вызывающий не может отличить «в пуле ровно столько
// совпадений» от «совпадений больше, показана первая часть». Без точного числа
// команда печатала len(выдачи) под словом «найдено».
func (s *Store) CountOnionSearch(ctx context.Context, text, status string, includeDead bool) (int, error) {
	query := `SELECT COUNT(*) FROM onion_pool`
	if where, args := onionSearchCond(text, status, includeDead); where != "" {
		query += ` WHERE ` + where
		var n int
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count onion search: %w", err)
		}
		return n, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("count onion search: %w", err)
	}
	return n, nil
}

// CleanOnionTitles вычищает мусорные заголовки: те, что равны адресу,
// содержат .onion или начинаются со схемы. Такие значения появляются от
// старых версий сбора и от каталогов, отдающих подпись вида «Имя
// http://адрес». Заголовок-адрес хуже отсутствующего: по нему нельзя
// искать, а вид он делает.
func (s *Store) CleanOnionTitles(ctx context.Context) (int64, error) {
	var total int64

	// Оба обновления идут порциями по первичному ключу, а не одним UPDATE по
	// всей таблице.
	//
	// Прежняя версия выполняла два безусловных UPDATE по всему onion_pool.
	// Предикаты в них неиндексируемые в принципе - INSTR, GLOB, LIKE и
	// вложенные REPLACE не могут использовать idx_onion_title, - поэтому каждый
	// UPDATE сканировал весь пул и держал пишущую транзакцию на всё это время.
	// При SetMaxOpenConns(1) поисковый сервер не отвечал ни на один запрос, пока
	// чистка не завершится, а на пуле в одиннадцать тысяч адресов это секунды.
	//
	// Порция выбирается по url (первичный ключ), поэтому подзапрос LIMIT
	// ограничен и не требует сортировки; обновление идёт по явному списку
	// ключей. Между порциями коннект освобождён, и сервер успевает обслужить
	// поиск. Контекст проверяется на каждой итерации, чтобы отмена прерывала
	// чистку, а не дожидалась её конца.
	const batch = 400
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE onion_pool SET title = TRIM(SUBSTR(title, INSTR(title, ' ')))
			WHERE url IN (
				SELECT url FROM onion_pool
				WHERE INSTR(title, ' ') > 2
				  AND SUBSTR(TRIM(title), 1, 1) GLOB '[0-9]'
				  AND SUBSTR(title, 1, INSTR(title, ' ') - 1) GLOB '[0-9]*[smh]'
				LIMIT ?
			)`, batch)
		if err != nil {
			return total, fmt.Errorf("clean latency prefix: %w", err)
		}
		n, err := res.RowsAffected()
		if err == nil {
			total += n
		}
		if n == 0 || n < batch {
			break
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE onion_pool SET title=''
			WHERE url IN (
				SELECT url FROM onion_pool
				WHERE title<>'' AND (
					title = url
					OR title = REPLACE(REPLACE(REPLACE(REPLACE(url, '.onion', ''), 'http://', ''), 'https://', ''), '/', '')
					OR title LIKE '%onion%'
					OR title LIKE 'http%'
					OR LENGTH(TRIM(title)) < 4
					OR SUBSTR(TRIM(title), 1, 1) NOT GLOB '[A-Za-z0-9А-Яа-я]'
				)
				LIMIT ?
			)`, batch)
		if err != nil {
			return total, fmt.Errorf("clean onion titles: %w", err)
		}
		n, err := res.RowsAffected()
		if err == nil {
			total += n
		}
		if n == 0 || n < batch {
			break
		}
	}
	return total, nil
}

// SetOnionMeta записывает заголовок, описание и категорию сервиса. Мета
// приходит из обхода и из источников; пустые поля не затирают известное.
func (s *Store) SetOnionMeta(ctx context.Context, url, title, description, category string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE onion_pool SET
			title=CASE WHEN ?<>'' THEN ? ELSE title END,
			description=CASE WHEN ?<>'' THEN ? ELSE description END,
			category=CASE WHEN ?<>'' THEN ? ELSE category END
		WHERE url=?`,
		title, title, description, description, category, category, url)
	if err != nil {
		return fmt.Errorf("set onion meta: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// splitExtList разбирает список расширений, разделённых запятой, пробелом или
// точкой с запятой. Каждая часть приводится к нижнему регистру и теряет
// ведущую точку, поэтому «.EPUB, pdf» и «epub PDF» дают один результат. Пустые
// части отбрасываются: список «epub,,pdf» не должен порождать условие ext = ”.
//
// Возвращает nil для пустой строки, что означает «не ограничивать выборку».
func splitExtList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// TrimLeft, а не TrimPrefix: ведущие точки снимаются целиком, поэтому
		// «.epub» даёт «epub», а «...» даёт пустую строку и отбрасывается. С
		// TrimPrefix ввод из одних точек превращался в «..» и попадал в условие
		// ext = '..', то есть в запрос, который никогда ничего не находит.
		e := strings.ToLower(strings.TrimLeft(strings.TrimSpace(f), "."))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitVerdictList разбирает список меток вердикта так же, как splitExtList
// разбирает расширения: разделители запятая, точка с запятой и пробельные
// символы, приведение к нижнему регистру, отсев пустых частей и дублей.
//
// Отдельная функция, а не переиспользование splitExtList, потому что у меток
// нет ведущих точек: снимать их означало бы молча принимать ввод, которого не
// существует, и расхождение с splitExtList стало бы невидимым.
//
// Регистр приводится к нижнему, потому что метку набирает пользователь в CLI, а
// filex.ClassifyFile всегда пишет нижний регистр. Без приведения фильтр
// «-verdict Ebook» возвращал бы пустой результат, неотличимый от «таких файлов
// нет».
//
// Возвращает nil для пустой строки, что означает «не ограничивать выборку».
func splitVerdictList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		v := strings.ToLower(strings.TrimSpace(f))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FileQuery описывает отбор по каталогу файлов. Пустые поля не
// ограничивают выборку: запрос без условий вернёт всё.
type FileQuery struct {
	// Text ищет подстроку в имени файла, в его URL и в адресе страницы, на
	// которой он был найден (source_page).
	Text string
	// Ext принимает одно расширение или список через запятую, пробел или
	// точку с запятой: «epub», «epub,pdf», «.EPUB, pdf».
	Ext    string
	TaskID string
	// Verdict принимает одну метку или список через запятую, пробел или точку с
	// запятой: «ebook», «ebook,document». Метки задаёт filex.ClassifyFile.
	//
	// Фильтр добавлен вместе с заполнением колонки: прежде verdict не писался
	// никогда, поэтому фильтровать по нему было нечего. Значения сравниваются
	// без учёта регистра, потому что метку может набрать пользователь в CLI, а
	// ClassifyFile всегда выдаёт нижний регистр.
	Verdict  string
	RiskOnly bool
	MinSize  int64
	MaxSize  int64
	// UnknownSize отбирает файлы, чей размер не удалось снять при сборе:
	// колонка size у них 0. Оба размерных фильтра такие записи исключают
	// («до 5 КБ» не должен возвращать видео неизвестного размера), и без
	// этого поля они были недостижимы вовсе: жалоба смоук-агента этапа 166 -
	// семь файлов с size=0 в каталоге, но max_size=100 возвращал пустоту, и
	// достать их не было никаким фильтром.
	//
	// Min и Max при UnknownSize не применяются: сравнивать с диапазоном
	// нечего, а тихо проигнорированный фильтр хуже явной семантики
	// «показать неизвестные».
	UnknownSize bool
	Limit       int
}

// SearchFiles ищет по каталогу файлов: подстрока в имени, URL или странице-
// источнике, расширение, диапазон размера. LIKE с ESCAPE, чтобы символы % и _ в
// запросе не превращались в шаблон и не находили лишнего.
// MaxFileSearchLimit - потолок выдачи поиска по каталогу файлов. Ровно столько
// строк готово вернуть хранилище, какой бы limit ни попросил вызывающий.
//
// Замер до правки на HEAD 36e4cae, проба на временной базе из 1200 файлов:
// SearchFiles(Limit=5000) вернул 1000 строк, Limit=1500 - 1000, Limit=1001 -
// 1000, Limit=1000 - 1000, Limit=999 - 999, Limit=0 - 100, Limit=-5 - 100.
// Число 1000 жило в теле функции, поэтому CLI не мог ни проверить --limit до
// запроса, ни назвать предел в отчёте.
const MaxFileSearchLimit = 1000

func (s *Store) SearchFiles(ctx context.Context, q FileQuery) ([]FileEntry, error) {
	limit := normLimit(q.Limit, 100)
	if limit > MaxFileSearchLimit {
		limit = MaxFileSearchLimit
	}

	cond := []string{}
	args := []any{}
	if t := strings.TrimSpace(q.Text); t != "" {
		pat := "%" + escapeLike(t) + "%"
		// Страница-источник участвует в поиске наравне с именем и адресом
		// файла. Имена в собранных каталогах машинные (0a1b.bin, f001.dat),
		// поэтому адрес страницы, на которой файл висел, нередко единственный
		// осмысленный признак группы; выдача CLI печатает его строкой
		// «страница: ...», и поле, которое показано пользователю, обязано
		// быть фильтром. Колонка объявлена NOT NULL DEFAULT '', так что NULL
		// здесь не появится и не превратит сравнение в неопределённость.
		cond = append(cond, `(filename LIKE ? ESCAPE '\' OR url LIKE ? ESCAPE '\' OR source_page LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	// Расширений может быть несколько: список через запятую, пробел или
	// точку с запятой.
	//
	// Прежняя версия сравнивала ext = ? дословно, поэтому -ext "epub,pdf"
	// молча возвращал пустой результат: в каталоге нет строки с расширением,
	// в точности равным «epub,pdf». Отсутствующий результат при этом
	// неотличим от «таких файлов нет», и пользователь не получал ни ошибки, ни
	// намёка, что синтаксис неверный.
	//
	// Разбор списка живёт в хранилище, а не в CLI, потому что тот же параметр
	// приходит и из MCP-инструмента search_files: иначе один вызывающий
	// принимал бы список, а другой нет.
	if exts := splitExtList(q.Ext); len(exts) > 0 {
		if len(exts) == 1 {
			cond = append(cond, `ext = ?`)
			args = append(args, exts[0])
		} else {
			cond = append(cond, `ext IN (`+strings.TrimSuffix(strings.Repeat("?,", len(exts)), ",")+`)`)
			for _, e := range exts {
				args = append(args, e)
			}
		}
	}
	if q.TaskID != "" {
		cond = append(cond, `task_id = ?`)
		args = append(args, q.TaskID)
	}
	// Меток вердикта может быть несколько, синтаксис тот же, что у расширений:
	// разбор списка живёт в хранилище, чтобы CLI и MCP-инструмент принимали
	// одинаковый ввод.
	if verdicts := splitVerdictList(q.Verdict); len(verdicts) > 0 {
		if len(verdicts) == 1 {
			cond = append(cond, `verdict = ?`)
			args = append(args, verdicts[0])
		} else {
			cond = append(cond, `verdict IN (`+strings.TrimSuffix(strings.Repeat("?,", len(verdicts)), ",")+`)`)
			for _, v := range verdicts {
				args = append(args, v)
			}
		}
	}
	if q.RiskOnly {
		// Список опасных меток задан в filex, а не продублирован здесь: если он
		// расширится, правка в одном месте обновит и предупреждение в CLI, и
		// этот фильтр.
		cond = append(cond, `verdict IN (`+strings.TrimSuffix(strings.Repeat("?,", len(filex.RiskVerdicts)), ",")+`)`)
		for _, v := range filex.RiskVerdicts {
			args = append(args, v)
		}
	}
	if q.MinSize > 0 && !q.UnknownSize {
		cond = append(cond, `size >= ?`)
		args = append(args, q.MinSize)
	}
	if q.MaxSize > 0 && !q.UnknownSize {
		// Ноль в колонке size означает «размер неизвестен», а не «нулевой
		// байт». Без явного исключения он проходил верхнюю границу как очень
		// маленький файл: запрос «покажи файлы до 5 КБ» возвращал втрое больше
		// записей, чем в каталоге вообще есть мелких, и среди них видео
		// неизвестного размера, которое могло весить сотни мегабайт.
		//
		// С min-size такой асимметрии не было - `size >= 1` нули отсекал сам.
		// Теперь оба фильтра трактуют неизвестный размер одинаково: не
		// подходят. Достать их - работа UnknownSize ниже.
		cond = append(cond, `size > 0 AND size <= ?`)
		args = append(args, q.MaxSize)
	}
	if q.UnknownSize {
		cond = append(cond, `(size IS NULL OR size = 0)`)
	}

	query := `SELECT id, task_id, url, filename, ext, size, mime, source_page, verdict, found_at
	          FROM file_catalog`
	if len(cond) > 0 {
		query += ` WHERE ` + strings.Join(cond, ` AND `)
	}
	query += ` ORDER BY size DESC, found_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search files: %w", err)
	}
	defer rows.Close()

	out := make([]FileEntry, 0, limit)
	for rows.Next() {
		var f FileEntry
		var found sql.NullString
		if err := rows.Scan(&f.ID, &f.TaskID, &f.URL, &f.Filename, &f.Ext,
			&f.Size, &f.MIME, &f.SourcePage, &f.Verdict, &found); err != nil {
			return nil, err
		}
		f.FoundAt = parseTS(found)
		out = append(out, f)
	}
	return out, rows.Err()
}

// FileStats отдаёт разбивку каталога: сколько всего файлов, суммарный
// объём и распределение по расширениям.
func (s *Store) FileStats(ctx context.Context) (total int, bytes int64, byExt map[string]int, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM file_catalog`).Scan(&total, &bytes); err != nil {
		return 0, 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ext, COUNT(*) FROM file_catalog GROUP BY ext ORDER BY COUNT(*) DESC, ext`)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	byExt = map[string]int{}
	for rows.Next() {
		var ext string
		var n int
		if err := rows.Scan(&ext, &n); err != nil {
			return 0, 0, nil, err
		}
		if ext == "" {
			ext = "(без расширения)"
		}
		byExt[ext] = n
	}
	return total, bytes, byExt, rows.Err()
}

// escapeLike экранирует служебные символы LIKE, чтобы запрос с % или _
// искал буквальное совпадение, а не произвольный шаблон.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func (s *Store) ListFiles(ctx context.Context, taskID string, limit int) ([]FileEntry, error) {
	limit = normLimit(limit, 200)
	q := `SELECT id, task_id, url, filename, ext, size, mime, source_page, verdict, found_at
	      FROM file_catalog`
	args := []any{}
	if taskID != "" {
		q += ` WHERE task_id=?`
		args = append(args, taskID)
	}
	q += ` ORDER BY found_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var out []FileEntry
	for rows.Next() {
		var f FileEntry
		var found sql.NullString
		if err := rows.Scan(&f.ID, &f.TaskID, &f.URL, &f.Filename, &f.Ext,
			&f.Size, &f.MIME, &f.SourcePage, &f.Verdict, &found); err != nil {
			return nil, err
		}
		f.FoundAt = parseTS(found)
		out = append(out, f)
	}
	return out, rows.Err()
}

func tsOrEmpty(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// CountFilesWithoutVerdict возвращает число строк каталога с пустой колонкой
// verdict.
//
// Нужно выводу CLI: строки, записанные до появления вердикта, не находятся ни по
// одному фильтру -verdict и -risk, и без счётчика «по фильтру ничего не найдено»
// выглядит как «таких файлов нет», хотя каталог наглядно содержит и epub, и pdf.
//
// В предикате только сравнение с пустой строкой, без члена IS NULL. Колонка
// объявлена в CREATE TABLE как TEXT NOT NULL с пустой строкой по умолчанию и
// через ALTER TABLE не добавлялась, поэтому NULL в ней не может появиться ни в
// одной базе, созданной этой схемой. Лишний член стоит дорого: замер EXPLAIN
// QUERY PLAN на копии живой базы дал SCAN file_catalog USING COVERING INDEX для
// предиката с IS NULL и SEARCH USING INDEX idx_file_verdict (verdict=?) для
// предиката без него - то есть с IS NULL счётчик обходит весь индекс, а без
// него читает только диапазон пустых меток.
func (s *Store) CountFilesWithoutVerdict(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM file_catalog WHERE verdict = ''`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count files without verdict: %w", err)
	}
	return n, nil
}

// backfillBatch - число строк, заполняемых одной транзакцией.
const backfillBatch = 400

// BackfillFileVerdicts заполняет колонку verdict у строк, где она пустая.
//
// Вердикт выводится из расширения и имени файла, без сети и без чтения
// содержимого, поэтому заполнение задним числом дёшево, детерминировано и
// безопасно повторять: второй вызов не найдёт пустых строк и вернёт ноль.
//
// Нужно потому, что колонка существовала с самого начала, но не заполнялась
// никогда. После правки записи новые строки получают вердикт при вставке, а
// старые остались бы пустыми навсегда, и фильтр по метке молча не находил бы их.
//
// Заполнение идёт порциями по backfillBatch строк, и у каждой порции своя
// транзакция. Прежняя версия читала все пустые строки одним запросом без
// ограничения в срез, а затем обновляла их построчно в одной транзакции, и её
// комментарий утверждал, что такой цикл «держит блокировку короткой». Утверждение
// неверно: пишущая транзакция SQLite держит блокировку записи с первой записи до
// COMMIT, поэтому одна транзакция на весь каталог - это ровно та длинная
// блокировка, которой комментарий якобы избегал. При SetMaxOpenConns(1) она к тому
// же занимала единственный коннект на всё время заполнения, и поисковый сервер не
// отвечал ни на один запрос.
//
// Выборка порции ограничена в самом запросе и, по замеру EXPLAIN QUERY PLAN на
// копии живой базы, идёт поиском по idx_file_verdict, а не полным сканом
// таблицы: прежний предикат с IS NULL давал SCAN file_catalog, потому что ext и
// filename в индекс не входят. Курсор порции не нужен - обновлённые строки
// получают непустую метку и сами покидают выборку, поэтому запрос той же формы
// возвращает следующую порцию.
//
// Атомарность теперь на уровне порции, а не всего заполнения, и это строго
// лучше прежнего поведения при прерывании: раньше откатывалось всё и каталог
// оставался полностью незаполненным, теперь завершённые порции остаются в базе,
// а повторный вызов доделывает остаток.
//
// Возвращает число обновлённых строк.
func (s *Store) BackfillFileVerdicts(ctx context.Context) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		selected, updated, err := s.backfillOneBatch(ctx, backfillBatch)
		if err != nil {
			return total, err
		}
		if selected == 0 {
			return total, nil
		}
		// Защита от зависания. По контракту filex.ClassifyRef всегда возвращает
		// непустую метку: нераспознанное расширение получает «unknown». Поэтому
		// строка не может остаться в выборке после обновления. Если контракт
		// когда-нибудь нарушится, одна и та же порция будет выбираться вечно -
		// вернуть ошибку лучше, чем повесить команду и поисковый сервер вместе
		// с ней.
		if updated == 0 {
			return total, fmt.Errorf(
				"заполнение вердиктов не продвинулось: выбрано %d строк, обновлено 0", selected)
		}
		total += updated
	}
}

// backfillOneBatch заполняет одну порцию строк с пустой меткой и фиксирует её
// отдельной транзакцией. Возвращает число выбранных строк и число обновлённых.
//
// Транзакция закрывается на любом пути, включая пустую выборку: при
// SetMaxOpenConns(1) незакрытая транзакция оставила бы пул без единственного
// коннекта.
func (s *Store) backfillOneBatch(ctx context.Context, limit int) (int, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin backfill: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, ext, filename FROM file_catalog WHERE verdict = '' LIMIT ?`, limit)
	if err != nil {
		tx.Rollback()
		return 0, 0, fmt.Errorf("select files without verdict: %w", err)
	}
	type pending struct {
		id      int64
		verdict string
	}
	todo := make([]pending, 0, limit)
	for rows.Next() {
		var id int64
		var ext, name string
		if err := rows.Scan(&id, &ext, &name); err != nil {
			rows.Close()
			tx.Rollback()
			return 0, 0, err
		}
		v := filex.ClassifyRef(filex.Ref{Ext: ext, Filename: name})
		// Метка «unknown» тоже записывается: пустая колонка неотличима от
		// незаполненной, а явная метка показывает, что файл разобран, но
		// расширение не распознано. Та же непустая метка выводит строку из
		// выборки следующей порции.
		todo = append(todo, pending{id: id, verdict: v})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		tx.Rollback()
		return 0, 0, err
	}
	rows.Close()

	if len(todo) == 0 {
		tx.Rollback()
		return 0, 0, nil
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE file_catalog SET verdict = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return len(todo), 0, fmt.Errorf("prepare backfill: %w", err)
	}
	defer stmt.Close()

	updated := 0
	for _, p := range todo {
		if err := ctx.Err(); err != nil {
			tx.Rollback()
			return len(todo), updated, err
		}
		res, err := stmt.ExecContext(ctx, p.verdict, p.id)
		if err != nil {
			tx.Rollback()
			return len(todo), updated, fmt.Errorf("backfill id %d: %w", p.id, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			updated += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return len(todo), updated, fmt.Errorf("commit backfill: %w", err)
	}
	return len(todo), updated, nil
}

func parseTS(v sql.NullString) time.Time {
	if !v.Valid || v.String == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, v.String); err == nil {
			return t
		}
	}
	return time.Time{}
}
