package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/store"
)

// cmdRestore возвращает рабочую базу из снимка.
//
// До этой команды восстановление было ручным: предупреждение о каталоге со
// снимками и без базы советовало скопировать файл и удалить рядом -wal и -shm,
// то есть требовало от оператора помнить две неочевидные вещи именно тогда, когда
// данные уже потеряны. Здесь обе делает команда.
//
// Порядок действий принципиален. Сначала снимок проверяется на целостность:
// восстановление из побитой копии хуже отсутствия восстановления, потому что
// стирает и ту базу, которая ещё подавала признаки жизни. Затем текущая база
// сохраняется под именем, которое маска снимков не ловит, поэтому ротация и
// поиск снимков не примут её за снимок. Только после этого убираются база и её
// журнал и на их место копируется снимок. Последним шагом восстановленная база
// открывается, прогоняются миграции и снимаются счётчики: оператор видит не
// «готово», а то, что именно вернулось.
//
// Подтверждение обязательно: без --yes команда печатает план и выходит с кодом 1,
// ничего не меняя. Восстановление перезаписывает рабочую базу, и делать это
// побочным эффектом любого вызова нельзя.
func cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	snapshot := fs.String("snapshot", "latest", "снимок: имя файла, путь или latest")
	dir := fs.String("dir", "", "каталог снимков (пусто = каталог базы и каталог из конфига)")
	yes := fs.Bool("yes", false, "подтвердить замену рабочей базы снимком")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if strings.TrimSpace(*dir) != "" {
		cfg.BackupDir = *dir
	}
	dbPath := cfg.DBPath()
	snaps := listSnapshots(filepath.Dir(dbPath), cfg.BackupDirOrDefault())
	if len(snaps) == 0 {
		fatalf("снимков нет: проверил %s и %s", filepath.Dir(dbPath), cfg.BackupDirOrDefault())
	}
	sort.Slice(snaps, func(i, j int) bool {
		return filepath.Base(snaps[i]) < filepath.Base(snaps[j])
	})
	chosen, err := pickSnapshot(snaps, *snapshot)
	if err != nil {
		fatalf("%v", err)
	}
	// Снимок не обязан быть рабочей базой: восстановление файла сам в себя
	// означало бы удалить базу и положить её же обрывок на место.
	if samePath(chosen, dbPath) {
		fatalf("снимок %s и есть рабочая база", chosen)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	state, err := store.CheckIntegrity(ctx, chosen)
	if err != nil {
		fatalf("снимок %s не читается как база: %v", filepath.Base(chosen), err)
	}
	if !strings.EqualFold(state, "ok") {
		fatalf("снимок %s повреждён: integrity_check = %s", filepath.Base(chosen), clipText(state, 300))
	}

	savedAs := ""
	hadDB := false
	if _, err := os.Stat(dbPath); err == nil {
		hadDB = true
		// Имя намеренно не совпадает с маской снимка: pre-restore-... не начинается
		// с voidsearchswag-, поэтому ни ротация, ни поиск снимков, ни предупреждение
		// о каталоге без базы не примут сохранённую прежнюю базу за снимок.
		savedAs = filepath.Join(filepath.Dir(dbPath),
			"pre-restore-"+time.Now().Format("20060102-150405.000")+".db")
	}
	if !*yes {
		note := "рабочей базы сейчас нет"
		if hadDB {
			note = "текущая база будет сохранена как " + filepath.Base(savedAs)
		}
		fatalf("база %s будет заменена снимком %s (%s): повторите с --yes, если это намеренно",
			dbPath, filepath.Base(chosen), note)
	}
	if hadDB {
		if err := copyFileContents(dbPath, savedAs); err != nil {
			fatalf("сохранить текущую базу: %v", err)
		}
	}
	// Журнал прежней базы убирается вместе с ней. Замера, что оставшийся -wal
	// ломает подложенный снимок, у меня нет: SQLite сбрасывает журнал с
	// несовпавшим salt. Оставлять рядом с восстановленной базой файлы от другой
	// базы всё равно не дело: они путают и ручной разбор, и поиск снимков.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			fatalf("удаление %s: %v", dbPath+suffix, err)
		}
	}
	if err := copyFileContents(chosen, dbPath); err != nil {
		fatalf("копирование снимка: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		fatalf("восстановленная база не открывается: %v", err)
	}
	defer AtExit("база", st.Close)()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции восстановленной базы: %v", err)
	}
	out, problems := statsData(ctx, st)
	report := map[string]any{
		"snapshot":    chosen,
		"restored_to": dbPath,
		"integrity":   state,
		"onion_total": out.OnionTotal,
		"onion_live":  out.OnionLive,
		"files_total": out.FilesTotal,
		"hunts_total": out.HuntsTotal,
	}
	if savedAs != "" {
		report["previous_saved_as"] = savedAs
	}
	if len(problems) > 0 {
		report["errors"] = problems
	}
	if *jsonOut {
		writeJSON(report)
	} else {
		fmt.Printf("восстановлено из %s в %s\n", filepath.Base(chosen), dbPath)
		if savedAs != "" {
			fmt.Printf("прежняя база сохранена как %s\n", savedAs)
		}
		fmt.Printf("целостность: %s\n", state)
		fmt.Printf("пул: %d всего / %d живых  файлы: %d  охоты: %d\n",
			out.OnionTotal, out.OnionLive, out.FilesTotal, out.HuntsTotal)
	}
	// Непрочитанный источник в восстановленной базе меняет код возврата: рапорт
	// «восстановлено» при нечитающихся таблицах был бы тем же молчанием, из-за
	// которого потеря данных вообще стала возможной. В машинном режиме причины
	// уходят полем errors в stdout, а не в stderr: поток обязан оставаться
	// разбираемым.
	if len(problems) > 0 {
		if !*jsonOut {
			for _, p := range problems {
				fmt.Fprintf(os.Stderr, "ERROR: restore: %s\n", p)
			}
		}
		RunShutdown()
		os.Exit(1)
	}
}

// pickSnapshot выбирает снимок по аргументу --snapshot. Пустое значение и latest
// дают последний по имени, то есть самый свежий: порядок имён хронологический.
// Путь проверяется на существование, имя файла ищется среди найденных снимков,
// чтобы опечатка давала отказ со списком доступного, а не восстановление не того.
func pickSnapshot(snaps []string, want string) (string, error) {
	w := strings.TrimSpace(want)
	if w == "" || strings.EqualFold(w, "latest") {
		return snaps[len(snaps)-1], nil
	}
	if filepath.IsAbs(w) || strings.ContainsAny(w, `/\`) {
		fi, err := os.Stat(w)
		if err != nil {
			return "", fmt.Errorf("снимок %s: %w", w, err)
		}
		if fi.IsDir() {
			return "", fmt.Errorf("снимок %s - каталог", w)
		}
		return w, nil
	}
	for _, s := range snaps {
		if strings.EqualFold(filepath.Base(s), w) {
			return s, nil
		}
	}
	return "", fmt.Errorf("снимок %q не найден среди %d доступных, последний %s",
		w, len(snaps), filepath.Base(snaps[len(snaps)-1]))
}

// copyFileContents копирует файл через временный и переименовывает его на место.
// Переименование вместо прямой записи в назначение оставляет старый файл целым,
// если копия оборвалась на середине: наполовину записанная база хуже прежней,
// потому что прежнюю уже не прочитать.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
