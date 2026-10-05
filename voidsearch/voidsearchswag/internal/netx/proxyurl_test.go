package netx

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseProxySpecSchemes(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		host   string
		user   string
		pass   string
	}{
		{"socks5://1.2.3.4:1080", "socks5", "1.2.3.4:1080", "", ""},
		{"socks4://1.2.3.4:1080", "socks4", "1.2.3.4:1080", "", ""},
		{"http://1.2.3.4:8080", "http", "1.2.3.4:8080", "", ""},
		{"https://1.2.3.4:8443", "https", "1.2.3.4:8443", "", ""},
		{"socks5h://1.2.3.4:1080", "socks5h", "1.2.3.4:1080", "", ""},
		// Без схемы подразумевается socks5.
		{"1.2.3.4:1080", "socks5", "1.2.3.4:1080", "", ""},
		// Креды через @.
		{"socks5://bob:secret@1.2.3.4:1080", "socks5", "1.2.3.4:1080", "bob", "secret"},
		// Креды четырьмя частями: host:port:user:pass.
		{"1.2.3.4:1080:bob:secret", "socks5", "1.2.3.4:1080", "bob", "secret"},
		// Пробелы по краям отбрасываются.
		{"  socks5://1.2.3.4:1080  ", "socks5", "1.2.3.4:1080", "", ""},
	}
	for _, c := range cases {
		pi, err := parseProxySpec(c.in)
		if err != nil {
			t.Errorf("parseProxySpec(%q): %v", c.in, err)
			continue
		}
		if pi.scheme != c.scheme {
			t.Errorf("%q: scheme=%q, ожидала %q", c.in, pi.scheme, c.scheme)
		}
		if pi.host != c.host {
			t.Errorf("%q: host=%q, ожидала %q", c.in, pi.host, c.host)
		}
		if pi.user != c.user || pi.pass != c.pass {
			t.Errorf("%q: креды %q/%q, ожидала %q/%q", c.in, pi.user, pi.pass, c.user, c.pass)
		}
	}
}

func TestParseProxySpecErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"ftp://1.2.3.4:21",
		"socks5://без-порта",
		"gopher://1.2.3.4:70",
	} {
		if _, err := parseProxySpec(in); err == nil {
			t.Errorf("parseProxySpec(%q) принят без ошибки", in)
		}
	}
}

func TestParseProxySpecSchemeCaseInsensitive(t *testing.T) {
	pi, err := parseProxySpec("SOCKS5://1.2.3.4:1080")
	if err != nil {
		t.Fatal(err)
	}
	if pi.scheme != "socks5" {
		t.Errorf("схема %q не приведена к нижнему регистру", pi.scheme)
	}
}

func TestProxyInfoIsSocks(t *testing.T) {
	cases := map[string]bool{
		"socks5":  true,
		"socks4":  true,
		"http":    false,
		"https":   false,
		"socks5h": false,
	}
	for scheme, want := range cases {
		pi := proxyInfo{scheme: scheme}
		if got := pi.isSocks(); got != want {
			t.Errorf("isSocks(%q)=%v, ожидала %v", scheme, got, want)
		}
	}
}

func TestParseProxyURL(t *testing.T) {
	u, err := parseProxyURL("socks5://bob:secret@1.2.3.4:1080")
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "socks5" {
		t.Errorf("scheme=%q", u.Scheme)
	}
	if u.Host != "1.2.3.4:1080" {
		t.Errorf("host=%q", u.Host)
	}
	if u.User.Username() != "bob" {
		t.Errorf("user=%q", u.User.Username())
	}
	pw, _ := u.User.Password()
	if pw != "secret" {
		t.Errorf("pass=%q", pw)
	}
}

func TestParseProxyURLWithoutCreds(t *testing.T) {
	u, err := parseProxyURL("http://1.2.3.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	if u.User != nil {
		t.Errorf("креды появились на пустом месте: %v", u.User)
	}
}

func TestParseProxyURLError(t *testing.T) {
	if _, err := parseProxyURL("ftp://1.2.3.4:21"); err == nil {
		t.Error("плохая схема принята")
	}
}

func TestEncodeSocksAddrIPv4(t *testing.T) {
	got := encodeSocksAddr("1.2.3.4")
	want := []byte{0x01, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("длина %d, ожидала %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("atyp IPv4 неверен: %v", got)
		}
	}
}

func TestEncodeSocksAddrIPv6(t *testing.T) {
	got := encodeSocksAddr("::1")
	if got[0] != 0x04 {
		t.Errorf("atyp=%d, ожидала 0x04", got[0])
	}
	if len(got) != 17 {
		t.Errorf("длина IPv6 %d, ожидала 17", len(got))
	}
}

func TestEncodeSocksAddrDomain(t *testing.T) {
	got := encodeSocksAddr("example.com")
	if got[0] != 0x03 {
		t.Fatalf("atyp=%d, ожидала 0x03", got[0])
	}
	if int(got[1]) != len("example.com") {
		t.Errorf("длина домена %d", got[1])
	}
	if string(got[2:]) != "example.com" {
		t.Errorf("домен %q", got[2:])
	}
}

func TestEncodeSocksAddrLongDomainTruncated(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := encodeSocksAddr(long)
	if int(got[1]) != 255 {
		t.Errorf("длина не обрезана до 255: %d", got[1])
	}
}

func TestSocksErrText(t *testing.T) {
	cases := map[byte]string{
		0x01: "запрещено политикой",
		0x02: "общий отказ",
		0x03: "сеть недоступна",
		0x04: "хост недоступен",
		0x05: "connection refused",
		0x06: "TTL истёк",
		0x07: "команда не поддерживается",
		0x08: "тип адреса не поддерживается",
	}
	for code, want := range cases {
		if got := socksErr(code); got != want {
			t.Errorf("socksErr(0x%02x)=%q, ожидала %q", code, got, want)
		}
	}
	if !strings.Contains(socksErr(0x77), "0x77") {
		t.Errorf("неизвестный код не показан: %q", socksErr(0x77))
	}
}

func TestSkipBoundAddrIPv4(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		// 4 байта адреса + 2 порта.
		server.Write([]byte{1, 2, 3, 4, 0x1f, 0x90})
	}()
	if err := skipBoundAddr(client, 0x01); err != nil {
		t.Errorf("IPv4 bound addr: %v", err)
	}
}

func TestSkipBoundAddrDomain(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write([]byte{3}) // длина
		server.Write([]byte("abc"))
		server.Write([]byte{0, 80})
	}()
	if err := skipBoundAddr(client, 0x03); err != nil {
		t.Errorf("домен bound addr: %v", err)
	}
}

func TestSkipBoundAddrIPv6(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write(make([]byte, 18)) // 16 адреса + 2 порта
	}()
	if err := skipBoundAddr(client, 0x04); err != nil {
		t.Errorf("IPv6 bound addr: %v", err)
	}
}

func TestSkipBoundAddrUnknownType(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := skipBoundAddr(client, 0x09); err == nil {
		t.Error("неизвестный тип адреса принят")
	}
}

func TestSocks5RoundTripNoAuth(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		hello := make([]byte, 3)
		readFull(server, hello)
		if hello[0] != 0x05 {
			t.Errorf("версия приветствия %d", hello[0])
		}
		server.Write([]byte{0x05, 0x00}) // метод: без авторизации
		// Запрос CONNECT: 3 байта шапки + atyp+длина+домен (13) + 2 порта.
		readFull(server, make([]byte, 18))
		server.Write([]byte{0x05, 0x00, 0x00, 0x01, 1, 2, 3, 4, 0x1f, 0x90})
	}()

	if err := socks5RoundTrip(client, "example.com:443", "", ""); err != nil {
		t.Errorf("socks5 без авторизации: %v", err)
	}
}

func TestSocks5RoundTripWithAuth(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		hello := make([]byte, 4) // с авторизацией предложено два метода
		readFull(server, hello)
		server.Write([]byte{0x05, 0x02}) // выбран логин-пароль
		// 1 версия + 1 длина логина + 3 логин + 1 длина пароля + 6 пароль.
		readFull(server, make([]byte, 12))
		server.Write([]byte{0x01, 0x00}) // авторизация принята
		readFull(server, make([]byte, 18))
		server.Write([]byte{0x05, 0x00, 0x00, 0x01, 1, 2, 3, 4, 0x1f, 0x90})
	}()

	if err := socks5RoundTrip(client, "example.com:443", "bob", "secret"); err != nil {
		t.Errorf("socks5 с авторизацией: %v", err)
	}
}

func TestSocks5RoundTripRejectsNoMethod(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		readFull(server, make([]byte, 3))
		server.Write([]byte{0x05, 0xFF}) // нет подходящего метода
	}()

	if err := socks5RoundTrip(client, "example.com:443", "", ""); err == nil {
		t.Error("отказ в методе не распознан")
	}
}

func TestSocks5RoundTripRejectsBadVersion(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		readFull(server, make([]byte, 3))
		server.Write([]byte{0x04, 0x00}) // чужая версия
	}()

	if err := socks5RoundTrip(client, "example.com:443", "", ""); err == nil {
		t.Error("чужая версия принята")
	}
}

func TestSocks5AuthRejected(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		// net.Pipe синхронный: читаем запрос целиком, только потом отвечаем.
		buf := make([]byte, 64)
		server.Read(buf)
		server.Write([]byte{0x01, 0x01}) // авторизация отклонена
	}()

	if err := socks5Auth(client, "bob", "wrong"); err == nil {
		t.Error("отклонённая авторизация принята")
	}
}

func TestSocks5AuthCredsTooLong(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	if err := socks5Auth(client, strings.Repeat("a", 256), "x"); err == nil {
		t.Error("слишком длинные креды приняты")
	}
}

func TestSocks5ConnectRejectsBadPort(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := socks5Connect(client, "example.com:0"); err == nil {
		t.Error("нулевой порт принят")
	}
	if err := socks5Connect(client, "example.com:70000"); err == nil {
		t.Error("порт вне диапазона принят")
	}
}

func TestSocks5ConnectRejectsServerError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		readFull(server, make([]byte, 18))
		server.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // хост недоступен
	}()

	err := socks5Connect(client, "example.com:443")
	if err == nil {
		t.Fatal("ошибка сервера не распознана")
	}
	if !strings.Contains(err.Error(), "хост недоступен") {
		t.Errorf("текст ошибки: %v", err)
	}
}

func TestSocks4ConnectSuccess(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		buf := make([]byte, 64)
		readFull(server, buf[:9]) // 4 заголовка + 4 адреса + нулевой байт юзера
		server.Write([]byte{0x00, 0x5a, 0, 0, 0, 0, 0, 0})
	}()

	if err := socks4Connect(client, "1.2.3.4:443", ""); err != nil {
		t.Errorf("socks4: %v", err)
	}
}

func TestSocks4ConnectRejectsIPv6(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := socks4Connect(client, "[::1]:443", ""); err == nil {
		t.Error("IPv6 в socks4 принят")
	}
}

func TestSocks4ConnectRejectsBadPort(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := socks4Connect(client, "1.2.3.4:0", ""); err == nil {
		t.Error("нулевой порт принят")
	}
}

func TestSocksDialContext(t *testing.T) {
	// Настоящее соединение не поднимаем: проверяем, что диалаер создан и
	// возвращает ошибку вместо паники на закрытом адресе.
	pi := proxyInfo{scheme: "socks5", host: "127.0.0.1:1"}
	dial := socksDial(pi, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dial(ctx, "tcp", "example.com:443"); err == nil {
		t.Error("соединение на закрытый порт прошло")
	}
}

func TestFingerprintByName(t *testing.T) {
	fp, ok := FingerprintByName("chrome_131_win")
	if !ok {
		t.Fatal("известный отпечаток не найден")
	}
	if fp.Name != "chrome_131_win" {
		t.Errorf("имя %q", fp.Name)
	}
	if fp.UserAgent == "" {
		t.Error("User-Agent пуст")
	}
	if fp.Accept == "" {
		t.Error("Accept пуст")
	}
	if len(fp.HeaderOrder) == 0 {
		t.Error("порядок заголовков пуст")
	}
}

func TestFingerprintByNameUnknown(t *testing.T) {
	if _, ok := FingerprintByName("netscape_0"); ok {
		t.Error("неизвестный отпечаток найден")
	}
}

func TestAllFingerprintsHaveTLSProfile(t *testing.T) {
	// Отпечаток без TLS-профиля означает отказ построения сессии: это
	// ловится только на живом запросе, поэтому проверяется заранее.
	for _, f := range Fingerprints {
		if f.Name == "" {
			t.Error("отпечаток без имени")
		}
		if f.UserAgent == "" {
			t.Errorf("%s: без User-Agent", f.Name)
		}
		if _, ok := f.TLSProfile(); !ok {
			t.Errorf("%s: нет TLS-профиля для ключа %q", f.Name, f.TLSKey)
		}
	}
}

func TestFingerprintNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fingerprints {
		if seen[f.Name] {
			t.Errorf("отпечаток %q объявлен дважды", f.Name)
		}
		seen[f.Name] = true
	}
}

func TestMaskProxy(t *testing.T) {
	cases := map[string]string{
		"":                                 "direct",
		"socks5://1.2.3.4:1080":            "socks5://1.2.3.4:1080",
		"socks5://bob:secret@1.2.3.4:1080": "socks5://1.2.3.4:1080",
		"1.2.3.4:1080":                     "proxy",
		"http://user@host:80":              "http://host:80",
	}
	for in, want := range cases {
		if got := maskProxy(in); got != want {
			t.Errorf("maskProxy(%q)=%q, ожидала %q", in, got, want)
		}
	}
}

func TestMaskSpecHidesCreds(t *testing.T) {
	got := MaskSpec("socks5://bob:secret@1.2.3.4:1080")
	if strings.Contains(got, "secret") {
		t.Errorf("пароль не скрыт: %q", got)
	}
	if strings.Contains(got, "bob") {
		t.Errorf("логин не скрыт: %q", got)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList("a,b;c\nd\te f")
	want := []string{"a", "b", "c", "d", "e", "f"}
	if len(got) != len(want) {
		t.Fatalf("частей %d, ожидала %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("часть %d: %q, ожидала %q", i, got[i], want[i])
		}
	}
}

func TestSplitListEmpty(t *testing.T) {
	if got := splitList(""); len(got) != 0 {
		t.Errorf("на пустой строке %v", got)
	}
	if got := splitList(",,;  \n"); len(got) != 0 {
		t.Errorf("на одних разделителях %v", got)
	}
}

func TestOrHelpers(t *testing.T) {
	if orStr("", "def") != "def" {
		t.Error("orStr не подставил дефолт")
	}
	if orStr("  ", "def") != "def" {
		t.Error("orStr не распознал пробелы")
	}
	if orStr("set", "def") != "set" {
		t.Error("orStr затёр значение")
	}
	if orInt(0, 5) != 5 {
		t.Error("orInt не подставил дефолт")
	}
	if orInt(3, 5) != 3 {
		t.Error("orInt затёр значение")
	}
	if orDur(0, time.Second) != time.Second {
		t.Error("orDur не подставил дефолт")
	}
	if orDur(time.Minute, time.Second) != time.Minute {
		t.Error("orDur затёр значение")
	}
}

func TestClip(t *testing.T) {
	if got := clip("  много   пробелов  ", 50); got != "много пробелов" {
		t.Errorf("пробелы не свернуты: %q", got)
	}
	if got := clip("abcdef", 3); got != "abc" {
		t.Errorf("обрезка: %q", got)
	}
	// Обрезка по рунам, а не по байтам: иначе кириллица рвётся пополам.
	if got := clip("привет", 3); got != "при" {
		t.Errorf("обрезка кириллицы: %q", got)
	}
	if got := clip("", 10); got != "" {
		t.Errorf("пустая строка -> %q", got)
	}
}

func TestClipReasonLength(t *testing.T) {
	long := strings.Repeat("я", 200)
	got := clipReason(long)
	if len([]rune(got)) != 70 {
		t.Errorf("длина причины %d, ожидала 70", len([]rune(got)))
	}
}

func TestDefaultPaths(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", "/tmp/vss-test")
	if got := DefaultDataDir(); got != "/tmp/vss-test" {
		t.Errorf("DefaultDataDir=%q", got)
	}
	if !strings.Contains(VendorDir(), "vendor") {
		t.Errorf("VendorDir=%q", VendorDir())
	}
	if !strings.Contains(DefaultTorDataDir(), "tor-data") {
		t.Errorf("DefaultTorDataDir=%q", DefaultTorDataDir())
	}
	if !strings.HasSuffix(DefaultStatePath(), "proxy-pool.json") {
		t.Errorf("DefaultStatePath=%q", DefaultStatePath())
	}
	if !strings.HasSuffix(DefaultDBPath(), "voidsearchswag.db") {
		t.Errorf("DefaultDBPath=%q", DefaultDBPath())
	}
}

func TestDefaultDataDirFallsBackToHome(t *testing.T) {
	t.Setenv("VOIDSEARCH_DATA_DIR", "")
	got := DefaultDataDir()
	if got == "" {
		t.Fatal("каталог данных пуст")
	}
	if !strings.Contains(got, "voidsearchswag") {
		t.Errorf("каталог данных не содержит имени проекта: %q", got)
	}
}

func TestTorBinaryName(t *testing.T) {
	name := TorBinaryName()
	if name != "tor" && name != "tor.exe" {
		t.Errorf("неожиданное имя бинаря: %q", name)
	}
}
