package store

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS sources (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT '',
		quality REAL NOT NULL DEFAULT 0,
		popularity REAL NOT NULL DEFAULT 0,
		first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_ok DATETIME
	)`,

	`CREATE TABLE IF NOT EXISTS onion_pool (
		url TEXT PRIMARY KEY,
		status TEXT NOT NULL DEFAULT 'unknown',
		category TEXT NOT NULL DEFAULT '',
		latency_avg INTEGER NOT NULL DEFAULT 0,
		success_rate REAL NOT NULL DEFAULT 0,
		fail_streak INTEGER NOT NULL DEFAULT 0,
		last_probe DATETIME,
		first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	`CREATE TABLE IF NOT EXISTS selectors (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url_pattern TEXT NOT NULL,
		field TEXT NOT NULL,
		selector TEXT NOT NULL,
		strategy TEXT NOT NULL DEFAULT 'css',
		confidence REAL NOT NULL DEFAULT 1.0,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(url_pattern, field)
	)`,

	`CREATE TABLE IF NOT EXISTS hunts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		query TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'auto',
		schedule_min INTEGER NOT NULL DEFAULT 360,
		last_run DATETIME,
		last_hash TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	`CREATE TABLE IF NOT EXISTS cache (
		query_hash TEXT PRIMARY KEY,
		mode TEXT NOT NULL,
		payload TEXT NOT NULL,
		expires_at DATETIME NOT NULL
	)`,

	`CREATE INDEX IF NOT EXISTS idx_cache_expires ON cache(expires_at)`,

	`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		progress INTEGER NOT NULL DEFAULT 0,
		message TEXT NOT NULL DEFAULT '',
		result_ref TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	`CREATE TABLE IF NOT EXISTS file_catalog (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL,
		filename TEXT NOT NULL DEFAULT '',
		ext TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL DEFAULT 0,
		mime TEXT NOT NULL DEFAULT '',
		source_page TEXT NOT NULL DEFAULT '',
		verdict TEXT NOT NULL DEFAULT '',
		found_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(task_id, url)
	)`,

	`ALTER TABLE onion_pool ADD COLUMN title TEXT NOT NULL DEFAULT ''`,

	`ALTER TABLE onion_pool ADD COLUMN description TEXT NOT NULL DEFAULT ''`,

	`CREATE INDEX IF NOT EXISTS idx_onion_status ON onion_pool(status)`,

	`CREATE INDEX IF NOT EXISTS idx_onion_title ON onion_pool(title)`,

	`DELETE FROM file_catalog WHERE id NOT IN (
		SELECT MIN(id) FROM file_catalog GROUP BY url
	)`,

	`CREATE UNIQUE INDEX IF NOT EXISTS idx_file_url ON file_catalog(url)`,

	`CREATE INDEX IF NOT EXISTS idx_file_ext ON file_catalog(ext)`,

	`CREATE TABLE IF NOT EXISTS engine_health (
		name TEXT PRIMARY KEY,
		url TEXT NOT NULL DEFAULT '',
		live INTEGER NOT NULL DEFAULT 0,
		latency_avg INTEGER NOT NULL DEFAULT 0,
		success_rate REAL NOT NULL DEFAULT 0,
		fail_streak INTEGER NOT NULL DEFAULT 0,
		probes INTEGER NOT NULL DEFAULT 0,
		successes INTEGER NOT NULL DEFAULT 0,
		last_probe DATETIME,
		disabled INTEGER NOT NULL DEFAULT 0
	)`,

	// engines - поисковые движки сверх зашитых сидов: найденные
	// автопромоутом среди живых сервисов пула. auto=1 отличает их от
	// ручных записей и позволяет чистить отдельно.
	`CREATE TABLE IF NOT EXISTS engines (
		name TEXT PRIMARY KEY,
		base TEXT NOT NULL,
		path TEXT NOT NULL DEFAULT '/search?q={q}',
		selector TEXT NOT NULL DEFAULT 'a[href*=''.onion'']',
		category TEXT NOT NULL DEFAULT 'general',
		auto INTEGER NOT NULL DEFAULT 1,
		last_ok DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// relevance - голоса LLM-судьи: оценка url по запросу. Веса реранка
	// учатся на этой таблице ночным пересчётом.
	`CREATE TABLE IF NOT EXISTS relevance (
		query_hash TEXT NOT NULL,
		url TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		score REAL NOT NULL DEFAULT 0.5,
		author TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (query_hash, url, author)
	)`,

	`CREATE INDEX IF NOT EXISTS idx_relevance_host ON relevance(host)`,

	// Индексы под запросы, которые сортируют или фильтруют всю таблицу.
	//
	// ListTasks делает ORDER BY created_at DESC LIMIT ? с возможным
	// WHERE status=?, а store.Stats считает строки WHERE status='running'. Без
	// индекса каждый вызов сортировал всю таблицу задач: с ростом истории
	// список задач замедлялся линейно, хотя показывал всегда первые сто строк.
	`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at)`,

	`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)`,

	// ListFiles сортирует по found_at DESC, а SearchFiles - по
	// size DESC, found_at DESC. Обе выборки ограничены LIMIT, но без индекса
	// SQLite сортирует весь каталог, чтобы отдать первые пятьдесят строк: на
	// десятках тысяч файлов это заметно, а при SetMaxOpenConns(1) ещё и
	// блокирует единственный коннект на всё время сортировки.
	`CREATE INDEX IF NOT EXISTS idx_file_found ON file_catalog(found_at)`,

	// Фильтр по verdict появился вместе с заполнением колонки: без индекса
	// «files -verdict ebook» сканирует весь каталог, хотя меток всего одиннадцать.
	`CREATE INDEX IF NOT EXISTS idx_file_verdict ON file_catalog(verdict)`,

	// Составной индекс под SearchFiles с фильтром по метке: WHERE verdict=?
	// ORDER BY size DESC, found_at DESC LIMIT ?. Одностолбцовый idx_file_verdict
	// отбирает строки, но сортировку SQLite делает отдельно - план запроса на
	// заполненной копии базы показывал «USE TEMP B-TREE FOR ORDER BY».
	`CREATE INDEX IF NOT EXISTS idx_file_verdict_size_found ON file_catalog(verdict, size, found_at)`,

	// Составной индекс под ListTasks с фильтром: WHERE status=? ORDER BY
	// created_at DESC LIMIT ?. Одностолбцовый idx_tasks_status отбирает строки,
	// но сортировку SQLite делает отдельно - план показывал «USE TEMP B-TREE FOR
	// ORDER BY». Составной индекс покрывает и отбор, и порядок.
	//
	// ВАЖНО про порядок операторов в этом списке. Список миграций - append-only
	// журнал, где версия равна позиции оператора, а Migrate пропускает всё с
	// версией не выше MAX(version) из schema_migrations. Вставка нового оператора
	// в середину сдвигает номера всех последующих, и уже развёрнутая база
	// считает новый оператор применённым и молча его пропускает.
	//
	// Именно это случилось при первой попытке: составной индекс tasks был вставлен
	// рядом с остальными индексами tasks, база с MAX(version)=23 пропустила его, а
	// версия выросла до 24 за счёт повторного выполнения уже существующего
	// idx_file_verdict. Индекс не создался, и заметно это стало только по плану
	// запроса - по росту номера миграции дефект не виден.
	//
	// Правило для всех будущих миграций: только дописывать в конец. Перестановка
	// или вставка в середину ломает уже развёрнутые базы.
	`CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks(status, created_at)`,

	// hunt_hits - история находок охоты. Строка на url, а не на находку:
	// охота при смене выдачи отдаёт весь список url, и оператору важно видеть,
	// какие из них появились впервые, а какие кочуют из выдачи в выдачу.
	//
	// До этой таблицы находка жила ровно столько, сколько длился один прогон:
	// hunt run печатал url, обновлял last_hash и забывал список, а перезапуск
	// процесса, вызов MCP или просто закрытый терминал оставляли оператора без
	// истории. Живой замер показывал это напрямую: после находки с десятью url в
	// базе не было ни одной таблицы, где их можно было бы прочитать.
	//
	// Запрос и режим копируются в строку намеренно: история обязана оставаться
	// читаемой, даже если охоту потом переименовали или перевели в другой режим.
	`CREATE TABLE IF NOT EXISTS hunt_hits (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hunt_id INTEGER NOT NULL,
		query TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL,
		first_found DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_found DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		times_seen INTEGER NOT NULL DEFAULT 1,
		UNIQUE(hunt_id, url)
	)`,

	// ListHuntHits делает WHERE hunt_id=? ORDER BY last_found DESC, id DESC
	// LIMIT ?. Составной индекс покрывает и отбор, и порядок: без него SQLite
	// собирала бы temp B-tree на всей истории охоты, а при SetMaxOpenConns(1)
	// это блокировало бы единственный коннект на всё время сортировки.
	`CREATE INDEX IF NOT EXISTS idx_hunt_hits_hunt_found ON hunt_hits(hunt_id, last_found, id)`,
}
