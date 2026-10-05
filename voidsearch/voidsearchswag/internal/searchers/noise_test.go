package searchers

import (
	"strings"
	"testing"
)

const torchResultHTML = `
<html><body>
<nav class="navbar"><ul class="nav-links">
  <li><a href="/">Home</a></li>
  <li><a href="http://tordexu73joywapk2txdr54jed4imqledpcvcuf75qsas2gwdgksvnyd.onion/advertising/?site=torch">Advertising</a></li>
  <li><a href="/about">About</a></li>
</ul></nav>
<div class="results">
  <article class="result">
    <h2 class="result-title"><a href="http://realdump1abcdefghijklmnopqrstuvwxyz234567abcdefghij.onion/leak">Real Result One</a></h2>
    <p class="result-url"><a href="http://realdump1abcdefghijklmnopqrstuvwxyz234567abcdefghij.onion/leak">http://realdump1...</a></p>
  </article>
  <article class="result">
    <h2 class="result-title"><a href="http://realdump2abcdefghijklmnopqrstuvwxyz234567abcdefghij.onion/data">Real Result Two</a></h2>
  </article>
</div>
<footer><a href="/privacy">Privacy Policy</a><a href="/terms">Terms</a></footer>
</body></html>`

func TestParseOnionSelectorSkipsNavigation(t *testing.T) {
	res, err := parseOnion([]byte(torchResultHTML), "torch", "article.result h2.result-title a")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("результатов %d, ожидала 2: %+v", len(res), res)
	}
	for _, r := range res {
		if strings.Contains(r.Title, "Advertising") || strings.Contains(r.Title, "Home") ||
			strings.Contains(r.Title, "About") || strings.Contains(r.Title, "Privacy") {
			t.Errorf("навигация просочилась в результаты: %q", r.Title)
		}
		if !strings.Contains(r.URL, "/leak") && !strings.Contains(r.URL, "/data") {
			t.Errorf("не тот URL: %s", r.URL)
		}
	}
}

func TestParseOnionGenericFiltersNoise(t *testing.T) {
	res, err := parseOnion([]byte(torchResultHTML), "torch", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range res {
		low := strings.ToLower(r.Title)
		if low == "about" || low == "home" || low == "advertising" ||
			low == "privacy policy" || low == "terms" {
			t.Errorf("шум просочился: %q", r.Title)
		}
	}
	if len(res) == 0 {
		t.Fatal("generic-ветка не нашла реальных результатов")
	}
}

func TestEngineNoiseFiltersServicePages(t *testing.T) {
	html := `<html><body>
<div class="sresults">
  <b><a href="http://real1abcdefghijklmnopqrstuvwxyz234567abcdefghijklmn.onion/">Real Site</a></b>
  <div><a href="/serviceinfo/?service=http&dom=real1.onion">i</a></div>
</div>
</body></html>`
	res, err := parseOnion([]byte(html), "tor66", "div.sresults b a")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if strings.Contains(r.URL, "/serviceinfo/") {
			t.Errorf("служебная страница в результатах: %s", r.URL)
		}
	}
}

func TestEngineNoiseNavByParentClass(t *testing.T) {
	html := `<html><body>
<div class="site-header"><a href="http://navabcdefghijklmnopqrstuvwxyz234567abcdefghijklmnop.onion/x">Nav Link</a></div>
<div class="item"><a href="http://goodabcdefghijklmnopqrstuvwxyz234567abcdefghijklmno.onion/y">Good Link</a></div>
</body></html>`
	res, err := parseOnion([]byte(html), "test", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if strings.Contains(r.Title, "Nav Link") {
			t.Errorf("навигационная ссылка не отфильтрована: %q", r.Title)
		}
	}
}

func TestDropEngineNoiseRemovesSelfLinks(t *testing.T) {
	in := []Result{
		{URL: "http://torchdeedp3i2jigzjdmfpn5ttjhthh5wbmda2rr3jvqjg5p77c54dqd.onion/search?query=x", Title: "self"},
		{URL: "http://realdump1abcdefghijklmnopqrstuvwxyz234567abcdefghij.onion/leak", Title: "real"},
	}
	out := dropEngineNoise(in, "torch")
	if len(out) != 1 {
		t.Fatalf("осталось %d, ожидала 1", len(out))
	}
	if out[0].Title != "real" {
		t.Errorf("оставлен не тот: %+v", out[0])
	}
}

func TestDropEngineNoiseKeepsInputIntact(t *testing.T) {
	in := []Result{
		{URL: "http://torchtest.onion/a", Title: "self"},
		{URL: "http://other.onion/b", Title: "other"},
	}
	_ = dropEngineNoise(in, "torch")
	if len(in) != 2 {
		t.Errorf("входной срез мутирован: длина %d", len(in))
	}
}

func TestTorchSeedUsesQueryParam(t *testing.T) {
	for _, s := range DefaultSeeds() {
		if s.Name == "torch" {
			if !strings.Contains(s.Path, "query={q}") {
				t.Errorf("torch должен использовать query={q}, а не %q", s.Path)
			}
			if strings.Contains(s.Path, "?q=") {
				t.Error("torch с ?q= отдаёт главную страницу вместо результатов")
			}
		}
		if s.Name == "tornet" && !strings.Contains(s.Path, "?q={q}") {
			t.Errorf("tornet должен использовать ?q={q}, а не %q", s.Path)
		}
		if s.Name == "tor66" && !strings.Contains(s.Path, "?q={q}") {
			t.Errorf("tor66 должен использовать ?q={q}, а не %q", s.Path)
		}
	}
}

func TestSeedsHaveSelectors(t *testing.T) {
	for _, s := range DefaultSeeds() {
		if s.Selector == "" {
			t.Errorf("сид %q без селектора - упадёт в generic и потянет навигацию", s.Name)
		}
		if !strings.Contains(s.Path, "{q}") {
			t.Errorf("сид %q без плейсхолдера {q}", s.Name)
		}
	}
}
