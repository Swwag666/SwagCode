package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/store"
)

// warnSnapshotOnlyDataDir предупреждает о запуске с каталогом данных, в котором
// нет рабочей базы, но лежат её снимки.
//
// До правки такой запуск выглядел полностью здоровым: база создавалась заново,
// миграции проходили, и каждая команда рапортовала нули как норму. Измерено на
// stats при двух снимках и отсутствующей базе: rc=0, 176 байт «пул: 0 всего /
// 0 живых ...», stderr пуст, рядом появилась пустая база на 159744 байта.
// Оператор, который переносил дату или чистил место, получал «восстановленный»
// сервис без единого признака потери данных.
//
// Снимки ищутся и в самом каталоге данных, и в каталоге бэкапов: штатно их
// кладёт в backups/ Config.BackupDirOrDefault, а при ручном переносе даты
// операторы сваливают всё в один каталог. Код возврата не меняется: пустая база
// остаётся рабочим состоянием первого запуска, а решение о восстановлении
// принимает человек.
func warnSnapshotOnlyDataDir(cmd string) {
	switch cmd {
	case "version", "--version", "-v", "help", "--help", "-h":
		// Команды, которые базу не открывают: предупреждение было бы шумом.
		return
	}
	cfg, err := config.Load()
	if err != nil {
		// Про конфиг команда расскажет сама и внятным отказом.
		return
	}
	db := cfg.DBPath()
	if _, err := os.Stat(db); err == nil {
		return
	}
	snaps := listSnapshots(filepath.Dir(db), cfg.BackupDirOrDefault())
	if len(snaps) == 0 {
		return
	}
	// Порядок хронологический: в имени снимка дата и время, поэтому сортировка
	// по имени файла совпадает с порядком создания.
	sort.Slice(snaps, func(i, j int) bool {
		return filepath.Base(snaps[i]) < filepath.Base(snaps[j])
	})
	last := snaps[len(snaps)-1]
	fmt.Fprintf(os.Stderr, "внимание: базы %s нет, но есть %d снимков, последний %s\n", db, len(snaps), filepath.Base(last))
	fmt.Fprintf(os.Stderr, "         сейчас заведётся пустая база, и команды будут рапортовать нули\n")
	fmt.Fprintf(os.Stderr, "         восстановление: voidsearchswag restore --yes возьмёт %s\n", filepath.Base(last))
	fmt.Fprintf(os.Stderr, "         вручную: скопируйте %s в %s и удалите рядом -wal и -shm от прежней базы\n", last, db)
}

// listSnapshots собирает снимки базы из перечисленных каталогов, пропуская
// повторы. Маска имени та же, что у store.BackupRotate и store.PruneBackups:
// префикс voidsearchswag- и суффикс .db, поэтому в выборку не попадает ни рабочая
// база, ни чужие файлы.
func listSnapshots(dirs ...string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if strings.HasPrefix(n, "voidsearchswag-") && strings.HasSuffix(n, ".db") {
				out = append(out, filepath.Join(dir, n))
			}
		}
	}
	return out
}

func cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	keep := fs.Int("keep", 0, "сколько снимков держать, от 1 до 365 (0 - значение из конфига)")
	dir := fs.String("dir", "", "каталог снимков (пусто = из конфига)")
	prune := fs.Bool("prune", false, "подтвердить удаление нескольких снимков при ротации")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)

	// Потолок флага равен тому, что config.Validate держит для BackupKeep: обе
	// стороны описывают одну величину, и расхождение между ними означало, что
	// значение из конфига законно, а то же значение из флага - нет.
	keepN := 0
	if *keep != 0 {
		keepN = validLimitCeiling("keep", *keep, maxFlagBackupKeep)
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *dir != "" {
		cfg.BackupDir = *dir
	}
	// Ноль означает «значение из конфига», а не «семь». Дефолт флага семь
	// совпадал с envDefault поля BackupKeep, поэтому переменная окружения
	// VOIDSEARCH_BACKUP_KEEP не действовала вовсе, а неположительное значение
	// молча превращалось в семь. Замер до правки на HEAD 2bf9853, копия боевой
	// базы, каталог снимков во временной папке:
	//
	//	VOIDSEARCH_BACKUP_KEEP=3 backup --json --dir ...
	//	    rc=0, keep=7, pruned=0
	//	backup --json --dir ... --keep 0
	//	    rc=0, keep=7
	//	backup --json --dir ... --keep 365
	//	    rc=0, keep=30
	//	backup --json --dir ... --keep -4
	//	    rc=0, keep=7
	//	backup -h
	//	    -keep int
	//	        сколько снимков держать (default 7)
	//
	// Потолок в тридцать снимков стоял только во флаге: config.Validate держит для
	// BackupKeep диапазон от одного до трёхсот шестидесяти пяти.
	keepN = flagOrConfig(keepN, cfg.BackupKeep)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}
	snapDir := cfg.BackupDirOrDefault()
	// Штатная ротация убирает ровно один самый старый снимок: каждый вызов
	// добавляет один новый. Всё, что стирает историю пачкой, требует явного
	// подтверждения - до правки backup --keep 1 на каталоге из четырёх снимков
	// возвращал rc=0 и ответ {"keep":1,"snapshot":...} без единого слова о трёх
	// удалённых, а тот же вызов доступен агенту через MCP.
	planned, err := store.PlannedPrune(snapDir, keepN)
	if err != nil {
		fatalf("снимки: %v", err)
	}
	if planned > 1 && !*prune {
		fatalf("ротация удалит %d снимков из %s (их там %d, keep=%d): повторите с --prune, если это намеренно",
			planned, snapDir, planned+keepN-1, keepN)
	}
	dest, err := st.BackupRotate(ctx, snapDir, keepN)
	if err != nil {
		fatalf("бэкап: %v", err)
	}
	if *jsonOut {
		writeJSON(map[string]any{"snapshot": dest, "keep": keepN, "pruned": planned})
		return
	}
	fmt.Printf("снимок: %s (держим %d)\n", dest, keepN)
	if planned > 0 {
		fmt.Printf("ротация удалила старых снимков: %d\n", planned)
	}
}
