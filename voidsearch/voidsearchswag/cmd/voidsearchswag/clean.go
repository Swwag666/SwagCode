package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/store"
)

// cmdClean вычищает из базы мусор, который накопился до введения фильтров.
//
// Обе чистки - CleanFileCatalog и CleanOnionTitles - раньше вызывались
// только внутри discover.Run. Это означало, что единственный способ привести
// существующий каталог в порядок - прогнать полноценную разведку: поднять tor,
// дождаться bootstrap и обойти источники. Для операции, которая трогает только
// локальную базу и не требует сети, цена несоразмерная, и на практике каталог
// просто не чистили: записи, попавшие туда до введения фильтра, оставались
// навсегда.
//
// Команда локальная, tor не поднимает и по умолчанию ничего не удаляет: без
// --apply она показывает, что было бы удалено. Это важно, потому что чистка
// необратима, а список служебных имён расширяется - возможность посмотреть
// результат до удаления защищает от потери настоящих файлов.
func cmdClean(args []string) {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	apply := fs.Bool("apply", false, "действительно удалить (без флага - только показать)")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)
	warnStrayFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()

	ctx, cancel := context.WithTimeout(context.Background(), validTimeout("timeout", 5*time.Minute))
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}

	// Число строк до чистки берётся точным счётчиком, а не длиной выборки.
	//
	// Прежняя версия читала st.ListFiles(ctx, "", 10000) и строила отчёт на len
	// от него. ListFiles обрезан внутренним пределом store (MaxStoreLimit = 5000)
	// и сортирует по found_at DESC, поэтому на каталоге больше 5000 строк
	// знаменатель «из N» врал, часть старого мусора не попадала в предпросмотр
	// вовсе, а остаток files_before - files_removed в JSON уходил в минус:
	// CleanFileCatalog удаляет по всей таблице, а вычитали из него обрезанную
	// выборку. Измерено на каталоге в 6000 строк, где 5500 были мусором:
	// предпросмотр печатал «файлов к удалению 4500 из 5000», JSON с --apply давал
	// files_before 5000 и files_after -500, хотя фактически в базе осталось 500
	// записей. Рядом тот же отчёт показывал «ещё 6000 строк без вердикта» -
	// точным COUNT, то есть в одном выводе одно число было настоящим, а другое
	// обрезанным.
	filesBefore, err := st.CountFiles(ctx)
	if err != nil {
		fatalf("каталог: %v", err)
	}

	var filesRemoved, titlesRemoved int64
	// Причины удаления собираются и в пробном прогоне, и в настоящем: цифра
	// «удалено 15» сама по себе не отвечает на вопрос, не потерялись ли
	// настоящие файлы. Причина по каждой записи делает результат проверяемым.
	type doomed struct {
		name   string
		url    string
		reason string
	}
	// Столько кандидатов печатается в человекочитаемом отчёте. Список на
	// несколько тысяч строк вытесняет с экрана само число и причину, а полный
	// состав отдаёт --json.
	const printLimit = 50

	var junk []doomed
	var verdictFilled int
	if *apply {
		if filesRemoved, err = st.CleanFileCatalog(ctx); err != nil {
			fatalf("чистка каталога: %v", err)
		}
		if titlesRemoved, err = st.CleanOnionTitles(ctx); err != nil {
			fatalf("чистка заголовков: %v", err)
		}
		// Заполнение вердиктов задним числом идёт после чистки: удалять мусор и
		// потом проставлять метки оставшимся дешевле, чем наоборот.
		//
		// Вердикт выводится из расширения и имени без сети, поэтому операция
		// дёшева и детерминирована, а повторный вызов просто вернёт ноль.
		if verdictFilled, err = st.BackfillFileVerdicts(ctx); err != nil {
			fatalf("заполнение вердиктов: %v", err)
		}
	} else {
		// Пробный прогон использует тот же filex.CatalogJunkReason, что и
		// store.CleanFileCatalog, поэтому предпросмотр показывает ровно то, что
		// удалит настоящая чистка. Две независимые реализации критериев
		// разошлись бы, и предпросмотр начал бы врать.
		//
		// Обход идёт через ScanFileCatalog, а не ListFiles: предпросмотр обязан
		// видеть весь каталог, иначе он показывает часть мусора и усыпляет
		// бдительность перед --apply.
		limit := printLimit
		if *jsonOut {
			// JSON читают машины, поэтому список кандидатов отдаётся целиком,
			// а его длина остаётся точным числом: отдельное поле схемы для
			// этого не нужно.
			limit = -1
		}
		err = st.ScanFileCatalog(ctx, 0, func(f store.FileEntry) error {
			reason := filex.CatalogJunkReason(f.Ext, f.Filename, f.URL)
			if reason == "" {
				return nil
			}
			filesRemoved++
			if limit < 0 || len(junk) < limit {
				junk = append(junk, doomed{f.Filename, f.URL, reason})
			}
			return nil
		})
		if err != nil {
			fatalf("предпросмотр каталога: %v", err)
		}
	}

	// Остаток каталога и признак того, чем он является - фактическим счётчиком
	// или прогнозом. Подробность в cleanRemainder.
	filesAfter, filesAfterExact, problems := cleanRemainder(ctx, st, *apply, filesBefore, filesRemoved)

	if *jsonOut {
		list := make([]map[string]string, 0, len(junk))
		for _, j := range junk {
			list = append(list, map[string]string{
				"filename": j.name, "url": j.url, "reason": j.reason,
			})
		}
		out := map[string]any{
			"applied":           *apply,
			"files_removed":     filesRemoved,
			"titles_removed":    titlesRemoved,
			"verdict_filled":    verdictFilled,
			"files_before":      filesBefore,
			"files_after":       filesAfter,
			"files_after_exact": filesAfterExact,
			"candidates":        list,
		}
		// Причина неполноты уходит в поле problems и не дублируется в stderr:
		// машинный вывод обязан остаться разбираемым, как в stats. Сигналом
		// неполноты служит ненулевой код возврата.
		if len(problems) > 0 {
			out["problems"] = problems
		}
		writeJSON(out)
		if len(problems) > 0 {
			RunShutdown()
			os.Exit(1)
		}
		return
	}

	if !*apply {
		fmt.Printf("пробный прогон (ничего не удалено): файлов к удалению %d из %d\n",
			filesRemoved, filesBefore)
		for _, j := range junk {
			fmt.Printf("  %-26s %s\n      %s\n", j.name, j.reason, j.url)
		}
		if rest := filesRemoved - int64(len(junk)); rest > 0 {
			fmt.Printf("  ...и ещё %d: показаны первые %d, полный список отдаёт --json\n",
				rest, len(junk))
		}
		// В пробном прогоне показываем и сколько строк ждёт вердикта: иначе
		// пользователь узнает об этом только когда фильтр -verdict вернёт пусто.
		if n, err := st.CountFilesWithoutVerdict(ctx); err == nil && n > 0 {
			fmt.Printf("  ещё %d строк без вердикта: будут заполнены при --apply\n", n)
		}
		if filesRemoved > 0 {
			fmt.Println("повторите с --apply, чтобы удалить")
		}
		return
	}
	fmt.Printf("вычищено: файлов %d, мусорных заголовков %d\n", filesRemoved, titlesRemoved)
	if verdictFilled > 0 {
		fmt.Printf("заполнено вердиктов: %d\n", verdictFilled)
	}
	// Остаток каталога в тексте не печатался вовсе, хотя именно по нему
	// проверяют, что --apply не снёс лишнего. Ветка достижима только после
	// --apply: пробный прогон выходит выше.
	if filesAfterExact {
		fmt.Printf("остаток каталога: %d (фактический подсчёт)\n", filesAfter)
	} else {
		fmt.Printf("остаток каталога: %d (прогноз вычитанием, точный подсчёт не удался)\n", filesAfter)
	}
	// Непрочитанный счётчик виден и в тексте: число с пометкой «прогноз»
	// отличается от факта, но причину неполноты даёт только эта строка.
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "ERROR: clean: %s\n", p)
	}
	if len(problems) > 0 {
		// os.Exit не выполняет defer, поэтому стек очистки опустошается явно,
		// как в fatalf: иначе база осталась бы с несброшенным WAL.
		RunShutdown()
		os.Exit(1)
	}
}

// cleanRemainder считает остаток каталога для отчёта clean и говорит, чем
// полученное число является.
//
// В пробном прогоне удаление не выполнялось, поэтому остаток - прогноз: сколько
// строк останется, если применить чистку. Физическую неизменность каталога
// гарантирует отсутствие вызова CleanFileCatalog. Отрицательным прогноз способен
// стать, только если между подсчётом и обходом в каталог добавили строки,
// поэтому значение прижимается к нулю.
//
// После --apply прогноз обязан заменяться фактическим COUNT(*): точное число не
// может разойтись с тем, что реально лежит в базе, а вычитание расходится
// всякий раз, когда удаление части строк не удалось. Раньше ошибка счётчика
// оставляла в отчёте прогноз без единой пометки - код шёл против собственного
// комментария, который обещал фактический счётчик, и потребитель JSON получал
// приблизительное значение под видом точного.
//
// Возвращает остаток, признак фактического подсчёта и список проблем.
func cleanRemainder(ctx context.Context, st *store.Store, apply bool, filesBefore int, filesRemoved int64) (int, bool, []string) {
	forecast := filesBefore - int(filesRemoved)
	if forecast < 0 {
		forecast = 0
	}
	if !apply {
		return forecast, false, nil
	}
	n, err := st.CountFiles(ctx)
	if err != nil {
		return forecast, false, []string{fmt.Sprintf("остаток каталога не прочитан: %v", err)}
	}
	return n, true, nil
}
