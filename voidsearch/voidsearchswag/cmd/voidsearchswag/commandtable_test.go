package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// readmePath - путь к README из каталога пакета.
const readmePath = "../../README.md"

// readmeCommandsLine достаёт строку перечня консольных команд из README.
var readmeCommandsLine = regexp.MustCompile(`Консольные команды: ([^\n]+)`)

// helpCommandLine распознаёт строку справки вида «  имя   описание»: имя и не
// меньше двух пробелов до текста.
var helpCommandLine = regexp.MustCompile(`^ {2}([a-z][a-z0-9]*) {2,}`)

// Перечень команд жил в трёх копиях: switch диспетчера, текст usage и строка
// README. Замер до правки на HEAD 8f95a88: диспетчер и справка знали 20 команд,
// а README перечислял 15 - без tord, promote, backup, restore и clean, - и
// перечислял подкоманды hunt как create|list|run|watch, хотя hits существует и
// описан в самой справке. Оператор, который читает README, не узнаёт ни о
// даемоне tor, ни о снимках базы.
func TestCommandsTableIsUnique(t *testing.T) {
	if len(commands) == 0 {
		t.Fatal("таблица команд пуста")
	}
	seen := make(map[string]bool, len(commands))
	for _, c := range commands {
		if strings.TrimSpace(c.name) != c.name || c.name == "" {
			t.Errorf("имя команды %q не нормализовано", c.name)
		}
		if strings.ContainsAny(c.name, " \t|") {
			t.Errorf("имя команды %q содержит разделители", c.name)
		}
		if seen[c.name] {
			t.Errorf("команда %q встречается в таблице дважды", c.name)
		}
		seen[c.name] = true
		if strings.TrimSpace(c.help) == "" {
			t.Errorf("у команды %q пустое описание в справке", c.name)
		}
		if c.run == nil {
			t.Errorf("у команды %q нет обработчика", c.name)
		}
	}
}

// Маршрутизация проверяется по таблице, а не запуском команд: часть из них уходит
// в сеть и поднимает tor.
func TestLookupCommandRoutesEveryCommand(t *testing.T) {
	for _, name := range commandNames() {
		run, ok := lookupCommand(name)
		if !ok {
			t.Errorf("lookupCommand(%q) = false", name)
			continue
		}
		if run == nil {
			t.Errorf("lookupCommand(%q) вернула nil", name)
		}
	}

	for _, name := range []string{"", " ", "nope", "hunt create", "--version", "-v", "help", "--help", "-h", "Files", "files "} {
		if _, ok := lookupCommand(name); ok {
			t.Errorf("lookupCommand(%q) = true, хотя это не имя команды из таблицы", name)
		}
	}
}

// Алиасы версии и справки обязаны остаться рабочими: они не входят в таблицу, но
// диспетчер их принимает.
func TestVersionAliasesStillWork(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		stdout, code := runMain(t, append(args, "--json")...)
		if code != 0 {
			t.Errorf("%v: rc=%d", args, code)
			continue
		}
		if !strings.Contains(stdout, `"program"`) || !strings.Contains(stdout, `"version"`) {
			t.Errorf("%v: stdout без полей program/version: %q", args, strings.TrimSpace(stdout))
		}
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	text := captureStderr(t, usage)
	if !strings.Contains(text, "Команды:") {
		t.Fatalf("в справке нет раздела «Команды:»:\n%s", text)
	}

	listed := make(map[string]bool)
	for _, ln := range strings.Split(text, "\n") {
		if m := helpCommandLine.FindStringSubmatch(ln); m != nil {
			listed[m[1]] = true
		}
	}
	if len(listed) == 0 {
		t.Fatalf("в справке не разобрана ни одна строка команды:\n%s", text)
	}

	for _, name := range commandNames() {
		if !listed[name] {
			t.Errorf("команда %s есть в таблице, но не напечатана в справке", name)
		}
	}
	for name := range listed {
		if _, ok := lookupCommand(name); !ok {
			t.Errorf("справка печатает %s, которого нет в таблице команд", name)
		}
	}
}

// Справка обязана сохранить хвост: общий флаг и путь к данным читают чаще списка
// команд, и потеря этого текста была бы незаметна до первого вопроса оператора.
func TestUsageKeepsTailSections(t *testing.T) {
	text := captureStderr(t, usage)
	for _, want := range []string{
		"VoidSearchSwag",
		"Общий флаг для любой команды:",
		"--verbose",
		"VOIDSEARCH_VERBOSE",
		"Данные и состояние:",
		".voidsearchswag",
		"$VOIDSEARCH_DATA_DIR",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в справке нет %q", want)
		}
	}
	if strings.Contains(text, "%%USERPROFILE%%") {
		t.Error("в справке остался удвоенный процент: строка печатается без форматирования")
	}
	if !strings.Contains(text, "%USERPROFILE%") {
		t.Errorf("в справке нет пути к каталогу данных: %s", "%USERPROFILE%")
	}
}

func TestReadmeListsEveryCommand(t *testing.T) {
	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("чтение %s: %v", readmePath, err)
	}
	m := readmeCommandsLine.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("в %s нет строки «Консольные команды:»", readmePath)
	}

	// Имя команды в README обёрнуто в обратные кавычки и может стоять как одна
	// (`files`), так и вместе с перечнем подкоманд (`hunt create|list|...`),
	// поэтому разбирается начало каждой обёртки.
	documented := make(map[string]bool)
	for _, found := range regexp.MustCompile("`([a-z][a-z0-9]*)").FindAllStringSubmatch(m[1], -1) {
		documented[found[1]] = true
	}
	if len(documented) == 0 {
		t.Fatalf("в строке «Консольные команды:» не разобрано ни одного имени")
	}

	for _, name := range commandNames() {
		if !documented[name] {
			t.Errorf("%s: команда %s не перечислена в строке «Консольные команды:»", readmePath, name)
		}
	}
	for name := range documented {
		if _, ok := lookupCommand(name); !ok {
			t.Errorf("%s: перечислена команда %s, которой нет в диспетчере", readmePath, name)
		}
	}
}

// Подкоманды hunt обязаны совпадать в трёх местах: сообщение об ошибке самой
// команды, описание в таблице и строка README. Замер до правки: README перечислял
// create|list|run|watch без hits.
func TestHuntSubcommandsDocumented(t *testing.T) {
	stdout, stderr, code := runMainSplit(t, "hunt")
	if code == 0 {
		t.Fatalf("hunt без подкоманды вернул rc=0")
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("hunt без подкоманды напечатал в stdout %q", strings.TrimSpace(stdout))
	}
	msg := regexp.MustCompile(`hunt ([a-z|]+)`).FindStringSubmatch(stderr)
	if msg == nil {
		t.Fatalf("в сообщении нет перечня подкоманд: %q", strings.TrimSpace(stderr))
	}
	live := strings.Split(msg[1], "|")
	if len(live) == 0 {
		t.Fatal("перечень подкоманд пуст")
	}

	help := ""
	for _, c := range commands {
		if c.name == "hunt" {
			help = c.help
			break
		}
	}
	if help == "" {
		t.Fatal("в таблице команд нет hunt")
	}
	for _, sub := range live {
		if !strings.Contains(help, sub) {
			t.Errorf("описание hunt в таблице не упоминает подкоманду %s", sub)
		}
	}

	raw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("чтение %s: %v", readmePath, err)
	}
	// Перечень подкоманд может встречаться в README не один раз, поэтому
	// собираются все совпадения, а не первое.
	documented := make(map[string]bool)
	for _, found := range regexp.MustCompile("`hunt ([a-z|]+)`").FindAllStringSubmatch(string(raw), -1) {
		for _, sub := range strings.Split(found[1], "|") {
			if sub != "" {
				documented[sub] = true
			}
		}
	}
	if len(documented) == 0 {
		t.Fatalf("в %s нет перечня подкоманд hunt", readmePath)
	}
	for _, sub := range live {
		if !documented[sub] {
			t.Errorf("%s: подкоманда hunt %s не перечислена, хотя команда её принимает", readmePath, sub)
		}
	}
	for d := range documented {
		found := false
		for _, sub := range live {
			if sub == d {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: перечислена подкоманда hunt %s, которой команда не принимает", readmePath, d)
		}
	}
}
