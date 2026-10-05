package classifier

import "testing"

func TestPublicDefault(t *testing.T) {
	v := Classify("https://example.com/page", "Заголовок", "Какой-то текст сниппета подлиннее для веса", "")
	if v.Type != TypePublic {
		t.Errorf("type=%q", v.Type)
	}
	if v.Reason == "" {
		t.Error("без причины вердикт не объяснить")
	}
}

func TestPrivateLoginForm(t *testing.T) {
	v := Classify("https://example.com/login", "Вход",
		"sign in to your account", `<form><input type="password" name="pw"></form>`)
	if v.Type != TypePrivate {
		t.Errorf("type=%q, ожидала private", v.Type)
	}
}

func TestSingleSignInLinkIsNotLogin(t *testing.T) {
	// Одна ссылка "sign in" в шапке публичной страницы - не форма входа.
	v := Classify("https://example.com/news", "Новости", "читать sign in далее", "")
	if v.Type == TypePrivate {
		t.Errorf("публичная страница помечена private: %+v", v)
	}
}

func TestPaidPaywall(t *testing.T) {
	v := Classify("https://news.example/article", "Статья", "subscribe to read full text", "")
	if v.Type != TypePaid {
		t.Errorf("type=%q, ожидала paid", v.Type)
	}
}

func TestScamCryptoDoubler(t *testing.T) {
	v := Classify("http://xyz.onion/invest", " contro", "crypto investment, гарантированная прибыль x2, удвоим депозит", "")
	if v.Type != TypeScam {
		t.Errorf("type=%q, ожидала scam", v.Type)
	}
	if v.Quality > 0.3 {
		t.Errorf("качество скама завышено: %v", v.Quality)
	}
}

func TestHonestCryptoNotScam(t *testing.T) {
	// Честная биржа тоже пишет "crypto", но без обещаний удвоения.
	v := Classify("https://exchange.example", "Биржа", "crypto exchange, комиссии и лимиты", "")
	if v.Type == TypeScam {
		t.Errorf("честный текст помечен скамом: %+v", v)
	}
}

func TestCaptchaLowersQuality(t *testing.T) {
	v := Classify("https://shop.example/", "Магазин", "Just a moment... captcha challenge", "")
	if v.Type != TypePublic {
		t.Errorf("captcha не меняет тип: %+v", v)
	}
	if v.Quality > 0.3 {
		t.Errorf("качество с защитой завышено: %v", v.Quality)
	}
}

func TestTrustedHostPopular(t *testing.T) {
	v := Classify("https://github.com/org/repo", "Repo", "длинное описание проекта с текстом", "")
	if v.Popularity < 0.9 {
		t.Errorf("популярность доверенного хоста занижена: %v", v.Popularity)
	}
}

func TestStubPageUnpopular(t *testing.T) {
	v := Classify("http://a.onion/", "", "x", "")
	if v.Popularity > 0.4 {
		t.Errorf("заглушка популярна: %v", v.Popularity)
	}
}
