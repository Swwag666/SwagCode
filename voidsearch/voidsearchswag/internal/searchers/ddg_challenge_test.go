package searchers

import (
	"strings"
	"testing"
)

// TestParseDDGRejectsChallengePage закрывает дефект, из-за которого страница
// антибот-проверки возвращалась как успешная выдача.
//
// Проверка isDDGChallenge стояла после третьего фолбэка, а тот собирал все
// внешние ссылки страницы без фильтрации. Страница капчи всегда содержит
// внешние ссылки - справку, юридические страницы, домен вендора, - поэтому
// фолбэк срабатывал, проверка не вызывалась никогда, и мусор уходил в кэш на
// весь CacheTTL. Мимолётный rate-limit отравлял запрос на час.
func TestParseDDGRejectsChallengePage(t *testing.T) {
	// Реалистичная страница аномалии: текст заглушки плюс обычные внешние
	// ссылки в подвале, которые прежний фолбэк принимал за результаты.
	challenge := `<html><head><title>Anomaly</title></head><body>
		<h1>Please confirm you are human</h1>
		<p>We detected unusual traffic from your network.</p>
		<a href="https://help.duckduckgo.com/anomaly">help</a>
		<a href="https://duckduckgo.com/privacy">Privacy Policy</a>
		<a href="https://example-legal.com/terms">Terms</a>
	</body></html>`

	res, err := parseDDG([]byte(challenge), "ddg-test")
	if err == nil {
		t.Fatalf("страница капчи принята как выдача: %d результатов %+v", len(res), res)
	}
	if !strings.Contains(err.Error(), "антибот") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("вместе с ошибкой вернулись результаты: %+v", res)
	}
}

func TestParseDDGChallengeDetectionIsSpecific(t *testing.T) {
	// «challenge» убран из маркеров: это обычное слово в текстах и разметке
	// DuckDuckGo, и оно давало ложное срабатывание на честной пустой выдаче.
	// Страница с результатом про «challenge» не должна считаться капчей.
	legit := `<html><body>
		<table><tr><td class="result-link">
			<a href="https://example.com/coding-challenge">Coding challenge</a>
		</td></tr>
		<tr><td class="result-snippet">Описание задачи</td></tr></table>
	</body></html>`

	res, err := parseDDG([]byte(legit), "ddg-test")
	if err != nil {
		t.Fatalf("честная выдача отвергнута: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("результатов %d, ожидала 1", len(res))
	}
	if !strings.Contains(res[0].Title, "challenge") {
		t.Errorf("заголовок потерян: %q", res[0].Title)
	}
}

func TestIsDDGChallengeChecksHeadOnly(t *testing.T) {
	// Проверка ограничена началом тела: страница капчи показывает свой текст
	// сразу, а приводить к нижнему регистру 12 МБ на каждый пустой результат
	// неоправданно дорого.
	prefix := []byte("<html><body><h1>anomaly detected</h1>")
	if !isDDGChallenge(prefix) {
		t.Error("маркер в начале тела не распознан")
	}

	// Маркер далеко за пределами проверяемого окна не считается: на глубине
	// 64 КБ это уже обычный контент страницы, а не заглушка.
	far := make([]byte, 0, (96<<10)+64)
	far = append(far, []byte("<html><body>")...)
	far = append(far, fillBytes('x', 96<<10)...)
	far = append(far, []byte("unusual traffic")...)
	if isDDGChallenge(far) {
		t.Error("проверка вышла за пределы окна в начале тела")
	}
}

func TestParseDDGCatchAllFiltersNoise(t *testing.T) {
	// Третий фолбэк теперь применяет тот же фильтр шума, что и parseOnion.
	// Без него в выдачу попадали «About», «Privacy», «Terms» и прочая
	// навигация - ссылки, которые выглядят как результаты.
	page := `<html><body>
		<a href="https://about.example/privacy">Privacy Policy</a>
		<a href="https://shop.example/terms">Terms</a>
		<a href="https://donate.example/donate">Donate</a>
		<a href="https://real.example/actual-result">Настоящий результат про кванты</a>
	</body></html>`

	res, err := parseDDG([]byte(page), "ddg-test")
	if err != nil {
		t.Fatalf("страница с одним полезным результатом отвергнута: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("результатов %d, ожидала 1 (шум должен быть отфильтрован): %+v", len(res), res)
	}
	if !strings.Contains(res[0].URL, "actual-result") {
		t.Errorf("вернулся не полезный результат: %s", res[0].URL)
	}
}

func TestParseDDGUnrecognizedMarkupStillReported(t *testing.T) {
	// Страница без признаков капчи и без распознаваемой разметки обязана дать
	// внятную ошибку, а не пустой успешный ответ.
	res, err := parseDDG([]byte("<html><body><p>ничего похожего</p></body></html>"), "ddg-test")
	if err == nil {
		t.Fatalf("нераспознанная разметка не вызвала ошибку: %+v", res)
	}
	if !strings.Contains(err.Error(), "не распознана") {
		t.Errorf("неожиданный текст ошибки: %v", err)
	}
}

// fillBytes - локальный помощник, чтобы не тянуть strings в инициализацию
// среза большого размера.
func fillBytes(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}
