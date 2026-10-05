package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/netx"
)

func cmdPoolcheck(args []string) {
	fs := flag.NewFlagSet("poolcheck", flag.ExitOnError)
	protocol := fs.String("protocol", "", "http|https|socks4|socks5 (пусто - значение из конфига)")
	country := fs.String("country", "", "ISO-код страны или all (пусто - значение из конфига)")
	size := fs.Int("size", 0, "целевой размер пула, не больше 10000 (0 - значение из конфига)")
	limit := fs.Int("limit", 0, "сколько адресов брать у провайдера, не больше 100000 (0 - значение из конфига)")
	skipVerify := fs.Bool("skip-verify", false, "не проверять живость (без флага - значение из конфига)")
	show := fs.Int("show", 10, "сколько живых показать (0 - не показывать)")
	parseFlags(fs, args)
	// Все три числа проверяются до пула и до сети. Без проверки неположительный
	// размер и неположительный лимит молча подменялись дефолтами самого пула, а не
	// флага: netx.orInt возвращает дефолт на любом неположительном значении. Замер
	// до правки на HEAD ea8bf4b, программный зонд на countingProvider:
	//
	//	size=-1     limit=-5      -> провайдеру limit=400, применено MaxLive=60
	//	size=0      limit=0       -> провайдеру limit=400, применено MaxLive=60
	//	size=40     limit=400     -> провайдеру limit=400, применено MaxLive=40
	//	size=100000 limit=1000000 -> провайдеру limit=1000000, MaxLive=100000
	//
	// Живой замер тем же бинарником: poolcheck --size -1 --limit 5 --skip-verify
	// --show 3 дал rc=0, три строки и пустой stderr; --limit -5 дал то же, хотя
	// провайдеру вместо пяти адресов уходило четыреста; --show -1 и --show 0 дали
	// rc=0 и ноль строк stdout без единого слова.
	sizeN := 0
	if *size != 0 {
		sizeN = validLimitCeiling("size", *size, maxFlagPoolSize)
	}
	limitN := 0
	if *limit != 0 {
		limitN = validLimitCeiling("limit", *limit, maxFlagFetchLimit)
	}
	// Ноль у --show законен: он означает «не показывать адреса». Отрицательное
	// значение давало тот же результат молча, а огромное печатало дубли, потому
	// что pool.Next ходит по кругу и не заканчивается вместе с пулом.
	showN := validRange("show", *show, 0, maxFlagLimit)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	// Ноль у размера и у выборки с этого этапа означает «значение из конфига», а
	// не «отказать». Дефолты флагов сорок и четыреста совпадали с envDefault
	// полей ProxyPoolSize и ProxyFetchLimit, команда не читала конфиг вовсе и
	// переменные окружения не действовали. Замер до правки на HEAD 360ca56,
	// копия боевой базы, тор выключен, транспорт direct, провайдеры недоступны:
	//
	//	VOIDSEARCH_PROXY_POOL_SIZE=7 VOIDSEARCH_PROXY_FETCH_LIMIT=9
	//	    poolcheck --skip-verify --show 0
	//	    rc=0, stdout пустой, stderr пустой
	//	poolcheck --skip-verify --show 0 --size 0 --limit 0
	//	    rc=1, ERROR: --size должен быть положительным, получено 0
	//	poolcheck -h
	//	    -size int
	//	        целевой размер пула, не больше 10000 (default 40)
	//	    -limit int
	//	        сколько адресов брать у провайдера, не больше 100000 (default 400)
	//
	// Команда не печатала ни одного слова при удачном прогоне, поэтому применённый
	// размер нельзя было увидеть даже в отладке.
	if *size == 0 {
		sizeN = cfg.ProxyPoolSize
	}
	if *limit == 0 {
		limitN = cfg.ProxyFetchLimit
	}
	// Строковые флаги и --skip-verify с этого этапа читают конфиг тем же
	// правилом. До правки у --protocol и --country были дефолты «http» и «all»,
	// у --skip-verify - false, и все три значения уходили в пул мимо конфига:
	// VOIDSEARCH_PROXYSCRAPE_PROTOCOL, VOIDSEARCH_PROXYSCRAPE_COUNTRY и
	// VOIDSEARCH_PROXY_SKIP_VERIFY действовали на транспорт pool, но не на
	// команду, которая этот пул греет. Замер до правки на HEAD 7342776,
	// пересобранный бинарник, пустой каталог данных, тор выключен, транспорт
	// direct:
	//
	//	VOIDSEARCH_PROXYSCRAPE_PROTOCOL=socks5
	//	VOIDSEARCH_PROXYSCRAPE_COUNTRY=ru
	//	VOIDSEARCH_PROXY_SKIP_VERIFY=true
	//	VOIDSEARCH_PROXY_POOL_SIZE=7 VOIDSEARCH_PROXY_FETCH_LIMIT=9
	//	    poolcheck --show 0
	//	    rc=0, «пределы: размер пула 7, выборка 9, протокол http,
	//	                   страна all, проверка живости да»
	//	poolcheck -h
	//	    -country string
	//	        ISO-код страны или all (default "all")
	//	    -protocol string
	//	        http|https|socks4|socks5 (default "http")
	//
	// Числа к прошлому этапу конфиг уже читал, поэтому строка пределов честно
	// называла семь и девять, а рядом с ними - протокол и страну, которых
	// оператор не задавал. Запрос уходил на публичный API за http-адресами всех
	// стран, и каждый адрес грелся живой проверкой, хотя конфиг просил socks5 по
	// России и проверку не делать. Справка при этом показывала дефолты флагов
	// как истину, то есть прочитать правило из вывода было нельзя.
	//
	// Пустая строка в конфиге законна: netx.ProxyScrape подставляет «http» и
	// «all» сам через Effective. Поэтому третьим слоем стоит дефолт провайдера,
	// а не пустое место - строка пределов обязана называть тот протокол и ту
	// страну, которые действительно уйдут в запрос.
	log := stderrLogger{}
	// Значения флагов подкладываются в тот же netx.Config, из которого транспорт
	// pool строит поставщика. Слой конфига здесь уже на месте: NetxConfig
	// пробрасывает поле только когда оно задано и оставляет дефолт netx, поэтому
	// strFlagOrConfig видит вторым слоем уже применённый конфиг. Третий слой -
	// дефолт провайдера - защищает строку пределов от пустого места.
	nc := cfg.NetxConfig(log)
	def := netx.DefaultProxyScrape()
	nc.ProxyScrapeProtocol = strFlagOrConfig(*protocol, nc.ProxyScrapeProtocol, def.Protocol)
	nc.ProxyScrapeCountry = strFlagOrConfig(*country, nc.ProxyScrapeCountry, def.Country)
	// Булеву флагу не хватает нуля как «не задано»: false здесь рабочее значение
	// «проверять живость», поэтому отличить «оператор выключил проверку» от
	// «оператор флаг не трогал» можно только по списку разобранных флагов.
	nc.ProxySkipVerify = boolFlagOrConfig(fs, "skip-verify", *skipVerify, cfg.ProxySkipVerify)
	// Числа уже прошли проверку потолка и подмену нуля значением конфига.
	nc.ProxyPoolSize = sizeN
	nc.ProxyFetchLimit = limitN

	prov, pc := poolParams(nc)
	lines := poolParamsLines(prov, pc)
	fmt.Printf("пределы: %s\nпоставщик: %s\n", lines[0], lines[1])

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := warmProxyPool(ctx, log, nc)
	if err != nil {
		fatalf("пул: %v", err)
	}
	defer pool.SaveState()

	live, total, killed := pool.Stats()
	log.Infof("пул %s: живых %d из %d (выброшено %d)", pool.ProviderName(), live, total, killed)
	for i := 0; i < showN; i++ {
		lp, ok := pool.Next()
		if !ok {
			break
		}
		fmt.Printf("  %s  exit=%s  %dms\n", netx.MaskSpec(lp.Spec), lp.ExitIP, lp.Latency.Milliseconds())
	}
}

// poolParams собирает поставщика и параметры пула правилом транспорта pool:
// netx.ProviderForPool и netx.PoolConfigFrom. Замер до правки на HEAD e7d99c5,
// подставной провайдер на 127.0.0.1:18099 с журналом запросов, тор выключен,
// транспорт direct, VERBOSE=1, пустой каталог данных:
//
//	VOIDSEARCH_PROXY_PROVIDER_URL=http://127.0.0.1:18099/list
//	VOIDSEARCH_PROXYSCRAPE_PROTOCOL=socks5 VOIDSEARCH_PROXYSCRAPE_COUNTRY=ru
//	VOIDSEARCH_PROXYSCRAPE_ANONYMITY=elite VOIDSEARCH_PROXYSCRAPE_SSL=yes
//	VOIDSEARCH_PROXYSCRAPE_TIMEOUT_MS=4321
//	VOIDSEARCH_PROXY_PROBE_TIMEOUT=3s VOIDSEARCH_PROXY_PROBE_CONCURRENCY=5
//	VOIDSEARCH_PROXY_SKIP_VERIFY=true
//	VOIDSEARCH_PROXY_POOL_SIZE=2 VOIDSEARCH_PROXY_FETCH_LIMIT=4
//	    poolcheck --show 0
//	    rc=0 за 3.2 с, «пределы: размер пула 2, выборка 4, протокол socks5,
//	                   страна ru, проверка живости нет»
//	    «пул proxyscrape/socks5: живых 2 из 2 (выброшено 0)»
//	    журнал подставного провайдера: ПУСТО, ни одного запроса
//	    setup --warm-pool --skip-check --no-browser
//	    rc=0 за 2.3 с, «прогрев пула: протокол socks5, страна ru, размер 2,
//	                   выборка 4, проверка живости нет»
//	    журнал подставного провайдера: ПУСТО, ни одного запроса
//
// Прошлый этап научил обе команды читать протокол, страну и проверку живости, но
// warmProxyPool по-прежнему строил netx.ProxyScrape из двух строк, а PoolConfig -
// из четырёх чисел. Поэтому endpoint, анонимность, ssl, таймаут запроса, таймаут
// пробы, параллельность пробы, минимум живых и путь состояния из конфига в
// прогрев не попадали вовсе: запрос уходил на публичный api.proxyscrape.com даже
// при явно заданном ProxyProviderURL, а транспорт pool через netx.NewRotator всё
// это читал. Два пути к одному пулу расходились по восьми полям, и расхождение
// нигде не печаталось.
//
// Теперь CLI не собирает поставщика сам, а подкладывает значения флагов в тот же
// netx.Config, из которого транспорт строит пул, и зовёт те же две функции netx.
func poolParams(nc netx.Config) (netx.Provider, netx.PoolConfig) {
	prov := netx.ProviderForPool(nc)
	return prov, netx.PoolConfigFrom(nc, prov)
}

// poolParamsLines возвращает две строки описания прогрева: применённые пределы и
// поставщика с параметрами запроса. Общая для poolcheck, который печатает их в
// stdout, и для setup --warm-pool, который кладёт их в журнал: оба пути к одному
// пулу обязаны описывать его одинаково, иначе расхождение снова станет невидимым.
//
// Строка поставщика называет тот endpoint и те query-параметры, которые
// действительно уйдут наружу: Effective подставляет дефолты провайдера, поэтому
// пустое поле конфига печатается уже подставленным значением, а не пустым
// местом. У статического списка протокола и страны нет вовсе, и печатать их
// значило бы показать оператору параметры, которые ни на что не влияют.
func poolParamsLines(prov netx.Provider, pc netx.PoolConfig) [2]string {
	tail := fmt.Sprintf(", проба %s x %d, минимум живых %d", pc.ProbeTimeout, pc.Concurrency, pc.MinLive)
	limits := fmt.Sprintf("размер пула %d, выборка %d, проверка живости %s",
		pc.MaxLive, pc.FetchLimit, yesNo(pc.Verify))
	switch p := prov.(type) {
	case netx.ProxyScrape:
		e := p.Effective()
		limits = fmt.Sprintf("размер пула %d, выборка %d, протокол %s, страна %s, проверка живости %s",
			pc.MaxLive, pc.FetchLimit, e.Protocol, e.Country, yesNo(pc.Verify))
		return [2]string{limits, fmt.Sprintf("%s, endpoint %s, анонимность %s, ssl %s, таймаут запроса %d мс%s",
			prov.Name(), e.Endpoint, e.Anonymity, e.SSL, e.TimeoutMS, tail)}
	case netx.StaticProvider:
		return [2]string{limits, fmt.Sprintf("%s, адресов %d%s", prov.Name(), len(p), tail)}
	default:
		return [2]string{limits, prov.Name() + tail}
	}
}

// warmProxyPool тянет прокси у провайдера, проверяет живость и кладёт state на
// диск. Общая для poolcheck и setup --warm-pool.
func warmProxyPool(ctx context.Context, log netx.Logger, nc netx.Config) (*netx.ProxyPool, error) {
	_, pc := poolParams(nc)
	return netx.NewProxyPool(ctx, pc, log)
}

// torEndpointFrom достаёт из ротатора адреса, которые нужно опубликовать для
// других процессов. Control-порт может отсутствовать: внешний tor часто
// доступен только по socks, и требовать control значило бы сломать
// переиспользование чужого демона.
func torEndpointFrom(rot netx.Rotator) (socks, control string) {
	if rot == nil {
		return "", ""
	}
	spec := strings.TrimSpace(rot.TransportSpec())
	for _, prefix := range []string{"socks5h://", "socks5://"} {
		if strings.HasPrefix(spec, prefix) {
			spec = strings.TrimPrefix(spec, prefix)
			break
		}
	}
	if ca, ok := rot.(netx.ControlAddrer); ok {
		control = strings.TrimSpace(ca.ControlAddr())
	}
	return strings.TrimSuffix(spec, "/"), control
}
