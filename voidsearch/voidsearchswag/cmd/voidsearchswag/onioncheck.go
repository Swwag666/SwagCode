package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/store"
)

func cmdOnioncheck(args []string) {
	fs := flag.NewFlagSet("onioncheck", flag.ExitOnError)
	timeout := fs.Duration("timeout", 90*time.Second, "общий таймаут пинга")
	noTor := fs.Bool("no-tor", false, "не поднимать tor (пинг не сработает)")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *noTor {
		cfg.Tor = "off"
	}

	log := stderrLogger{}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()
	if err := st.Migrate(context.Background()); err != nil {
		fatalf("миграции: %v", err)
	}

	engine, cleanup := buildEngine(cfg, log, st)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	ctx, cancel := context.WithTimeout(context.Background(), validTimeout("timeout", *timeout))
	defer cancel()

	live, total, skipped := engine.ProbeOnion(ctx)
	// Пропуск не равен смерти: без tor onion-адрес недостижим, и прежняя строка
	// «живых 0 из 7» выглядела как массовый отказ движков, хотя не состоялась
	// сама проверка. Состояние движков при этом не меняется.
	fmt.Println(onioncheckHeadline(live, total, skipped))
	if total > 0 && skipped >= total {
		// Таблица ниже печатает живость, латентность и долю успехов из пула
		// здоровья, то есть из прошлых волн. Без оговорки «tornet жив, успех 93%,
		// проб 29» читалось как результат этого прогона, в котором не состоялось
		// ни одной пробы.
		fmt.Println("  состояние ниже - по прошлым замерам, в этой волне не проверялось")
	}
	fmt.Println()

	// Состояние tor печатается проверенным, а не по факту наличия хендла.
	//
	// Команда диагностическая, и именно здесь пользователь видел прямое
	// противоречие: «control=подключён» при шести подряд таймаутах NEWNYM.
	// Прежняя строка утверждала подключение по ненулевому хендлу, тогда как
	// пригодность канала определяется только круговым запросом - дедлайн на
	// сокете в Go абсолютный, поэтому однажды истёкшее соединение навсегда
	// отвечает i/o timeout, оставаясь ненулевым.
	//
	// Без этой строки onioncheck показывал здоровье движков, но не показывал
	// состояние транспорта, от которого это здоровье зависит: мёртвый control
	// выглядел как «движки отвалились», хотя причина была в tor.
	if engine.Client != nil {
		if r := engine.Client.Rotator(); r != nil {
			fmt.Printf("tor: %s\n", torStatusLine(r))
			fmt.Println()
		}
	}

	for _, e := range engine.HealthReport() {
		state := "мёртв"
		if e.Live && !e.Disabled {
			state = "жив"
		} else if e.Disabled {
			state = "отключён"
		}
		// Имя укорачивается для вывода, а колонка рассчитана на укороченное имя:
		// «onion-» плюс 8 символов префикса дают 14, поэтому ширина 16. Полное
		// v3 onion-имя в 56 символов ломало бы выравнивание всей таблицы, а
		// прежнее %-12s съезжало на два символа даже после укорачивания.
		fmt.Printf("  %-16s %-9s %6dms  успех %.0f%%  проб %d\n",
			displayEngineName(e.Name), state, e.LatencyAvg, e.SuccessRate*100, e.Probes)
		if e.LastError != "" {
			// Отступ совпадает с началом колонки состояния: 2 пробела ведущих
			// плюс 16 ширины имени плюс 1 разделитель.
			fmt.Printf("                   ошибка: %s\n", e.LastError)
		}
	}

	// Отчёт печатается до выхода: код возврата добавляет сигнал для обвязки, а не
	// заменяет объяснение. RunShutdown закрывает базу и tor, потому что os.Exit не
	// выполняет defer.
	if code := onioncheckExitCode(total, skipped); code != 0 {
		RunShutdown()
		os.Exit(code)
	}
}

// onioncheckExitCode решает, состоялась ли проверка onion-движков. Ноль - хотя бы
// один движок спрошен либо проверять было нечего. Единица - вся волна пропущена:
// нет ни tor, ни прокси, и «живых 3 из 7 по прошлым замерам» означает, что
// состояние пула неизвестно, а не что три движка живы сейчас. Мёртвые движки при
// состоявшемся пинге остаются нулём: диагностика затем и запускается, чтобы их
// находить. Правило общее с probe и promote, поэтому все три команды возвращают
// единицу по одному признаку - ни одна проверка не состоялась.
func onioncheckExitCode(total, skipped int) int {
	if total == 0 || skipped < total {
		return 0
	}
	return 1
}

// onioncheckHeadline собирает первую строку отчёта о живости onion-движков.
//
// Число живых приходит из пула здоровья, поэтому оно включает замеры прошлых
// волн. При пропущенных проверках строка обязана называть источник: живой замер
// на копии боевой базы давал «живых 3 из 7 (пропущено 7: нужен tor или прокси)»,
// где ни один движок не был спрошен, а три живых остались от прошлых прогонов.
// Читатель получал утверждение о текущем состоянии пула, которого команда в этом
// прогоне не проверяла. На свежей базе та же конструкция читалась ещё хуже:
// «живых 0 из 4» означало отсутствие истории, а не смерть движков.
//
// Функция отделена от команды, потому что команда поднимает tor и конфиг, а три
// состава волны - состоявшаяся, частично пропущенная и полностью пропущенная -
// должны проверяться без сети.
func onioncheckHeadline(live, total, skipped int) string {
	switch {
	case skipped <= 0:
		return fmt.Sprintf("onion-поисковики: живых %d из %d", live, total)
	case skipped >= total:
		return fmt.Sprintf("onion-поисковики: живых %d из %d по прошлым замерам (пропущено %d: нужен tor или прокси)",
			live, total, skipped)
	default:
		return fmt.Sprintf("onion-поисковики: живых %d из %d (пропущено %d: нужен tor или прокси, живость непроверенных по прошлым замерам)",
			live, total, skipped)
	}
}

// torStatusLine собирает строку состояния tor для отчёта.
//
// Статус control-канала берётся у ротатора, который умеет его проверять.
// Интерфейс опциональный: собственный демон и внешний tor реализуют его
// по-разному, а прямой транспорт не реализует вовсе, поэтому отсутствие
// метода не ошибка, а другой вид транспорта.
func torStatusLine(r netx.Rotator) string {
	base := fmt.Sprintf("%s, транспорт %s", r.Kind(), r.TransportSpec())

	type controlStater interface {
		ControlStatus() string
	}
	if cs, ok := r.(controlStater); ok {
		return base + ", control " + cs.ControlStatus()
	}

	type controlAddrer interface {
		ControlAddr() string
	}
	if ca, ok := r.(controlAddrer); ok {
		if addr := ca.ControlAddr(); addr != "" {
			return base + ", control " + addr
		}
	}
	return base + ", control не задан"
}
