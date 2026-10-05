package discover

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Onion-адреса — base32: алфавит a-z и 2-7, цифры 0,1,8,9 недопустимы.
// Тестовые адреса обязаны быть валидными, иначе регулярка верно их отсеет.
const (
	v2a = "abcdefghijklmnop"
	v2b = "qrstuvwxyz234567"
	v2c = "bcdefghijklmnopq"
	v3a = "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx"
	v3b = "bcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwxy"
	ahm = "juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd"
)

func TestHostOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://" + v2a + ".onion/path", v2a + ".onion"},
		{"https://" + v2a + ".onion", v2a + ".onion"},
		{v2a + ".onion", v2a + ".onion"},
		{"http://" + v3a + ".onion/page", v3a + ".onion"},
		{"http://" + ahm + ".onion/search", ahm + ".onion"},
		{"http://example.com/" + v2a + ".onion", ""},
		{"https://google.com", ""},
		{"", ""},
		{"http://" + v2a + ".onion:8080/x", v2a + ".onion"},
		{"not an address", ""},
		{"http://aaa111bbb222cccc.onion", ""},
		{"http://" + v2a + ".com", ""},
		{"http://" + v2b + ".onion", v2b + ".onion"},
	}
	for _, c := range cases {
		if got := HostOf(c.in); got != c.want {
			t.Errorf("HostOf(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestHostOfUppercase(t *testing.T) {
	got := HostOf("http://ABCDEFGHIJKLMNOP.ONION/x")
	want := "abcdefghijklmnop.onion"
	if got != want {
		t.Errorf("HostOf вернул %q, ожидала %q (регистр не приведён)", got, want)
	}
}

func TestExtractFindsOnionsInText(t *testing.T) {
	body := `
<html><body>
<a href="http://` + v2a + `.onion/page">one</a>
<a href="https://` + v2b + `.onion">two</a>
bare text http://` + v2c + `.onion/inline end
<a href="/relative/` + v2c + `.onion">relative</a>
<a href="https://example.com">clearnet</a>
</body></html>`
	got := Extract(body)
	want := []string{v2a + ".onion", v2b + ".onion", v2c + ".onion"}
	if len(got) != len(want) {
		t.Fatalf("найдено %d (%v), ожидала %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("позиция %d = %q, ожидала %q", i, got[i], want[i])
		}
	}
}

func TestExtractFindsV3Address(t *testing.T) {
	body := `<a href="http://` + v3a + `.onion/x">v3</a>`
	got := Extract(body)
	if len(got) != 1 || got[0] != v3a+".onion" {
		t.Errorf("v3-адрес не найден: %v", got)
	}
}

func TestExtractFindsMultipleV3(t *testing.T) {
	body := `<a href="http://` + v3a + `.onion/a">a</a><a href="http://` + v3b + `.onion/b">b</a>`
	got := Extract(body)
	if len(got) != 2 {
		t.Errorf("найдено %d v3-адресов, ожидала 2: %v", len(got), got)
	}
}

func TestExtractDedupes(t *testing.T) {
	body := `<a href="http://` + v2a + `.onion/1"></a>
<a href="http://` + v2a + `.onion/2"></a>
<a href="http://` + v2a + `.onion/3"></a>`
	got := Extract(body)
	if len(got) != 1 {
		t.Errorf("дубликаты не схлопнуты: %v", got)
	}
}

func TestExtractIgnoresClearnet(t *testing.T) {
	body := `<a href="https://google.com">g</a><a href="http://example.org/x">e</a>`
	if got := Extract(body); len(got) != 0 {
		t.Errorf("clearnet попал в выдачу: %v", got)
	}
}

func TestExtractIgnoresInvalidBase32(t *testing.T) {
	body := `<a href="http://aaa111bbb222cccc.onion">bad digits</a>
<a href="http://aaa888bbb999cccc.onion">worse</a>`
	if got := Extract(body); len(got) != 0 {
		t.Errorf("невалидные base32 попали в выдачу: %v", got)
	}
}

func TestExtractFindsBareAddressWithoutHref(t *testing.T) {
	body := `Onion: ` + v2a + `.onion works fine`
	got := Extract(body)
	if len(got) != 1 || got[0] != v2a+".onion" {
		t.Errorf("голый адрес не найден: %v", got)
	}
}

func TestMergeDedupesAndKeepsFirst(t *testing.T) {
	in := []Candidate{
		{Address: v2a + ".onion", Source: "first"},
		{Address: v2b + ".onion", Source: "first"},
		{Address: v2a + ".onion", Source: "second"},
	}
	out := Merge(in)
	if len(out) != 2 {
		t.Fatalf("осталось %d, ожидала 2", len(out))
	}
	if out[0].Source != "first" {
		t.Errorf("сохранён не первый источник: %q", out[0].Source)
	}
}

func TestMergeDropsInvalid(t *testing.T) {
	in := []Candidate{
		{Address: v2a + ".onion"},
		{Address: ""},
		{Address: "example.com"},
		{Address: "aaa111bbb222cccc.onion"},
	}
	out := Merge(in)
	if len(out) != 1 {
		t.Errorf("невалидные не отсеяны: %+v", out)
	}
}

func TestMergePreservesOrder(t *testing.T) {
	in := []Candidate{
		{Address: v2b + ".onion"},
		{Address: v2a + ".onion"},
		{Address: v2c + ".onion"},
	}
	out := Merge(in)
	want := []string{v2b + ".onion", v2a + ".onion", v2c + ".onion"}
	for i := range want {
		if out[i].Address != want[i] {
			t.Errorf("порядок нарушен на %d: %q", i, out[i].Address)
		}
	}
}

func TestRateLimiterSpacesRequests(t *testing.T) {
	rl := NewRateLimiter(60 * time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := rl.Wait(ctx, v2a+".onion"); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 120*time.Millisecond {
		t.Errorf("три запроса прошли за %v, пауза не соблюдена", elapsed)
	}
}

func TestRateLimiterSeparatesHosts(t *testing.T) {
	rl := NewRateLimiter(300 * time.Millisecond)
	ctx := context.Background()

	if err := rl.Wait(ctx, v2a+".onion"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rl.Wait(ctx, v2b+".onion"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("разные хосты ждут друг друга (%v) - лимит общий вместо per-host", el)
	}
}

func TestRateLimiterRespectsCancel(t *testing.T) {
	rl := NewRateLimiter(5 * time.Second)
	ctx := context.Background()

	if err := rl.Wait(ctx, v2a+".onion"); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	start := time.Now()
	err := rl.Wait(cctx, v2a+".onion")
	if err == nil {
		t.Error("отменённый контекст не прервал ожидание")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("ожидание не прервано, прошло %v", el)
	}
}

func TestRateLimiterDefaultDelay(t *testing.T) {
	rl := NewRateLimiter(0)
	if rl.Delay() != 2*time.Second {
		t.Errorf("нулевая задержка не заменена дефолтом: %v", rl.Delay())
	}
}

func TestCrawlConfigDefaults(t *testing.T) {
	c := CrawlConfig{}.withDefaults()
	if c.Depth != 2 {
		t.Errorf("depth=%d, ожидала 2", c.Depth)
	}
	if c.Concurrency != 4 {
		t.Errorf("concurrency=%d, ожидала 4", c.Concurrency)
	}
	if c.MaxHosts != 50 {
		t.Errorf("max_hosts=%d, ожидала 50", c.MaxHosts)
	}
	if c.PerHostDelay != 2*time.Second {
		t.Errorf("per_host_delay=%v, ожидала 2s", c.PerHostDelay)
	}
	if c.PageTimeout != 45*time.Second {
		t.Errorf("page_timeout=%v, ожидала 45s", c.PageTimeout)
	}
}

func TestCrawlNilClientReportsError(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := c.Crawl(context.Background(), []string{v2a + ".onion"})
	if rep.Failed == 0 {
		t.Error("отсутствие клиента не отражено в отчёте")
	}
	if rep.Pages != 1 {
		t.Errorf("pages=%d, ожидала 1", rep.Pages)
	}
	if rep.Ok != 0 {
		t.Errorf("ok=%d, ожидала 0", rep.Ok)
	}
}

func TestCrawlCancelledContext(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, rep := c.Crawl(ctx, []string{v2a + ".onion"})
	// Этап 172: отмена - отдельное поле, а не значение limit_hit: слепой
	// прогон этапа 171 на max_hosts=1000 потерял событие потолка, когда
	// отмена затёрла его строкой «контекст отменён».
	if !rep.Cancelled {
		t.Error("отмена контекста не отражена полем cancelled")
	}
	if rep.LimitHit != "" {
		t.Errorf("отмена - не предел, а limit_hit=%q", rep.LimitHit)
	}
}

func TestCrawlSkipsInvalidSeeds(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := c.Crawl(context.Background(), []string{
		"", "not-onion.com", "example.org", "aaa111bbb222cccc.onion",
	})
	if rep.Pages != 0 {
		t.Errorf("невалидные сиды обошлись: pages=%d", rep.Pages)
	}
}

func TestCrawlMaxHostsLimits(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{MaxHosts: 2})
	_, rep := c.Crawl(context.Background(), []string{
		v2a + ".onion", v2b + ".onion", v2c + ".onion", v3a + ".onion",
	})
	if rep.Pages > 2 {
		t.Errorf("обошли %d хостов при потолке 2", rep.Pages)
	}
	if rep.LimitHit == "" {
		t.Error("потолок хостов не отражён в отчёте")
	}
}

func TestCrawlRespectsDepth(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{Depth: 1, MaxHosts: 10})
	_, rep := c.Crawl(context.Background(), []string{v2a + ".onion", v2b + ".onion"})
	if rep.Depth != 1 {
		t.Errorf("глубина в отчёте %d, ожидала 1", rep.Depth)
	}
	if rep.Pages != 2 {
		t.Errorf("pages=%d, ожидала 2 (только сиды)", rep.Pages)
	}
}

func TestCrawlNewHostsSorted(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := c.Crawl(context.Background(), []string{v2a + ".onion"})
	for i := 1; i < len(rep.NewHosts); i++ {
		if rep.NewHosts[i-1] > rep.NewHosts[i] {
			t.Errorf("список хостов не отсортирован: %v", rep.NewHosts)
			break
		}
	}
}

func TestCrawlEmptySeeds(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := c.Crawl(context.Background(), nil)
	if rep.Pages != 0 || rep.Found != 0 {
		t.Errorf("на пустых сидах что-то нашлось: %+v", rep)
	}
}

func TestCrawlDedupesSeedAddresses(t *testing.T) {
	c := NewCrawler(nil, nil, CrawlConfig{})
	_, rep := c.Crawl(context.Background(), []string{v2a + ".onion", v2a + ".onion"})
	if rep.Pages != 1 {
		t.Errorf("дублирующийся сид обошли %d раз, ожидала 1", rep.Pages)
	}
}

func TestCleanTitleStripsTrailingAddress(t *testing.T) {
	raw := "Drug Hub http://drughub666py6fgnml5kmxa7fva5noppkf6wkai4fwwvzwt4rz645aqd.onion"
	got := cleanTitle("recon222tttn4ob7ujdhbn3s4gjre7netvzybuvbq2bcqwltkiqinhad.onion", raw)
	if got != "Drug Hub" {
		t.Errorf("cleanTitle = %q, ожидала Drug Hub", got)
	}
}

func TestCleanTitleKeepsPlainName(t *testing.T) {
	got := cleanTitle(v2a+".onion", "Hidden Market")
	if got != "Hidden Market" {
		t.Errorf("cleanTitle = %q", got)
	}
}

func TestCleanTitleDropsSelfReference(t *testing.T) {
	addr := v2a + ".onion"
	cases := []string{
		addr,
		"http://" + addr,
		"https://" + addr,
		"http://" + addr + "/",
		"HTTPS://" + strings.ToUpper(addr),
		v2a,
	}
	for _, c := range cases {
		if got := cleanTitle(addr, c); got != "" {
			t.Errorf("подпись %q не отброшена, вернулось %q", c, got)
		}
	}
}

func TestCleanTitleEmpty(t *testing.T) {
	if got := cleanTitle(v2a+".onion", "   "); got != "" {
		t.Errorf("пустая подпись дала %q", got)
	}
}

func TestCleanTitleSingleChar(t *testing.T) {
	if got := cleanTitle(v2a+".onion", "x"); got != "" {
		t.Errorf("односимвольная подпись не отброшена: %q", got)
	}
}

func TestCleanTitleTruncates(t *testing.T) {
	got := cleanTitle(v2a+".onion", strings.Repeat("word ", 100))
	if len([]rune(got)) > 160 {
		t.Errorf("подпись не обрезана: %d символов", len([]rune(got)))
	}
}

func TestCleanTitleNormalizesWhitespace(t *testing.T) {
	got := cleanTitle(v2a+".onion", "  Many    spaces  here  ")
	if got != "Many spaces here" {
		t.Errorf("пробелы не схлопнуты: %q", got)
	}
}

func TestExtractFromSourceTakesAnchorText(t *testing.T) {
	f := NewFinder(nil, nil)
	body := `<ul><li><a href="http://` + v2a + `.onion/">First Market</a></li>
<li><a href="http://` + v2b + `.onion/">Second Forum</a></li></ul>`
	got := f.extractFromSource(body, source{name: "s", selector: "li a"})
	if len(got) != 2 {
		t.Fatalf("найдено %d, ожидала 2", len(got))
	}
	if got[0].Title != "First Market" {
		t.Errorf("заголовок %q, ожидала First Market", got[0].Title)
	}
	if got[1].Title != "Second Forum" {
		t.Errorf("заголовок %q", got[1].Title)
	}
}

func TestExtractFromSourceFallsBackWithoutTitles(t *testing.T) {
	f := NewFinder(nil, nil)
	body := `plain ` + v2a + `.onion <b>Some Label</b>`
	got := f.extractFromSource(body, source{name: "s", selector: ".nothing-matches"})
	if len(got) != 1 {
		t.Fatalf("fallback не сработал: %d", len(got))
	}
	if got[0].Address != v2a+".onion" {
		t.Errorf("адрес %q", got[0].Address)
	}
	if got[0].Title != "" {
		t.Errorf("голому адресу приписана чужая подпись: %q", got[0].Title)
	}
}

func TestExtractFromSourceEmptySelectorFallsBack(t *testing.T) {
	f := NewFinder(nil, nil)
	body := `<a href="http://` + v2a + `.onion/">x</a>`
	got := f.extractFromSource(body, source{name: "s"})
	if len(got) != 1 {
		t.Errorf("источник без селектора не отработал: %d", len(got))
	}
}

func TestCleanTitleDropsUrlTailCompletely(t *testing.T) {
	raw := "ProPublica Offline http://p53lf57qovyuvwsc6xnrppyply3vtqm7l6pcobkmyqsiofyeznfu5uqd.onion Protonmail"
	got := cleanTitle("pornhubvybmsymdol4iibwgwtkpwmeyd6luq2gxajgjzfjvotyt5zhyd.onion", raw)
	if got != "ProPublica Offline" {
		t.Errorf("cleanTitle = %q, ожидала ProPublica Offline", got)
	}
}

func TestCleanTitleBareOnionWordDropped(t *testing.T) {
	got := cleanTitle(v2a+".onion", "abcdefghijklmnop.onion")
	if got != "" {
		t.Errorf("голый адрес в подписи не отброшен: %q", got)
	}
}

func TestCleanTitleKeepsNameWithSpaces(t *testing.T) {
	got := cleanTitle(v2a+".onion", "Exploit.in Forum")
	if got != "Exploit.in Forum" {
		t.Errorf("cleanTitle = %q", got)
	}
}

func TestPlausibleTitle(t *testing.T) {
	good := []string{"Hidden Market", "Exploit.in Forum", "3xploit", "Dark Fail", "Time & Space"}
	for _, g := range good {
		if !plausibleTitle(g) {
			t.Errorf("годное название %q отброшено", g)
		}
	}
	bad := []string{`/"`, `"`, `/tdw"`, "ab", "", "   ", `://"`, `"//`}
	for _, b := range bad {
		if plausibleTitle(b) {
			t.Errorf("обрывок разметки %q принят за название", b)
		}
	}
}

func TestCleanTitleDropsHtmlRemnants(t *testing.T) {
	cases := []string{`/"`, `"`, `/tdw"`, `href="/x`}
	for _, c := range cases {
		if got := cleanTitle(v2a+".onion", c); got != "" {
			t.Errorf("обрывок %q принят: %q", c, got)
		}
	}
}

func TestStripLatencyPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"4.0s An index of sites", "An index of sites"},
		{"7.8s HostMeNow is a provider", "HostMeNow is a provider"},
		{"12m long form", "long form"},
		{"3h hours form", "hours form"},
		{"No prefix here", "No prefix here"},
		{"s only", "s only"},
		{"4.0x keep", "4.0x keep"},
		{"5s", "5s"},
	}
	for _, c := range cases {
		if got := stripLatencyPrefix(c.in); got != c.want {
			t.Errorf("stripLatencyPrefix(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestCleanTitleStripsLatency(t *testing.T) {
	got := cleanTitle(v2a+".onion", "4.0s An index of clearnet sites")
	if got != "An index of clearnet sites" {
		t.Errorf("cleanTitle = %q", got)
	}
}

func TestUnescapeEntities(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HostMeNow&#039;s shop", "HostMeNow's shop"},
		{"a &amp; b", "a & b"},
		{"&quot;quoted&quot;", `"quoted"`},
		{"no entities", "no entities"},
	}
	for _, c := range cases {
		if got := unescapeEntities(c.in); got != c.want {
			t.Errorf("unescapeEntities(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestCleanTitleUnescapes(t *testing.T) {
	got := cleanTitle(v2a+".onion", "HostMeNow&#039;s provider")
	if strings.Contains(got, "&#039;") {
		t.Errorf("сущность не раскодирована: %q", got)
	}
}

func TestPageTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<html><head><title>Hello</title></head></html>", "Hello"},
		{"<TITLE>Upper</TITLE>", "Upper"},
		{"<html><title>  Много   пробелов  </title>", "Много пробелов"},
		{"<html>no title</html>", ""},
		{"<title>unclosed", "unclosed"},
		{"<html><TITLE>Mixed</title>", "Mixed"},
	}
	for _, c := range cases {
		if got := pageTitle(c.in); got != c.want {
			t.Errorf("pageTitle(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestPageTitleTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "x"
	}
	got := pageTitle("<title>" + long + "</title>")
	if len([]rune(got)) > 120 {
		t.Errorf("заголовок не обрезан: %d символов", len([]rune(got)))
	}
}

func TestNormalizeBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{v2a + ".onion", "http://" + v2a + ".onion"},
		{"http://" + v2a + ".onion", "http://" + v2a + ".onion"},
		{"http://" + v2a + ".onion/", "http://" + v2a + ".onion"},
		{"https://x.onion/a/", "https://x.onion/a"},
		{"  " + v2a + ".onion  ", "http://" + v2a + ".onion"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBase(c.in); got != c.want {
			t.Errorf("NormalizeBase(%q) = %q, ожидала %q", c.in, got, c.want)
		}
	}
}

func TestSourcesListed(t *testing.T) {
	s := Sources()
	if len(s) == 0 {
		t.Fatal("clearnet-источники не перечислены")
	}
	seen := map[string]bool{}
	for _, name := range s {
		if name == "" {
			t.Error("пустое имя источника")
		}
		if seen[name] {
			t.Errorf("источник %q продублирован", name)
		}
		seen[name] = true
	}
}

func TestDiscoverClearnetNilClient(t *testing.T) {
	f := NewFinder(nil, nil)
	_, reports := f.DiscoverClearnet(context.Background())
	if len(reports) != len(clearnetSources) {
		t.Errorf("отчётов %d, ожидала %d (по одному на источник)", len(reports), len(clearnetSources))
	}
	for _, r := range reports {
		if r.OK {
			t.Errorf("%s помечен успешным без клиента", r.Name)
		}
		if r.Error == "" {
			t.Errorf("%s без причины ошибки", r.Name)
		}
	}
}

func TestDiscoverReportsAreOrdered(t *testing.T) {
	f := NewFinder(nil, nil)
	_, reports := f.DiscoverClearnet(context.Background())
	for i, r := range reports {
		if r.Name != clearnetSources[i].name {
			t.Errorf("позиция %d = %q, ожидала %q (порядок отчёта нестабилен)",
				i, r.Name, clearnetSources[i].name)
		}
	}
}

func TestCandidateKeyIsHost(t *testing.T) {
	c := Candidate{Address: v2a + ".onion"}
	if c.Key() != v2a+".onion" {
		t.Errorf("ключ дедупа %q, ожидала хост", c.Key())
	}
}

func TestCandidateKeyIgnoresSource(t *testing.T) {
	a := Candidate{Address: v2a + ".onion", Source: "one"}
	b := Candidate{Address: v2a + ".onion", Source: "two"}
	if a.Key() != b.Key() {
		t.Error("один адрес из разных источников даёт разные ключи")
	}
}
