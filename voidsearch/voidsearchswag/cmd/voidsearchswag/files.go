package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/filex"
	"voidsearchswag/internal/store"
)

func cmdFiles(args []string) {
	fs := flag.NewFlagSet("files", flag.ExitOnError)
	query := fs.String("query", "", "подстрока в имени, адресе файла или странице, где он найден")
	var ext listFlag
	fs.Var(&ext, "ext", "расширения через запятую: epub,pdf или zip sql; флаг можно повторять")
	var minSize, maxSize sizeFlag
	fs.Var(&minSize, "min-size", "минимальный размер: 1024, 1MB, 1.5MiB, 2G")
	fs.Var(&maxSize, "max-size", "максимальный размер: 1024, 1MB, 1.5MiB, 2G")
	unknownSize := fs.Bool("unknown-size", false,
		"только файлы неизвестного размера: под -min-size/-max-size они не попадают")
	taskID := fs.String("task", "", "отобрать по задаче сбора")
	var verdict listFlag
	fs.Var(&verdict, "verdict",
		"метки категории через запятую: ebook,document или exe,archive; флаг можно повторять "+
			"(доступны: "+strings.Join(filex.AllVerdicts(), ", ")+")")
	riskOnly := fs.Bool("risk", false, "только исполняемый код и ключи")
	limit := fs.Int("limit", 50, "сколько записей показать")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)
	// Потолок берётся из хранилища, а не дублируется числом: store.SearchFiles
	// всё равно не вернёт больше MaxFileSearchLimit, и если предел там сдвинут,
	// проверка здесь сдвинется вместе с ним.
	limitN := validLimitCeiling("limit", *limit, store.MaxFileSearchLimit)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}

	total, bytes, byExt, err := st.FileStats(ctx)
	if err != nil {
		fatalf("статистика: %v", err)
	}
	if total == 0 && !*jsonOut {
		// Подсказка адресована человеку, поэтому в машинном режиме её нет: там
		// пустой каталог отдаёт ту же схему, что и непустой, с returned 0 при
		// catalog_total 0. До правки строка печаталась раньше проверки флага, и
		// files --json на свежей базе выдавал rc=0 и 132 байта русского текста
		// вместо JSON - разбор потока падал на первой строке.
		fmt.Println("каталог пуст: соберите файлы через discover --crawl или collect")
		return
	}

	files, err := st.SearchFiles(ctx, store.FileQuery{
		Text:        *query,
		Ext:         ext.value(),
		Verdict:     verdict.value(),
		RiskOnly:    *riskOnly,
		MinSize:     minSize.val,
		MaxSize:     maxSize.val,
		UnknownSize: *unknownSize,
		TaskID:      *taskID,
		Limit:       limitN,
	})
	if err != nil {
		fatalf("поиск: %v", err)
	}

	if *jsonOut {
		out := map[string]any{
			"returned":      len(files),
			"limit":         limitN,
			"catalog_total": total,
			"catalog_bytes": bytes,
			"by_ext":        byExt,
			"files":         files,
		}
		// Тот же честный исход, что и в текстовом режиме ниже: машинный
		// потребитель тоже имеет право знать, почему фильтр пуст.
		if (minSize.val > 0 || maxSize.val > 0) && len(files) == 0 && !*unknownSize {
			if n, err := st.FileUnknownCount(ctx); err == nil && n > 0 {
				out["unknown_size_files"] = n
				out["note"] = fmt.Sprintf(
					"размерный фильтр не вернул ничего, но в каталоге %d файлов неизвестного размера: unknown_size достанет их", n)
			}
		}
		if *unknownSize {
			out["unknown_size_only"] = true
		}
		writeJSON(out)
		return
	}

	fmt.Printf("каталог: %d файлов, %s\n", total, humanBytes(bytes))
	if len(byExt) > 0 {
		exts := make([]string, 0, len(byExt))
		for e, n := range byExt {
			exts = append(exts, fmt.Sprintf("%s=%d", e, n))
		}
		sort.Strings(exts)
		fmt.Printf("по расширениям: %s\n", strings.Join(exts, " "))
	}
	if len(files) == 0 {
		fmt.Println("по фильтру ничего не найдено")
		// Строки, записанные до появления вердикта, имеют пустую колонку и не
		// находятся ни по одной метке. Без объяснения «ничего не найдено»
		// неотличимо от «таких файлов нет», хотя каталог наглядно содержит и
		// epub, и pdf.
		if verdict.value() != "" || *riskOnly {
			if n, err := st.CountFilesWithoutVerdict(ctx); err == nil && n > 0 {
				fmt.Printf("в каталоге %d строк без вердикта: они записаны до "+
					"появления метки и не находятся этим фильтром\n", n)
				fmt.Println("заполните их командой: voidsearchswag clean --apply")
			}
		}
		// Тот же честный исход для размерных фильтров: неизвестный размер
		// под -min-size/-max-size не попадает по смыслу, но молчание об этом
		// оставляет человека с пустотой и без пути (жалоба смоук-агента
		// этапа 166: семь файлов с size=0, -max-size 100 - ноль записей).
		if (minSize.val > 0 || maxSize.val > 0) && !*unknownSize {
			if n, err := st.FileUnknownCount(ctx); err == nil && n > 0 {
				fmt.Printf("в каталоге %d файлов неизвестного размера: размерные "+
					"фильтры их не показывают\n", n)
				fmt.Println("показать их: voidsearchswag files --unknown-size")
			}
		}
		return
	}
	fmt.Printf("показано %d:\n\n", len(files))
	for _, f := range files {
		size := humanBytes(f.Size)
		if f.Size == 0 {
			size = "размер неизвестен"
		}
		// Вердикт печатается рядом с размером, а не отдельной строкой: это
		// короткая метка категории, и выносить её на свою строку значило бы
		// растянуть список вдвое.
		line := fmt.Sprintf("  %s  [%s", f.Filename, size)
		if f.Verdict != "" {
			line += ", " + f.Verdict
		}
		fmt.Printf("%s]\n    %s\n", line, f.URL)
		// Предупреждение о риске выводится отдельно и заметно: файл из
		// даркнета, который запускается как код, пользователь обязан увидеть до
		// того как откроет его.
		if filex.VerdictIsRisk(f.Verdict) {
			fmt.Printf("    внимание: %s\n", riskNote(f.Verdict))
		}
		if f.SourcePage != "" {
			fmt.Printf("    страница: %s\n", f.SourcePage)
		}
	}
}

// riskNote объясняет метку риска словами, а не повторяет её текст. Вход
// нормализуется тем же правилом, что и решение печатать предупреждение: иначе
// метка в другом регистре распознаётся как риск, но объяснение уходит в ветку по
// умолчанию и теряет смысл.
func riskNote(verdict string) string {
	switch filex.NormalizeVerdict(verdict) {
	case filex.VerdictExecutable:
		return "исполняемый код, запускать только понимая происхождение файла"
	case filex.VerdictSecret:
		return "ключи или база паролей, файл может содержать чужие секреты"
	default:
		return "файл требует осторожности"
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
