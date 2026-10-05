package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
)

// parseFlags поднимает флаги из хвоста аргументов и разбирает их, подготовив
// русский отчёт об ошибке разбора.
func parseFlags(fs *flag.FlagSet, args []string) {
	parseFlagsRaw(fs, hoistFlags(fs, args))
}

// parseFlagsRaw разбирает аргументы без перестановки. Им пользуются команды,
// которые hoistFlags исторически не применяли (hunt create, hunt run,
// hunt watch): менять их поведение вместе с формой отчёта незачем, а отчёт об
// ошибке им нужен точно такой же.
func parseFlagsRaw(fs *flag.FlagSet, args []string) {
	armFlagReport(fs, args)
	fs.Parse(args)
}

// armFlagReport перехватывает сообщение пакета flag и печатает его по-русски,
// с именем команды и, в режиме --json, телом в stdout.
//
// Живой замер ДО на бинаре c488560, копия базы %TEMP%\vss\livedata102:
//
//	stats --nonsense            -> rc=2, stdout 0 байт, stderr 89
//	                               «flag provided but not defined: -nonsense»
//	                               и стандартный «Usage of stats:»
//	poolsearch --json --nope    -> rc=2, stdout 0 байт, stderr 1814
//	run --httpp 127.0.0.1:1     -> rc=2, stdout 0 байт, stderr 657
//	backup --what               -> rc=2, stdout 0 байт, stderr 356
//
// То есть машина, которая просила JSON, не получала тела вовсе и видела только
// код 2, а человек читал английскую причину и список флагов без имени команды.
// При этом у части команд свой usage уже русский и подробный (poolsearch), а у
// части - стандартный, то есть форма отказа гуляла от команды к команде.
//
// Наборы остаются с flag.ExitOnError намеренно: код возврата 2 для ошибки
// аргументов - сложившийся контракт, и ломать его чужим скриптам нельзя.
// Меняется только вывод, и делается это до разбора, потому что пакет flag
// печатает причину и зовёт Usage сам, а затем завершает процесс.
func armFlagReport(fs *flag.FlagSet, args []string) {
	trap := &flagTrap{fs: fs, args: args}
	fs.SetOutput(trap)
	prev := fs.Usage
	fs.Usage = func() { trap.report(prev) }
}

// flagTrap копит то, что пакет flag собирался напечатать в stderr, чтобы отдать
// это одной русской строкой. Поток вывода подменяется именно поэтому: сообщение
// «flag provided but not defined» уходит в ловушку, а не оператору.
type flagTrap struct {
	fs   *flag.FlagSet
	args []string
	buf  bytes.Buffer
}

func (t *flagTrap) Write(p []byte) (int, error) { return t.buf.Write(p) }

// report печатает причину и справку.
//
// Пустой буфер означает справочный вызов: на -h и --help пакет flag зовёт Usage
// без сообщения об ошибке и завершает процесс с кодом 0. Печатать там текст про
// неизвестный флаг значило бы показать оператору отказ там, где он просил
// справку, поэтому ветки разделены.
//
// prev - прежний Usage команды. Его нельзя терять: у poolsearch, discover и
// других команд он объясняет позиционные аргументы, которых в списке флагов
// просто нет, и ради этого текста он когда-то и был написан.
func (t *flagTrap) report(prev func()) {
	raw := strings.TrimSpace(t.buf.String())
	// Вывод возвращается на stderr до печати: PrintDefaults пишет в поток
	// набора, и оставь мы ловушку, список флагов ушёл бы в буфер и пропал.
	t.fs.SetOutput(os.Stderr)

	if raw == "" {
		t.printUsage(prev)
		return
	}

	msg := flagMessage(t.fs.Name(), raw)
	fmt.Fprintln(os.Stderr, "ERROR: "+msg)
	if wantsJSON(t.args) {
		// Машинный потребитель читает stdout: код 2 говорит «аргументы не
		// разобраны», а причину он берёт из потока данных. Сериализация идёт
		// через общий writeJSON, потому что страж
		// TestJSONOutputGoesThroughWriteJSON требует ровно одну точку
		// сериализации в пакете.
		jsonMode = true
		writeJSON(map[string]string{"error": msg})
		return
	}
	t.printUsage(prev)
}

// printUsage решает, чью справку показать.
//
// NewFlagSet присваивает f.Usage = f.defaultUsage, поэтому prev никогда не
// бывает nil, а сравнить функции напрямую нельзя: остаётся посмотреть, что они
// печатают. Стандартная справка начинается со строки «Usage of <имя>:» и не
// называет ни причины отказа, ни команды по-человечески, поэтому вместо неё
// печатаем свой русский заголовок и тот же список флагов. Свой usage команды
// (у poolsearch он объясняет позиционный запрос) сохраняется целиком: текст
// писался ради оператора, и терять его нельзя.
//
// prev вызывается ровно один раз: вывод перехватывается в буфер, поэтому
// повторный вызов напечатал бы справку дважды.
func (t *flagTrap) printUsage(prev func()) {
	if prev == nil {
		t.printFlagList()
		return
	}
	var buf bytes.Buffer
	saved := t.fs.Output()
	t.fs.SetOutput(&buf)
	prev()
	t.fs.SetOutput(saved)
	text := buf.String()
	switch {
	case strings.HasPrefix(text, "Usage of "), strings.HasPrefix(text, "Usage:"):
		t.printFlagList()
	case buf.Len() > 0:
		// Свой usage, который пишет в поток набора: отдаём накопленное.
		os.Stderr.Write(buf.Bytes())
	default:
		// Свой usage пишет напрямую в stderr и уже напечатался сам.
	}
}

func (t *flagTrap) printFlagList() {
	fmt.Fprintf(os.Stderr, "флаги команды %s:\n", t.fs.Name())
	t.fs.PrintDefaults()
}

// flagMessage переводит сообщение пакета flag на русский и добавляет имя
// команды. Имя обязательно: оператор, который гоняет несколько команд подряд
// или читает лог служебного запуска, без него не понимает, к чему относится
// отказ.
//
// Разбор идёт по префиксам текста пакета flag, потому что другой ручки нет:
// failf возвращает уже собранную строку. Список префиксов взят из исходника
// flag.go (строки 1093-1139): bad flag syntax, flag provided but not defined,
// invalid boolean value, invalid boolean flag, flag needs an argument,
// invalid value. Нераспознанный случай не теряется - он уходит в общий текст
// вместе с оригиналом, поэтому новое сообщение библиотеки сделает отказ менее
// красивым, но не молчаливым.
func flagMessage(cmd, raw string) string {
	switch {
	case strings.HasPrefix(raw, "flag provided but not defined: -"):
		return fmt.Sprintf("неизвестный флаг -%s команды %s",
			strings.TrimPrefix(raw, "flag provided but not defined: -"), cmd)
	case strings.HasPrefix(raw, "flag needs an argument: -"):
		return fmt.Sprintf("флагу -%s команды %s нужно значение",
			strings.TrimPrefix(raw, "flag needs an argument: -"), cmd)
	case strings.HasPrefix(raw, "invalid value "):
		return fmt.Sprintf("неверное значение флага команды %s: %s",
			cmd, strings.TrimPrefix(raw, "invalid value "))
	case strings.HasPrefix(raw, "invalid boolean value "):
		return fmt.Sprintf("неверное булево значение флага команды %s: %s",
			cmd, strings.TrimPrefix(raw, "invalid boolean value "))
	case strings.HasPrefix(raw, "invalid boolean flag "):
		return fmt.Sprintf("неверный булев флаг команды %s: %s",
			cmd, strings.TrimPrefix(raw, "invalid boolean flag "))
	case strings.HasPrefix(raw, "bad flag syntax: "):
		return fmt.Sprintf("неверный синтаксис флага команды %s: %s",
			cmd, strings.TrimPrefix(raw, "bad flag syntax: "))
	}
	return fmt.Sprintf("ошибка разбора флагов команды %s: %s", cmd, raw)
}
