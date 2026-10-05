package classifier

import (
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// Этот файл фиксирует главное требование к переписыванию Classify: вердикты не
// изменились. Эталон - прежняя реализация дословно, на strings.Contains и
// strings.ToLower(strings.Join(...)). Сравнение идёт на фиксированных и на
// случайных входах, включая маркеры в верхнем регистре и некорректный UTF-8.

// classifyReference - прежняя реализация Classify без изменений.
func classifyReference(rawURL, title, snippet, htmlHint string) Verdict {
	u, _ := url.Parse(strings.TrimSpace(rawURL))
	host := ""
	if u != nil {
		host = strings.ToLower(u.Host)
	}
	text := strings.ToLower(strings.Join([]string{title, snippet, htmlHint}, "\n"))

	if t, q, reason := scamCheckReference(text); t != "" {
		return Verdict{Type: t, Quality: q, Popularity: 0.1, Reason: reason}
	}
	if hasLoginFormReference(text) {
		return Verdict{Type: TypePrivate, Quality: 0.7, Popularity: popularityReference(host, text),
			Reason: "форма входа: password/login/sign-in в разметке или тексте"}
	}
	if hasPaywallReference(text) {
		return Verdict{Type: TypePaid, Quality: 0.6, Popularity: popularityReference(host, text),
			Reason: "paywall-маркеры: подписка/premium/платный доступ"}
	}
	q := 0.5
	reasons := []string{"публичный источник без ограничений"}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "https://") {
		q += 0.05
	}
	if strings.TrimSpace(title) != "" && strings.TrimSpace(snippet) != "" {
		q += 0.1
	}
	if hasCaptchaReference(text) {
		q = min(q, 0.3)
		reasons = []string{"антибот-защита: captcha/challenge - содержимое видно не полностью"}
	}
	if q > 1.0 {
		q = 1.0
	}
	return Verdict{Type: TypePublic, Quality: round1(q),
		Popularity: popularityReference(host, text), Reason: strings.Join(reasons, "; ")}
}

func hasLoginFormReference(text string) bool {
	hits := 0
	for _, m := range loginMarkers {
		if strings.Contains(text, m) {
			hits++
		}
	}
	if strings.Contains(text, `type="password"`) || strings.Contains(text, `type='password'`) {
		return true
	}
	return hits >= 2
}

func hasPaywallReference(text string) bool {
	for _, m := range paywallMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

func hasCaptchaReference(text string) bool {
	for _, m := range captchaMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

func scamCheckReference(text string) (string, float64, string) {
	hasMoney := false
	for _, m := range moneyMarkers {
		if strings.Contains(text, m) {
			hasMoney = true
			break
		}
	}
	if !hasMoney {
		return "", 0, ""
	}
	for _, p := range promiseMarkers {
		if strings.Contains(text, p) {
			return TypeScam, 0.1, "скам-связка: деньги + гарантия/удвоение (" + p + ")"
		}
	}
	for _, p := range pharmaMarkers {
		if strings.Contains(text, p) {
			return TypeScam, 0.2, "скам-маркер: " + p
		}
	}
	return "", 0, ""
}

// popularityReference считает длину текста целиком, без предела.
func popularityReference(host, text string) float64 {
	trusted := []string{
		"wikipedia.org", "github.com", "stackoverflow.com", "arxiv.org",
		"python.org", "golang.org", "mozilla.org",
	}
	for _, t := range trusted {
		if strings.Contains(host, t) {
			return 0.9
		}
	}
	switch l := len([]rune(text)); {
	case l > 1000:
		return 0.6
	case l > 300:
		return 0.5
	case l > 80:
		return 0.4
	default:
		return 0.3
	}
}

// fixedInputs покрывает каждую ветку вердикта и несколько пограничных случаев:
// маркер в конце большого тела, маркер в верхнем регистре, два маркера входа,
// некорректный UTF-8, пустые входы.
func fixedInputs() []struct{ url, title, snippet, hint string } {
	filler := strings.Repeat("обычный текст страницы без маркеров. ", 40)
	return []struct{ url, title, snippet, hint string }{
		{"http://a.onion/", "", "", ""},
		{"https://ru.wikipedia.org/wiki/X", "Заголовок", "Сниппет", "содержимое статьи"},
		{"http://b.onion/x", "Вход", "Страница", `<form><input type="password"></form>`},
		{"http://c.onion/x", "Вход", "Страница", "forgot password and log in to continue"},
		{"http://d.onion/x", "Вход", "Страница", "sign in to your account"},
		{"http://e.onion/x", "Журнал", "Текст", "оформите подписку, чтобы читать дальше"},
		{"http://f.onion/x", "Крипта", "Текст", "crypto investment: double your money"},
		{"http://g.onion/x", "Крипта", "Текст", "честный обзор bitcoin без обещаний"},
		{"http://h.onion/x", "Аптека", "Текст", "купить диплом и viagra недорого"},
		{"http://i.onion/x", "Проверка", "Текст", "Just A Moment... attention required"},
		{"http://j.onion/x", "Длинное", "Тело", filler + "unlock full access"},
		{"http://k.onion/x", "РЕГИСТР", "ТЕКСТ", "CRYPTO INVESTMENT GUARANTEED PROFIT"},
		{"http://l.onion/x", "Битый", "UTF", "хвост \xff\xfe с некорректными байтами"},
		{"http://m.onion/x", "Короткий", "", "a"},
		{"", "Без адреса", "Сниппет", "какой-то текст"},
		{"https://github.com/x", "Репозиторий", "Описание", strings.Repeat("код код код. ", 200)},
	}
}

func TestClassifyMatchesReferenceOnFixedInputs(t *testing.T) {
	for _, in := range fixedInputs() {
		want := classifyReference(in.url, in.title, in.snippet, in.hint)
		got := Classify(in.url, in.title, in.snippet, in.hint)
		if got != want {
			t.Errorf("расхождение вердикта\nвход: %q / %q / %q / hint(%d байт)\nполучено: %+v\nэталон:   %+v",
				in.url, in.title, in.snippet, len(in.hint), got, want)
		}
	}
}

// TestClassifyMatchesReferenceOnRandomInputs строит тексты из кусков маркеров,
// их префиксов и суффиксов, наполнителя и вариантов в верхнем регистре. Такие
// входы ловят ошибки как в автомате (частичное совпадение, суффиксы), так и в
// приведении регистра.
func TestClassifyMatchesReferenceOnRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(20240519))

	pieces := []string{}
	for _, list := range [][]string{
		moneyMarkers, promiseMarkers, pharmaMarkers,
		loginMarkers, paywallMarkers, captchaMarkers,
	} {
		for _, m := range list {
			pieces = append(pieces, m, strings.ToUpper(m), m[:len(m)/2], m[len(m)/2:])
		}
	}
	filler := []string{
		"обычный текст", "<div class=\"row\">", " ", "\n", "the quick brown fox",
		"ПРИВЕТ МИР", "καλημέρα", "0123456789", "\xff", "<p></p>", "İstanbul",
	}

	randomText := func(maxParts int) string {
		n := rng.Intn(maxParts) + 1
		var sb strings.Builder
		for i := 0; i < n; i++ {
			if rng.Intn(2) == 0 {
				sb.WriteString(pieces[rng.Intn(len(pieces))])
			} else {
				sb.WriteString(filler[rng.Intn(len(filler))])
			}
		}
		return sb.String()
	}

	urls := []string{"http://x.onion/a", "https://ru.wikipedia.org/w", "", "ftp://host/p", "https://example.com"}

	for i := 0; i < 3000; i++ {
		in := struct{ url, title, snippet, hint string }{
			url:     urls[rng.Intn(len(urls))],
			title:   randomText(3),
			snippet: randomText(3),
			hint:    randomText(12),
		}
		want := classifyReference(in.url, in.title, in.snippet, in.hint)
		got := Classify(in.url, in.title, in.snippet, in.hint)
		if got != want {
			t.Fatalf("расхождение на входе %d: url=%q title=%q snippet=%q hint=%q\nполучено: %+v\nэталон:   %+v",
				i, in.url, in.title, in.snippet, in.hint, got, want)
		}
	}
}

// TestScanMarkersMatchesContains проверяет сам автомат: для каждого маркера
// результат совпадает с strings.Contains по тому же тексту.
func TestScanMarkersMatchesContains(t *testing.T) {
	texts := []string{
		"",
		"нет ничего интересного",
		"crypto",
		"crypt",
		"ccrypto bitcoin",
		`<input TYPE="PASSWORD">`,
		`<input type="password">`,
		"войти в аккаунт или введите пароль",
		"unlock full access",
		"удвоим ваш доход гарантированная прибыль",
		"хвост с маркером в самом конце: datadome",
		strings.Repeat("наполнение ", 500) + "subscribe to read",
		"подтвердите, что вы не робот",
		"некорректный \xff\xfe байт и captcha рядом",
	}
	for _, text := range texts {
		lowered := strings.ToLower(text)
		got := scanMarkers(lowered)
		if len(got) != len(allMarkers) {
			t.Fatalf("размер набора %d, маркеров %d", len(got), len(allMarkers))
		}
		for i, m := range allMarkers {
			if want := strings.Contains(lowered, m); got[i] != want {
				t.Errorf("текст %q: маркер %q получен=%v эталон=%v", text, m, got[i], want)
			}
		}
	}
}

// TestScanMarkersRandom сверяет автомат с strings.Contains на случайных
// последовательностях байтов и кусков маркеров.
func TestScanMarkersRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "b", "c", "крипт", "о", "доход", " ", "password", `type="`, "1", "0", "%", "x"}

	for i := 0; i < 5000; i++ {
		n := rng.Intn(40) + 1
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		// Маркеры заданы строчными, поэтому полный маркер добавляется уже
		// приведённым к нижнему регистру.
		text := strings.ToLower(sb.String())
		if rng.Intn(3) == 0 {
			text += allMarkers[rng.Intn(len(allMarkers))]
		}

		got := scanMarkers(text)
		for k, m := range allMarkers {
			if want := strings.Contains(text, m); got[k] != want {
				t.Fatalf("итерация %d, текст %q: маркер %q получен=%v эталон=%v", i, text, m, got[k], want)
			}
		}
	}
}

// TestACScannerSuffixOverlap проверяет слияние выводов: когда один шаблон
// является суффиксом другого, найдены должны быть оба. Реальный список
// маркеров таких пар не содержит, поэтому автомат проверяется на отдельном
// наборе шаблонов.
func TestACScannerSuffixOverlap(t *testing.T) {
	patterns := []string{"abcd", "bcd", "cd", "d", "ab", "abcd"}
	sc := newACScanner(patterns)

	for _, text := range []string{"", "d", "cd", "bcd", "abcd", "xxabcdyy", "ab", "abc", "zzz", "abcdabcd"} {
		got := make(markerHits, len(patterns))
		sc.scan(text, got)
		for i, p := range patterns {
			if want := strings.Contains(text, p); got[i] != want {
				t.Errorf("текст %q шаблон %q: получен=%v эталон=%v", text, p, got[i], want)
			}
		}
	}
}

// TestACScannerSharedPrefix проверяет, что общий префикс шаблонов не ломает
// переходы: «crypto» и «крипт» начинаются по-разному, а «double your» и
// «datadome» делят только отсутствие общего префикса, поэтому набор взят с
// явным пересечением в начале.
func TestACScannerSharedPrefix(t *testing.T) {
	patterns := []string{"abc", "abd", "abe", "bc", "c"}
	sc := newACScanner(patterns)

	for _, text := range []string{"abc", "abd", "abe", "ab", "a", "bc", "c", "xabcy", "abdabc", "cccc"} {
		got := make(markerHits, len(patterns))
		sc.scan(text, got)
		for i, p := range patterns {
			if want := strings.Contains(text, p); got[i] != want {
				t.Errorf("текст %q шаблон %q: получен=%v эталон=%v", text, p, got[i], want)
			}
		}
	}
}

// TestBuildTextMatchesToLowerJoin доказывает побайтовое совпадение подготовки
// текста с прежней конструкцией strings.ToLower(strings.Join(...)).
func TestBuildTextMatchesToLowerJoin(t *testing.T) {
	parts := []string{
		"", " ", "\n", "ASCII TEXT", "строчный текст", "ЗАГЛАВНЫЕ РУССКИЕ",
		"ΠΆΝΤΆ", "İstanbul ǅungla", "\xff\xfe\x00", "καλή μέρα",
		strings.Repeat("СмЕшАнНыЙ ТеКсТ ", 100), "type=\"PASSWORD\"",
	}

	for _, a := range parts {
		for _, b := range parts {
			for _, c := range parts {
				want := strings.ToLower(strings.Join([]string{a, b, c}, "\n"))
				got := buildText(a, b, c)
				if got != want {
					t.Fatalf("расхождение сборки текста\na=%q b=%q c=%q\nполучено: %q\nэталон:   %q", a, b, c, got, want)
				}
			}
		}
	}
}

// TestBuildTextRandom сверяет сборку на случайных последовательностях рун,
// включая суррогатные области и некорректные байты.
func TestBuildTextRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	randomPart := func() string {
		n := rng.Intn(30)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			switch rng.Intn(4) {
			case 0:
				sb.WriteRune(rune('a' + rng.Intn(26)))
			case 1:
				sb.WriteRune(rune('А' + rng.Intn(32)))
			case 2:
				sb.WriteByte(byte(rng.Intn(256)))
			default:
				sb.WriteRune(rune(rng.Intn(0x1000)))
			}
		}
		return sb.String()
	}

	for i := 0; i < 3000; i++ {
		a, b, c := randomPart(), randomPart(), randomPart()
		want := strings.ToLower(strings.Join([]string{a, b, c}, "\n"))
		got := buildText(a, b, c)
		if got != want {
			t.Fatalf("итерация %d: расхождение\na=%q b=%q c=%q\nполучено: %q\nэталон:   %q", i, a, b, c, got, want)
		}
	}
}

// TestBoundedRuneCount подтверждает два свойства: до предела результат равен
// полному подсчёту, на пределе и выше - равен пределу, а неположительный
// предел даёт ноль.
func TestBoundedRuneCount(t *testing.T) {
	short := "обычный текст разной длины"
	if got := boundedRuneCount(short, popularityRuneCap); got != utf8.RuneCountInString(short) {
		t.Errorf("короткий текст: получено %d, эталон %d", got, utf8.RuneCountInString(short))
	}
	if got := boundedRuneCount("", popularityRuneCap); got != 0 {
		t.Errorf("пустой текст: получено %d", got)
	}

	long := strings.Repeat("текст ", 5000) // 30000 рун
	if got := boundedRuneCount(long, popularityRuneCap); got != popularityRuneCap {
		t.Errorf("длинный текст: получено %d, ожидается предел %d", got, popularityRuneCap)
	}
	if got := boundedRuneCount(long, 0); got != 0 {
		t.Errorf("нулевой предел: получено %d", got)
	}
	if got := boundedRuneCount(long, -5); got != 0 {
		t.Errorf("отрицательный предел: получено %d", got)
	}

	// Точная граница: ровно limit рун и limit-1 рун.
	exact := strings.Repeat("ы", popularityRuneCap)
	if got := boundedRuneCount(exact, popularityRuneCap); got != popularityRuneCap {
		t.Errorf("ровно предел: получено %d", got)
	}
	if got := boundedRuneCount(exact[:len(exact)-2], popularityRuneCap); got != popularityRuneCap-1 {
		t.Errorf("на руну меньше предела: получено %d", got)
	}
}

// TestPopularityUnchangedByRuneCap проверяет, что предел подсчёта не меняет
// оценку: все пороги ветвления ниже предела, поэтому результат совпадает с
// полным подсчётом рун.
func TestPopularityUnchangedByRuneCap(t *testing.T) {
	lengths := []int{0, 1, 79, 80, 81, 299, 300, 301, 999, 1000, 1001, 5000, 20000}
	for _, n := range lengths {
		text := padRunes(n)
		if utf8.RuneCountInString(text) != n {
			t.Fatalf("фикстура неверна: длина %d вместо %d", utf8.RuneCountInString(text), n)
		}
		want := popularityReference("example.onion", text)
		got := popularity("example.onion", text)
		if got != want {
			t.Errorf("длина %d рун: получено %v, эталон %v", n, got, want)
		}
	}
}

// padRunes возвращает текст ровно из n многобайтовых рун.
func padRunes(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("т", n)
}

// TestTrustedHostSkipsRuneCount проверяет, что для доверенного хоста длина
// текста не влияет на результат.
func TestTrustedHostSkipsRuneCount(t *testing.T) {
	for _, host := range []string{"ru.wikipedia.org", "github.com", "stackoverflow.com"} {
		if got := popularity(host, ""); got != 0.9 {
			t.Errorf("хост %s: получено %v, ожидается 0.9", host, got)
		}
		if got := popularity(host, strings.Repeat("текст", 10000)); got != 0.9 {
			t.Errorf("хост %s на длинном тексте: получено %v, ожидается 0.9", host, got)
		}
	}
}
