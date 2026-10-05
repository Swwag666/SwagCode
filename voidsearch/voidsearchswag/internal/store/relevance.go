package store

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// Vote - голос судьи по одному URL.
type Vote struct {
	QueryHash string
	URL       string
	Host      string
	Score     float64
	Author    string
}

// SubmitVotes пишет оценки пачкой: один вызов judge_submit - много строк.
// Score клампится в 0..1, пустые url отбрасываются, хост выводится из url.
//
// Пачка пишется одной транзакцией. Раньше каждый голос был отдельным
// ExecContext при MaxOpenConns(1): пачка в несколько тысяч оценок держала
// единственное соединение и блокировала все остальные инструменты, а при
// ошибке в середине часть строк оставалась записанной, и вызывающий не мог
// узнать, сколько именно - клиент повторял отправку и задваивал голоса.
func (s *Store) SubmitVotes(ctx context.Context, votes []Vote) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("submit vote: begin: %w", err)
	}
	n := 0
	for _, v := range votes {
		u := strings.TrimSpace(v.URL)
		if u == "" || strings.TrimSpace(v.QueryHash) == "" {
			continue
		}
		score := v.Score
		// NaN не проходит ни одно из сравнений ниже: score < 0 и score > 1
		// для NaN дают false, поэтому значение ушло бы в базу как есть и
		// дальше отравляло бы каждый пересчёт бонусов. Заменяем на нейтральное.
		if math.IsNaN(score) {
			score = 0.5
		}
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		// Хост приводится к нижнему регистру независимо от того, пришёл ли он
		// от вызывающего или выведен из url. Раньше v.Host использовался как
		// есть, а выведенный - через ToLower, поэтому ключи в смешанном регистре
		// не совпадали с поиском strings.ToLower(hostOf(r.URL)) в rerank, и
		// выученный бонус для таких хостов молча не применялся.
		host := strings.ToLower(strings.TrimSpace(v.Host))
		if host == "" {
			if pu, perr := url.Parse(u); perr == nil {
				host = strings.ToLower(pu.Hostname())
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO relevance(query_hash, url, host, score, author, updated_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(query_hash, url, author) DO UPDATE SET
				score=excluded.score, host=excluded.host, updated_at=CURRENT_TIMESTAMP`,
			v.QueryHash, u, host, score, v.Author); err != nil {
			_ = tx.Rollback()
			return n, fmt.Errorf("submit vote: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("submit vote: commit: %w", err)
	}
	return n, nil
}

// HostQuality считает бонус хостов по голосам: средний скор с весом голоса
// 1/sqrt(всего голосов автора) против накрутки. Возвращает хосты, за которые
// проголосовало минимум minVotes разных судей, и бонус clamp((avg-0.5)*6, -3, +3):
// полный балл равен весу токена в заголовке - судья перебивает, но не диктует.
//
// Весь подсчёт выполняется в SQL одним запросом.
//
// Прежняя версия выгружала всю таблицу relevance в срез Go и агрегировала в
// памяти двумя проходами: первый собирал число голосов каждого автора, второй
// считал взвешенные средние. Буферизация была нужна именно потому, что вес
// голоса зависит от общего числа голосов автора, которое неизвестно до конца
// обхода. Но это же выражается подзапросом с GROUP BY author, и тогда база
// агрегирует сама.
//
// Разница существенна, потому что HostQuality вызывается на горячем пути поиска,
// а не в фоновой задаче: при SetMaxOpenConns(1) полная загрузка таблицы голосов
// блокировала единственное соединение на каждый поисковый запрос, а объём памяти
// рос пропорционально всей истории оценок и не был ограничен ничем.
//
// Семантика:
//   - вес автора 1/sqrt(n), где n - число его голосов с непустым host. Ветвь
//     «n <= 1 даёт вес 1.0» не нужна отдельно: 1/sqrt(1) = 1, то есть формула
//     покрывает случай единственного голоса;
//   - подсчёт голосов автора ограничен строками с непустым host, как и прежде:
//     прежний код увеличивал счётчик в обходе выборки, которая уже была
//     отфильтрована по host<>”;
//   - порог minVotes применяется к числу РАЗНЫХ авторов хоста через HAVING
//     COUNT(DISTINCT author), а не к числу строк. Это намеренное изменение: при
//     подсчёте строк один судья с пятью голосами давал хосту максимальный бонус
//     +3, и порог «минимум три мнения» превращался в «минимум три строки», то
//     есть в отсутствие порога вовсе. Взвешивание 1/sqrt(n) гасит накрутку в
//     среднем, но не в факте доверия: хост с пятью голосами одного человека
//     проходил порог наравне с хостом, за который высказались трое. Теперь
//     доверие измеряется людьми, как и задумано порогом;
//   - зажим бонуса в [-3, +3] выполняется через MIN/MAX.
//
// Наличие SQRT, MIN и MAX проверено на фактической сборке modernc.org/sqlite:
// математические функции требуют SQLITE_ENABLE_MATH_FUNCTIONS, и предполагать их
// наличие нельзя.
//
// HostQuality считает сводку по ВСЕЙ таблице: она нужна статистике («бонусы:
// N хостов») и ночному тику. Реранк обязан брать HostQualityForQuery: оценка
// судьи отвечает на вопрос «насколько этот URL отвечает данному запросу», и
// глобальный бонус превращал её в вердикт «хост хорош вообще».
func (s *Store) HostQuality(ctx context.Context, minVotes int) (map[string]float64, error) {
	return s.hostQuality(ctx, "", minVotes)
}

// HostQualityForQuery считает бонусы хостов по голосам одного запроса. Веса
// авторов и порог minVotes считаются в пределах того же запроса: автор,
// оставивший тысячу оценок по чужим запросам, не должен терять вес здесь, а три
// его голоса по чужому запросу не должны давать доверие этому.
//
// Пустой queryHash возвращает пустую карту, а не сводку по всей базе: молчаливое
// применение чужих голосов к запросу и было дефектом, поэтому вырожденный вызов
// обязан оставаться без бонусов.
func (s *Store) HostQualityForQuery(ctx context.Context, queryHash string, minVotes int) (map[string]float64, error) {
	if strings.TrimSpace(queryHash) == "" {
		return map[string]float64{}, nil
	}
	return s.hostQuality(ctx, queryHash, minVotes)
}

// hostQuality - общий расчёт. Пустой queryHash означает всю таблицу, иначе и
// внешний запрос, и подзапрос весов ограничиваются одним query_hash. Порядок
// аргументов следует за порядком плейсхолдеров в тексте: подзапрос в JOIN идёт
// раньше основного WHERE, а порог minVotes стоит последним в HAVING.
func (s *Store) hostQuality(ctx context.Context, queryHash string, minVotes int) (map[string]float64, error) {
	if minVotes <= 0 {
		minVotes = 3
	}
	subFilter, mainFilter := "", ""
	args := make([]any, 0, 3)
	if queryHash != "" {
		subFilter = " AND query_hash=?"
		mainFilter = " AND r.query_hash=?"
		args = append(args, queryHash, queryHash)
	}
	args = append(args, minVotes)

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.host,
		       MIN(3.0, MAX(-3.0,
		           (SUM(r.score * w.weight) / SUM(w.weight) - 0.5) * 6.0)) AS bonus
		FROM relevance r
		JOIN (SELECT author, 1.0 / SQRT(COUNT(*)) AS weight
		      FROM relevance
		      WHERE host <> ''`+subFilter+`
		      GROUP BY author) w
		  ON w.author = r.author
		WHERE r.host <> ''`+mainFilter+`
		GROUP BY r.host
		HAVING COUNT(DISTINCT r.author) >= ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("host quality: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var host string
		var bonus float64
		if err := rows.Scan(&host, &bonus); err != nil {
			return nil, err
		}
		out[host] = bonus
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// VoteStats отдаёт объём судейской базы: голосов, запросов, хостов.
func (s *Store) VoteStats(ctx context.Context) (votes, queries, hosts int, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relevance`).Scan(&votes); err != nil {
		return 0, 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT query_hash) FROM relevance`).Scan(&queries); err != nil {
		return 0, 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT host) FROM relevance WHERE host<>''`).Scan(&hosts); err != nil {
		return 0, 0, 0, err
	}
	return votes, queries, hosts, nil
}

// VotesForQuery отдаёт объём судейской базы по одному запросу: сколько строк
// хранится и сколько разных авторов голосовали.
//
// Метод нужен потому, что число обработанных элементов пачки не равно числу строк
// в базе. Оценка того же url тем же автором обновляет существующую строку, поэтому
// повторная отправка после сбоя связи возвращала «accepted: 2» при неизменных
// семи строках, и клиент не мог отличить «добавлено два голоса» от «обновлены два
// существующих». Ровно ради этого сценария SubmitVotes и возвращает счётчик, так
// что молчание здесь обесценивало прежнюю правку.
//
// Отдельно считается число авторов: порог доверия к оценке имеет смысл считать по
// людям, а не по строкам, и без этого числа судейская база выглядит объёмнее, чем
// она есть.
func (s *Store) VotesForQuery(ctx context.Context, queryHash string) (rows, authors int, err error) {
	if strings.TrimSpace(queryHash) == "" {
		return 0, 0, nil
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT author) FROM relevance WHERE query_hash=?`, queryHash).Scan(&rows, &authors)
	if err != nil {
		return 0, 0, fmt.Errorf("votes for query: %w", err)
	}
	return rows, authors, nil
}
