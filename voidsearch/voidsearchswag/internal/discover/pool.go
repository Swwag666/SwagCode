package discover

import (
	"context"
	"fmt"
	"time"

	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/store"
)

// Pool связывает discovery-слой с базой и health-пулом: найденные адреса
// попадают в onion_pool, откуда их берут поиск и пробы живости.
type Pool struct {
	Store  *store.Store
	Finder *Finder
	Crawl  *Crawler
	Log    Logger

	// saveBudget/measureBudget/writeBudget - потолки несократимых фаз
	// записи: пула, замера размеров и каталога. Нулевое значение означает
	// продакшн-дефолт (90s/60s/300s). Настраивается только из тестов:
	// живой прогон 168 показал, что общий бюджет делает файловую фазу
	// заложницей объёма пула, и покрытие этого требует мгновенного
	// исчерпания бюджета, а не минут ожидания.
	saveBudget  time.Duration
	sizesBudget time.Duration
	filesBudget time.Duration
}

func (p *Pool) poolBudget() time.Duration {
	// Этап 174: != 0 вместо > 0. Нулевое значение - продакшн-дефолт 90s,
	// а отрицательное - «бюджет уже истёк»: WithTimeout с прошедшим d
	// помечает контекст мёртвым синхронно, без ожидания первого тика
	// таймера. Нано-бюджет (1ns) для этого не годится: тик асинхронен, и
	// лёгкий батч из синхронного sqlite успевает записаться до него -
	// тест 168 ловил именно эту гонку, а не контракт «не писать при
	// мёртвом бюджете». Отрицательные бюджеты существуют только в тестах.
	if p.saveBudget != 0 {
		return p.saveBudget
	}
	return 90 * time.Second
}

func (p *Pool) measureBudget() time.Duration {
	if p.sizesBudget > 0 {
		return p.sizesBudget
	}
	return 60 * time.Second
}

func (p *Pool) writeBudget() time.Duration {
	if p.filesBudget > 0 {
		return p.filesBudget
	}
	return 300 * time.Second
}

type Options struct {
	Depth        int
	MaxHosts     int
	PerHostDelay time.Duration
	WithCrawl    bool
	Seeds        []string
}

type Result struct {
	Found   int `json:"found"`
	New     int `json:"new"`
	Updated int `json:"updated"`
	Crawled int `json:"crawled"`
	Files   int `json:"files"`
	// Revisited - сколько найденных обходом файлов уже лежит в каталоге от
	// прежних прогонов.task_id первой находки не переписывается, поэтому
	// эти записи не появятся под file_task_id этого прогона: без отдельного
	// счётчика «files: 5» на сплошь повторных находках читалось как
	// «записано 5», а file_search по метке из ответа возвращал пустоту.
	Revisited int `json:"revisited"`
	Probed    int `json:"probed"`
	// SaveFailed - сколько адресов и их мет не удалось записать в пул. Found
	// растёт всегда, а New только при удачной записи, поэтому «найдено 3,
	// новых 0» без этого счётчика означало сразу два разных факта: «адреса уже
	// известны» и «база не приняла ни одной записи».
	SaveFailed int `json:"save_failed"`
	// FileSaveFailed - сколько найденных файлов не дошло до каталога. Счётчик
	// отдельный: Files считает записанные, и без пары к нему «файлов 0»
	// неотличимо от «каталог не принял ни одной записи».
	FileSaveFailed int `json:"file_save_failed"`
	// ProbeFailed - сколько проб живости по итогам обхода не записалось.
	ProbeFailed int `json:"probe_failed"`
	// ProbeSkipped - сколько проб живости не состоялось вовсе: запрос не ушёл в
	// сеть из-за отсутствия tor или прокси. Счётчик отделён и от ProbeFailed
	// (запись не удалась), и от Probed (записано): без него обход на машине без
	// tor печатал «живость: обновлено по обходу 1 хостов», подавая отсутствующий
	// транспорт как полезную работу, и портил статистику живых адресов.
	ProbeSkipped int `json:"probe_skipped"`
	// LastError - первая причина отказа записи. Первая, а не последняя: при
	// мёртвой базе все отказы одинаковы, и объясняет картину именно первый.
	LastError string `json:"last_error,omitempty"`
	// FileTaskID - метка задачи, под которую записаны найденные обходом
	// файлы. Обход с crawl пишет в file_catalog записи с task_id
	// «crawl-<время>», но ответ его не называл, и файлы становились
	// невидимыми для единственного пути юзера к ним: file_search по
	// task_id. Живой замер этапа 167 на стенде agenthunt: discover с
	// crawl=true дописал 51 файл (2222jzj4 47, 2b5calm3 4) с task_id
	// crawl-20261001-190804 через 92 секунды ПОСЛЕ ответа параллельного
	// collect_files - тот честно отчитался своим mcp-... и своим
	// catalog_total, а дальнейший рост каталога для вызывающего был
	// необъясним: по его task_id стабильно 88 из 139 новых. Ответ
	// показывает метку, file_search task_id=<она> возвращает записи, и
	// «потерянная треть» превращается в явную принадлежность задаче.
	FileTaskID string         `json:"file_task_id,omitempty"`
	Sources    []SourceReport `json:"sources"`
	Crawl      *CrawlReport   `json:"crawl,omitempty"`
	Elapsed    string         `json:"elapsed"`
	Addresses  []string       `json:"addresses,omitempty"`
}

// FailSave учитывает адрес или мету, которые пул не принял. Метод, а не
// инкремент в цикле: точек записи несколько, и расхождение между ними вернуло бы
// молчание в одной из них.
func (r *Result) FailSave(err error) {
	if r == nil || err == nil {
		return
	}
	r.SaveFailed++
	r.setErr(err)
}

// FailFile учитывает файл, который не дошёл до каталога.
func (r *Result) FailFile(err error) {
	if r == nil || err == nil {
		return
	}
	r.FileSaveFailed++
	r.setErr(err)
}

// FailProbe учитывает пробу живости, которая не записалась.
func (r *Result) FailProbe(err error) {
	if r == nil || err == nil {
		return
	}
	r.ProbeFailed++
	r.setErr(err)
}

func (r *Result) setErr(err error) {
	if r.LastError == "" {
		r.LastError = err.Error()
	}
}

// SkipProbe учитывает пробу живости, которая не состоялась из-за отсутствия
// транспорта. Ошибки здесь нет, как нет и вердикта о сервисе, поэтому причина в
// LastError не пишется: LastError объясняет отказы записи, а не состояние
// окружения автора.
func (r *Result) SkipProbe() {
	if r == nil {
		return
	}
	r.ProbeSkipped++
}

// Run собирает адреса из clearnet-источников, при необходимости обходит
// их рекурсивно, дедуплицирует и записывает в базу. Уже известные адреса
// не считаются новыми, но их мета обновляется: заголовок и категория
// уточняются от прогона к прогону, иначе база навсегда остаётся списком
// голых адресов, по которому нельзя искать.
func (p *Pool) Run(ctx context.Context, opts Options) (Result, error) {
	start := time.Now()
	var res Result

	if p.Store == nil {
		return res, fmt.Errorf("store не задан")
	}
	if p.Finder == nil {
		return res, fmt.Errorf("finder не задан")
	}

	cands, reports := p.Finder.DiscoverClearnet(ctx)
	res.Sources = reports
	p.logf("discover: clearnet-источники дали %d адресов", len(cands))

	if n, err := p.Store.CleanOnionTitles(ctx); err != nil {
		p.logf("discover: чистка заголовков: %v", err)
	} else if n > 0 {
		p.logf("discover: вычищено мусорных заголовков: %d", n)
	}

	if n, err := p.Store.CleanFileCatalog(ctx); err != nil {
		p.logf("discover: чистка каталога: %v", err)
	} else if n > 0 {
		p.logf("discover: вычищено мусорных файлов: %d", n)
	}

	if opts.WithCrawl && p.Crawl != nil {
		// Этап 171: потолок вызова обязан резать и сиды, и сам обход,
		// причём одним числом. Прежде opts.MaxHosts доходил только до
		// среза сидов: живой BEFORE (стенд vss171, чистая база) -
		// max_hosts=15 в запросе, crawl.max_hosts=50 и 50 обойдённых
		// страниц в ответе (15 сидов + 35 ссылок), «достигнут потолок
		// хостов» срабатывал по серверным 50. Молчаливый вызов был крив
		// в другую сторону: обработчик подставлял жёсткие 50, и на
		// сервере с потолком 7 срез сидов брал 50 адресов, из которых
		// обходчик резал первые 7 по своей настройке. Теперь копия
		// обходчика несёт запрошенный потолок (ноль и минус - серверный,
		// как у CLI-флага), а срез сидов берёт его же: сидов не может
		// быть больше потолка. Глубина - этап 170: тоже копией.
		crawler := p.Crawl
		if opts.Depth > 0 {
			crawler = crawler.WithDepth(opts.Depth)
		}
		if opts.MaxHosts > 0 {
			crawler = crawler.WithMaxHosts(opts.MaxHosts)
		}
		seeds := opts.Seeds
		seedCut := 0
		if len(seeds) == 0 {
			// Этап 174: срез входа теперь виден числом. Второе значение -
			// сколько подходящих кандидатов не поместилось в потолок. Живой
			// BEFORE-факт смоука этапа 172 (агент C): 14217 кандидатов,
			// потолок 3, сидов 3, срез 14214 - и ни одно поле ответа об
			// этом не знало. Явные сиды сверх потолка срезает сам Crawl,
			// и их число в SeedsCut не входит: их событие - limit_hit
			// (контракт maxhosts).
			seeds, seedCut = p.seedHosts(ctx, cands, crawler.Cfg.MaxHosts)
		}
		if len(seeds) > 0 {
			crawled, crep := crawler.Crawl(ctx, seeds)
			crep.SeedsCut = seedCut
			res.Crawl = &crep
			res.Crawled = crep.Pages
			cands = append(cands, crawled...)
			p.logf("discover: обход дал %d адресов за %s", len(crawled), crep.Elapsed)
		}
	}

	merged := Merge(cands)
	res.Found = len(merged)
	res.Addresses = make([]string, 0, len(merged))
	for _, c := range merged {
		res.Addresses = append(res.Addresses, c.Address)
	}

	// Фаза записи: всё собранное доезжает до базы. Живой замер этапа 167
	// (смоук-агенты A3 и C4 на стенде agenthunt): discover с timeout
	// 420..480 собирал 313..315 файловых ссылок, а rctx к моменту записи
	// был уже отменён - files=0 при светящемся file_task_id, 43 отказа
	// «set onion meta: context deadline exceeded» на ходах мета/пула,
	// и повторные file_search по метке возвращали пустоту: «заявка без
	// доставки». Этап 167 закрыл это для файлов; этап 168 закрывает для
	// остальных записей того же прогона: новые адреса, мета найденных
	// страниц, живость обхода и заголовки Titles - они пишутся под
	// несократимым saveCtx, а файлы - под собственными бюджетами ниже,
	// чтобы большой пул не съедал файловую фазу. Пул может нести десятки
	// тысяч адресов, поэтому 90s; без отмены родителя, но с потолком,
	// чтобы зависшая запись не подвесила вызов. Отказы хвоста видны как
	// save_failed с last_error, а не пропадают молча.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), p.poolBudget())
	defer saveCancel()

	// Этап 174: батч-запись. Поадресный цикл ниже - фолбэк, и он обязан
	// остаться, а не быть выброшенным: живой замер этапа 173 показал
	// поадресную запись вторым по тяжести куском хвоста (22.3с/26.6с при
	// бюджетах 30с/90с - 14 тысяч адресов по автокоммиту на каждый), но
	// у неё есть свойство, которое батч даёт не всегда: гранулярность
	// отказов. Один битый адрес в транзакции откатывает весь батч, и без
	// фолбэка единственная ошибка записи превращалась бы в «пул не принял
	// ничего» с одним save_failed на всё. При отказе батча вызывающий
	// падает на поадресный цикл, который повторяет попытку каждому адресу
	// и честно считает отказавших. Путь почти никогда не ходячий - база
	// локальная, реальный отказ это мёртвый ctx или сломанный файл, - но
	// контракт этапа 168 («отказы видны, а не пропадают») пережил и его.
	if len(merged) > 0 {
		items := make([]store.OnionSync, 0, len(merged))
		for _, c := range merged {
			items = append(items, store.OnionSync{
				URL:         c.Address,
				Title:       c.Title,
				Description: c.Description,
				Category:    c.Category,
			})
		}
		newN, updN, err := p.Store.SyncOnions(saveCtx, items)
		if err == nil {
			res.New += newN
			res.Updated += updN
			p.logf("discover: пул записан батчем: новых %d, мета %d", newN, updN)
		} else {
			p.logf("discover: батч-запись пула: %v - перехожу на поадресную", err)
			p.savePoolOneByOne(saveCtx, merged, &res)
		}
	}

	// Обход - это проба живости, только дороже: он уже сходил на хост и
	// знает, ответил тот или нет. Запись идёт после того, как адреса
	// заведены в пул, иначе записывать живость было бы не для чего.
	// Без этого мёртвые адреса переспрашиваются каждым прогоном и съедают
	// потолок хостов, который мог бы уйти на живые. Живость - часть
	// собранного, поэтому пишется под saveCtx, а не под rctx обрезки.
	if res.Crawl != nil {
		res.Probed = p.recordCrawl(saveCtx, res.Crawl, &res)
		if res.Probed > 0 {
			p.logf("discover: живость по обходу записана для %d хостов", res.Probed)
		}
	}

	if res.Crawl != nil && len(res.Crawl.FileRefs) > 0 {
		// Секунды в метке - коллизия: повторный discover в ту же секунду
		// наследует task_id первого, и file_search по метке смешивает
		// задачи. Живой случай этапа 168: повторный прогон стенда after167b
		// получил crawl-20261002-122334 - секунда та же, что у первого.
		// Миллисекунды снимают коллизию, сохраняя сортируемый формат.
		taskID := "crawl-" + time.Now().Format("20060102-150405.000")
		res.FileTaskID = taskID
		// Бюджет файлов - собственный, а не остаток пула. Живой замер
		// этапа 168 (стенд after167b): discover нашёл 14385 адресов, пул
		// съел почти весь общий 90s, и файловая фаза вышла на исчерпанный
		// бюджет - 200 найденных ссылок, files=0, revisited=0, и ни одного
		// file_save_failed: файлы «потерялись» так же молча, как до 167,
		// только причиной стал потолок пула.
		//
		// Замер и запись - разные природы, и бюджет у них тоже разный:
		// замер ходит в сеть (200 HEAD через onion - это минуты), запись
		// - пара локальных запросов на файл. Единый файловый бюджет
		// воспроизводил бы ту же ловушку этажом ниже: замер съедал бы
		// окно, и запись снова начиналась бы с мёртвым контекстом.
		// Поэтому замер живёт под measureCtx (сеть), а запись - под
		// несократимым writeCtx (300s с запасом: 200 файлов записываются
		// за секунды, потолок здесь - страховка от зависшей базы, а не
		// планировщик).
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), p.writeBudget())
		defer writeCancel()
		// Размеры уточняются перед записью: замер идёт по заголовкам и
		// ограничен по числу файлов, иначе прогон упирается в сеть.
		//
		// Обрезок замера НЕ подменяет список записи и ответа. Живой прогон
		// этапа 168 (смоук-агент F4): file_refs_found=284, а files+revisited
		// упирались ровно в потолок 200 - 84 ссылки не записывались и не
		// считались повторными, потому что возврат MeasureSizes (срез до
		// defaultSizeProbes) перезаписывал res.Crawl.FileRefs. Мутации Size
		// идут по общему массиву, поэтому полный список сохраняет замеры
		// первых URL, запись идёт по всем найденным ссылкам, и длина
		// file_refs в ответе совпадает с file_refs_found.
		//
		// Этап 169 - замер по свежести. Живой BEFORE (стенд vss169, чистый
		// каталог): окно 60s и пер-хостовая пауза дают ~35 замеров из 275,
		// а порядок был алфавитный по URL - новые ссылки не имели
		// приоритета, и записи ложились в каталог с size=0 рядом с
		// замеренными; на повторном прогоне file_refs терял размеры,
		// которые каталог знал. Локальный проход по каталогу (миллисекунды,
		// без сети) расставляет ссылки: неизвестные каталогу - вперёд,
		// известные - в хвост, уже с размером из каталога. Сетевое окно
		// тратится на то, чего каталог не знает: на повторе меряется горстка
		// нового вместо сотен повторов, а file_refs ответа несёт размеры,
		// добытые прошлыми прогонами. Ошибка локального lookup не валит
		// фазу: ссылка считается свежей и меряется как новая - худший
		// исход это лишний HEAD, не потеря.
		//
		// Этап 170 - тот же lookup батчем, а не по одной. Прежний проход
		// делал FileKnown и FileSize на каждую ссылку, а фаза записи ниже -
		// ещё один FileKnown: на живых 709 file_refs это ~2100 одиночных
		// обращений к единственному коннекту базы (SetMaxOpenConns(1)),
		// и пока файловая фаза гнала этот поток, читатель file_search мог
		// ждать своей очереди. Смоук этапа 169 зафиксировал у агента B
		// зависание file_search на 130s (повтор 38s); на стенде это не
		// воспроизводится даже под двумя параллельными discover (0.8..1.2s
		// на 1917 записях), но длина очереди к коннекту обязана падать
		// от тысяч обращений к двум: снапшот читает каталог порциями по
		// 400 URL до и после замера. Ошибка снапшота держит семантику
		// этапа 169: весь срез считается свежим и меряется как новый.
		fresh := make([]filex.Ref, 0, len(res.Crawl.FileRefs))
		stale := make([]filex.Ref, 0, len(res.Crawl.FileRefs))
		urls := make([]string, len(res.Crawl.FileRefs))
		for i, f := range res.Crawl.FileRefs {
			urls[i] = f.URL
		}
		snap, _ := p.Store.FileSnapshots(writeCtx, urls)
		for _, f := range res.Crawl.FileRefs {
			st, ok := snap[f.URL]
			if !ok || !st.Known {
				fresh = append(fresh, f)
				continue
			}
			if st.Size > 0 {
				f.Size = st.Size
			}
			stale = append(stale, f)
		}
		res.Crawl.FileRefs = append(fresh, stale...)
		// Этап 173: замер живёт под бюджетом ВЫЗОВА, а не поверх него.
		// Живой смоук этапа 172 (агент B, прогретая база) и BEFORE-стенд
		// vss173: ответ приходил на 1.7с/22.3с/26.6с позже заявленных
		// 5/30/90с, а прогрев 120с - на 61.9с: HTTP-клиент, поставивший
		// таймаут равным бюджету вызова, терял валидные ответы, и перебор
		// рос вместе с бюджетом, потому что замер (сеть, HEAD через tor,
		// до 60s) ехал под WithoutCancel после обрезки rctx. Запись -
		// локальные секунды, её несократимость - контракт этапов 167/168;
		// замер - довеска: размер не входит в «адреса пула, мета,
		// живость, файлы», size=0 в каталоге - штатное состояние. После
		// обрезки сеть для размеров не ходит вовсе, в живом бюджете
		// замер получает остаток и доезжает, сколько успел.
		measureCtx, measureCancel := context.WithTimeout(ctx, p.measureBudget())
		defer measureCancel()
		p.Crawl.MeasureSizes(measureCtx, res.Crawl.FileRefs, 0)
		// Этап 177: итог фазы замера - в ответ. До этого size=0 у file_ref
		// не имел объяснения в самом ответе (нет Content-Length? окно
		// истекло? потолок 200?). Считаем по факту: размер-bearing ссылки -
		// measured (мера или каталог), пустые - unmeasured; срез окна -
		// мерой не завершён. Отмена родителя (обрезка бюджета вызова)
		// тоже попадает в measureCtx.Err() и честно зовётся срезом.
		measured := 0
		for i := range res.Crawl.FileRefs {
			if res.Crawl.FileRefs[i].Size > 0 {
				measured++
			}
		}
		res.Crawl.FilesMeasured = measured
		res.Crawl.FilesUnmeasured = len(res.Crawl.FileRefs) - measured
		res.Crawl.MeasureCut = measureCtx.Err() != nil
		// Известность проверяется заново ПОСЛЕ замера: между снапшотами
		// прошло до минуты сети, и параллельный прогон мог записать те же
		// ссылки - свежий снапшот честно посчитает их повторными, как
		// прежний per-URL FileKnown. Ошибка второго снапшота возвращает
		// запись к per-URL пути: считать неизвестное известным нельзя,
		// это потеря заявки, а не лишний HEAD.
		snapWrite, snapErr := p.Store.FileSnapshots(writeCtx, urls)
		knownAt := func(url string) (bool, error) {
			if snapErr == nil {
				return snapWrite[url].Known, nil
			}
			return p.Store.FileKnown(writeCtx, url)
		}
		for _, f := range res.Crawl.FileRefs {
			if err := writeCtx.Err(); err != nil {
				break
			}
			// Повторная находка не пишется под свежий task_id: AddFile
			// держит происхождение первой записи, и без этой проверки
			// повторный обход отчитывался бы «файлов 51» при пустом
			// file_search по метке из ответа - та же невидимость, что у
			// новых файлов до FileTaskID, только врёт счётчиком, а не
			// молчанием. Проверка та же, что в catalog.Collect: каталоговый
			// снапшот и отдельный счётчик повторных.
			known, kerr := knownAt(f.URL)
			if kerr != nil {
				res.FailFile(kerr)
				continue
			}
			if known {
				res.Revisited++
				continue
			}
			if err := p.Store.AddFile(writeCtx, store.FileEntry{
				TaskID:     taskID,
				URL:        f.URL,
				Filename:   f.Filename,
				Ext:        f.Ext,
				Size:       f.Size,
				MIME:       f.MIME,
				SourcePage: f.SourcePage,
				// Вердикт заполняется при записи, как и в catalog.Collect:
				// колонка существовала, но оставалась пустой у всех строк,
				// поэтому фильтр по verdict не работал на настоящих данных.
				Verdict: filex.ClassifyRef(f),
			}); err != nil {
				p.logf("discover: файл %s: %v", f.URL, err)
				res.FailFile(err)
				continue
			}
			res.Files++
		}
		if res.Files > 0 {
			p.logf("discover: каталог пополнен на %d файлов", res.Files)
		}
		if res.Revisited > 0 {
			p.logf("discover: повторных находок %d, они остались под прежними task_id", res.Revisited)
		}
	}

	if res.Crawl != nil && len(res.Crawl.Titles) > 0 {
		// Titles - собранное, а не обслуживание: до этапа 168 эти записи
		// шли под rctx и при обрезке терялись молча, с счётчиком в
		// save_failed, который никто не мог привязать к задаче.
		for host, title := range res.Crawl.Titles {
			if err := p.Store.SetOnionMeta(saveCtx, host, title, "", ""); err != nil {
				p.logf("discover: мета обхода %s: %v", host, err)
				res.FailSave(err)
			}
		}
	}

	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

// savePoolOneByOne - поадресная запись пула, фолбэк батча с этапа 174.
// Счётчики и поведение перенесены из Run дословно: new - записанные,
// updated - обновившие мету, отказ - FailSave и продолжение (один адрес
// не валит остальных). Этап 174: прежний молчаливый break по мёртвому
// saveCtx удалён. Мёртвый ctx сюда приходит уже как «батч отказал», и
// каждый адрес обязан честно считаться отказом записи (FailSave), а не
// пропадать молча: иначе батч превращает мёртвый бюджет в «пул что-то
// не записал, но никто не спросит почему». Контракт filetask
// («new=0 updated=0 при мёртвом бюджете») держится глубже: UpsertOnion
// под мёртвым ctx не пишет, счётчики не растут - отказы видны, записи нет.
func (p *Pool) savePoolOneByOne(ctx context.Context, merged []Candidate, res *Result) {
	for _, c := range merged {
		known, err := p.Store.GetOnion(ctx, c.Address)
		if err == nil {
			if c.Title == known.Title && c.Category == known.Category {
				continue
			}
			if err := p.Store.SetOnionMeta(ctx, c.Address, c.Title, c.Description, c.Category); err != nil {
				p.logf("discover: мета %s: %v", c.Address, err)
				res.FailSave(err)
				continue
			}
			res.Updated++
			continue
		}
		if err := p.Store.UpsertOnion(ctx, store.Onion{
			URL:         c.Address,
			Status:      "unknown",
			Category:    c.Category,
			Title:       c.Title,
			Description: c.Description,
		}); err != nil {
			p.logf("discover: запись %s: %v", c.Address, err)
			res.FailSave(err)
			continue
		}
		res.New++
	}
}

// recordCrawl записывает в пул живость, выясненную обходом. Пауза между
// слоями обхода длиннее пробной, поэтому успех считается более сильным
// сигналом, чем результат обычной пробы: хост ответил целой страницей.
//
// Отдельно отмечается отсутствие транспорта: запрос не ушёл в сеть, вердикта о
// сервисе нет, и провал в таком случае убивал живые адреса пула на машине без
// tor (seedHosts набирает семена именно из живых, то есть портились лучшие
// записи). Таймаут, наоборот, остаётся отказом: запрос ушёл и не вернулся, это
// факт о сервисе, а не об окружении автора.
//
// Этап 178: страницы со срезом (Cut - запрос убит бюджетом вызова до
// страничного предела) живость не пишут вовсе: до этого они писали провал
// по Error!="", и fail_streak рос у хостов, которые даже не спросили до
// конца - смоук этапа 178 читал несправедливую полосу неудач после вызова,
// обрезанного по timeout.
//
// Приёмник res обязателен: возвращаемое число считает удачные записи, и без
// счётчиков отказов и пропусков ноль означал и «обход не выяснил живости», и
// «база не приняла ни одной пробы», и «тор не поднят».
func (p *Pool) recordCrawl(ctx context.Context, crep *CrawlReport, res *Result) int {
	if p.Store == nil || len(crep.PageDetail) == 0 {
		return 0
	}
	n := 0
	for _, page := range crep.PageDetail {
		if err := ctx.Err(); err != nil {
			break
		}
		host := page.Host
		if host == "" {
			continue
		}
		if page.NoTransport {
			res.SkipProbe()
			continue
		}
		if page.Cut {
			// Срез вызова - не вердикт о сервисе: ни провала, ни успеха.
			// В SkipProbe не входит: там записи без транспорта, у которых
			// окружение виновато; здесь виноват бюджет вызова.
			continue
		}
		ok := page.Error == ""
		// Этап 175: латентность - замер страницы, а не заглушка. Ровно
		// 1000ms писалось с этапа, когда Page времени не нёс, и вся
		// обходная статистика пула вырождалась в «тысячу»: SQL живой базы
		// давал latency_avg=1000 у всех live-записей подряд. Провал тоже
		// передаёт замер (время до отказа), но SQL-ветка пробы сама
		// сохраняет прежнюю латентность при неуспехе.
		latency := page.LatencyMS
		if err := p.Store.RecordProbe(ctx, host, ok, latency); err != nil {
			p.logf("discover: живость обхода %s: %v", host, err)
			res.FailProbe(err)
			continue
		}
		n++
	}
	return n
}

// seedHosts выбирает, какие адреса отправлять в обход. Порядок такой:
// живые, затем свежие (о которых пул ничего не знает), и лишь остатком
// потолка - подтверждённо мёртвые. Возвращает сиды и число кандидатов,
// отрезанных потолком (второе - в crawl.seeds_cut с этапа 174: срез входа
// обязан быть виден, смоук этапа 172 читал «обошли 3 из 14217» без ответа,
// куда делись остальные).
//
// Обход тратит на каждый хост запрос с таймаутом и паузу, поэтому попытка
// на заведомо мёртвый адрес - это потерянный бюджет: такой адрес не ответит
// и в этот раз. Свежий, наоборот, стоит попытки - именно он может стать
// новым живым сервисом.
func (p *Pool) seedHosts(ctx context.Context, cands []Candidate, maxHosts int) ([]string, int) {
	limit := maxHosts
	if limit <= 0 {
		limit = 50
	}

	seen := map[string]bool{}
	out := make([]string, 0, limit)
	cut := 0

	// Живые из пула идут первыми: они уже подтвердили, что отвечают.
	if p.Store != nil {
		if live, err := Known(ctx, p.Store, "live", limit); err == nil {
			for _, h := range live {
				if !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
			}
		} else {
			p.logf("discover: живые для обхода: %v", err)
		}
	}

	// Свежие адреса отделяются от мёртвых по статусу в пуле.
	fresh := make([]string, 0, len(cands))
	dead := make([]string, 0, len(cands))
	statuses := map[string]string{}
	if p.Store != nil && len(cands) > 0 {
		urls := make([]string, 0, len(cands))
		for _, c := range cands {
			urls = append(urls, c.Address)
		}
		st, err := p.Store.KnownStatuses(ctx, urls)
		if err != nil {
			p.logf("discover: статусы для обхода: %v", err)
		} else {
			statuses = st
		}
	}

	// Этап 174: дедуп кандидатов до подсчёта. Источники отдают один и
	// тот же адрес по многу раз (живой прогон: 8 источников, каждый со
	// своей копией страницы), и без отсеивания дублей SeedsCut считал бы
	// копии, а не кандидатов: 5 адресов от 8 источников при потолке 2
	// отчитали бы «отрезано 38» вместо честных 3. Живые из пула сюда не
	// попадают - они уже сиды.
	dupe := make(map[string]bool, len(cands))
	for _, c := range cands {
		if dupe[c.Address] || seen[c.Address] {
			continue
		}
		dupe[c.Address] = true
		switch statuses[c.Address] {
		case "live":
			// Уже добавлен выше; повтор не нужен.
		case "dead":
			dead = append(dead, c.Address)
		default:
			fresh = append(fresh, c.Address)
		}
	}

	// Этап 174: циклы не возвращаются досрочно, а считают отрезанное. Прежний
	// «if len(out) >= limit { return out }» молча выбрасывал хвост списка, и
	// срез входа не был виден нигде: ни в отчёте, ни в логе. Число кандидатов,
	// не ставших сидами, уходит вторым возвращаемым значением в SeedsCut.
	for _, h := range fresh {
		if seen[h] {
			continue
		}
		if len(out) >= limit {
			cut++
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range dead {
		if seen[h] {
			continue
		}
		if len(out) >= limit {
			cut++
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out, cut
}

// Known возвращает адреса из пула, готовые к пробе живости. Фильтр по
// статусу позволяет взять только неизвестные (свежие) или только живые.
//
// Порядок сохраняется таким, каким его отдал store: сначала живые и быстрые.
// Раньше здесь стояла сортировка по алфавиту, и она убивала это
// ранжирование - обход брал адреса вперемешку с мёртвыми и тратил на них
// половину потолка хостов.
func Known(ctx context.Context, st *store.Store, status string, limit int) ([]string, error) {
	if st == nil {
		return nil, fmt.Errorf("store не задан")
	}
	onions, err := st.ListOnions(ctx, status, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(onions))
	for _, o := range onions {
		out = append(out, o.URL)
	}
	return out, nil
}

// NormalizeBase приводит адрес пула к базовому URL, пригодному как Base
// для движка поиска: схема обязательна, хвостовой слэш не нужен.
// Реализация живёт в filex, потому что та же нормализация нужна всем, кто
// берёт адрес из базы и идёт в сеть: promote, collect, crawl. Держать копию
// здесь значит однажды развести два поведения.
func NormalizeBase(baseURL string) string { return filex.NormalizeBase(baseURL) }

func (p *Pool) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Infof(format, args...)
	}
}
