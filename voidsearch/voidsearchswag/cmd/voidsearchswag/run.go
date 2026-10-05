package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/mcpserver"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/parser"
	"voidsearchswag/internal/promote"
	"voidsearchswag/internal/store"
)

// listenPolicyFor проверяет сочетание адреса слушателя и токена по собранному
// config. Вынесено из cmdRun потому, что команда поднимает долгоживущий сервер
// и ждать её завершения в тесте нельзя, а связь полей config с политикой
// проверить нужно.
func listenPolicyFor(cfg config.Config) error {
	return mcpserver.CheckListenPolicy(cfg.HTTPAddr, cfg.HTTPToken, cfg.HTTPAllowOpen)
}

// recoverStaleTasks финализирует running-задачи, оставшиеся от прошлой
// жизни процесса. Этап 180: рестарт или жёсткий kill сервера посреди
// обхода оставлял задачу running навсегда - tasks_running в status врал
// вечно (смоук 179 оставил 8 таких в базе стенда). Живой задачи в момент
// старта процесса не существует по определению, поэтому переводится всё
// running без разбора: до tool-вызовов новый процесс ещё не создал ни
// одной своей.
func recoverStaleTasks(ctx context.Context, st *store.Store, log netx.Logger) error {
	n, err := st.FailRunningTasks(ctx, "прерван рестартом сервера")
	if err != nil {
		return err
	}
	if n > 0 {
		log.Infof("задачи: %d зависших running переведены в failed (прерваны рестартом)", n)
	}
	return nil
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	httpAddr := fs.String("http", "", "streamable-http адрес (пусто = stdio)")
	noTor := fs.Bool("no-tor", false, "не поднимать tor (clearnet/proxy режим)")
	tokenFlag := fs.String("token", "", "bearer-токен для --http (или VOIDSEARCH_HTTP_TOKEN)")
	tokenFile := fs.String("token-file", "", "файл с bearer-токеном (не светится в ps)")
	tlsCert := fs.String("tls-cert", "", "PEM-сертификат для --http (пара с --tls-key)")
	tlsKey := fs.String("tls-key", "", "PEM-ключ для --http (пара с --tls-cert)")
	allowOpen := fs.Bool("http-allow-open", false,
		"слушать не-петлевой адрес без токена (или VOIDSEARCH_HTTP_ALLOW_OPEN=1)")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *noTor {
		cfg.Tor = "off"
	}
	if *tokenFlag != "" {
		cfg.HTTPToken = *tokenFlag
	}
	if *tokenFile != "" {
		// Явный --token-file означает «токен берём из файла», поэтому флаг
		// перекрывает и значение из окружения. Но стирать HTTPToken можно
		// только после успешного чтения: прежний порядок обнулял токен до
		// ApplyTokenFile, и при опечатке в пути сервер поднимался полностью
		// открытым, а не с прежним рабочим токеном.
		cfg.HTTPTokenFile = *tokenFile
		saved := cfg.HTTPToken
		cfg.HTTPToken = ""
		if err := cfg.ApplyTokenFile(); err != nil {
			cfg.HTTPToken = saved
			fatalf("токен: %v", err)
		}
	}
	if *tlsCert != "" {
		cfg.TLSCert = *tlsCert
	}
	if *tlsKey != "" {
		cfg.TLSKey = *tlsKey
	}
	if *allowOpen {
		cfg.HTTPAllowOpen = true
	}

	// Политика слушателя проверяется до тяжёлой инициализации: база, ядро и
	// tor-слой занимают секунды, а отказ должен быть мгновенным и понятным.
	// Тот же запрет продублирован в mcpserver.ServeWithOptions, поэтому обойти
	// его через другой вход в сервер не получится.
	if err := listenPolicyFor(cfg); err != nil {
		fatalf("слушатель: %v", err)
	}

	lvl, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	cw := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
	logger := zerolog.New(cw).Level(lvl).With().Timestamp().Logger()
	log := zlogAdapter{logger}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	defer AtExit("база", st.Close)()
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}
	if n, err := st.CachePurgeExpired(ctx); err == nil && n > 0 {
		log.Infof("кэш: вычищено %d протухших записей", n)
	}
	if err := recoverStaleTasks(ctx, st, log); err != nil {
		fatalf("задачи: %v", err)
	}

	engine, cleanup := buildEngine(cfg, log, st)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	log.Infof("поисковое ядро готово: onion=%d, браузер=%v",
		onionCount(engine), cfg.Headless)
	pool := buildDiscover(cfg, log, st, engine)
	if pool == nil {
		log.Warnf("discovery-слой недоступен: инструменты разведки выключены")
	}
	prober := buildProber(cfg, log, st, engine, 0)
	collector := buildCollector(cfg, log, st, engine)
	parsew := &parser.Parser{Store: st, Fetch: engineFetch{engine}}
	hunter := newHuntRunner(st, engine)
	promoter := &promote.Promoter{Client: engine.Client, Log: log}
	keep := cfg.BackupKeep
	if keep <= 0 {
		keep = 7
	}

	srv := mcpserver.New(mcpserver.Deps{
		Version:    version,
		Store:      st,
		Rot:        engine.Rotator(),
		Search:     engine,
		Discover:   pool,
		Prober:     prober,
		Collector:  collector,
		Parser:     parsew,
		Hunter:     hunter,
		Promoter:   promoter,
		Peers:      cfg.Peers,
		PeerToken:  cfg.HTTPToken,
		BackupDir:  cfg.BackupDirOrDefault(),
		BackupKeep: keep,
		Started:    time.Now(),
	})

	if cfg.HTTPAddr != "" {
		scheme := "http"
		if cfg.TLSCert != "" {
			scheme = "https"
		}
		// Этап 178 (смоук-I): banner несёт хост, а не пустоту. Смоук-стенды
		// стартуют --http :3368 - host-часть пуста, и строка читалась
		// «MCP слушает http://:3368/mcp»: адрес без хоста не paste-able и
		// выглядит как сломанный лог. Слушатель по пустому хосту обязан
		// всем интерфейсам - banner это называет.
		bannerAddr := cfg.HTTPAddr
		if strings.HasPrefix(bannerAddr, ":") {
			bannerAddr = "0.0.0.0" + bannerAddr
		}
		log.Infof("MCP слушает %s://%s/mcp", scheme, bannerAddr)
		if cfg.HTTPToken != "" {
			log.Infof("HTTP-auth включён (Bearer)")
		} else if cfg.HTTPAllowOpen {
			log.Warnf("HTTP без токена открыт всей сети по --http-allow-open: любой хост сети получает все инструменты")
		} else {
			log.Infof("HTTP без токена: адрес %s петлевой, сервер виден только этой машине", cfg.HTTPAddr)
		}
		if *tokenFlag != "" {
			log.Warnf("--token светится в ps: лучше --token-file или env")
		}
	} else {
		log.Infof("MCP по stdio; выход по Ctrl+C")
	}
	startBackground(ctx, log, cfg, st, engine, hunter, pool, prober, promoter, srv)
	if err := mcpserver.ServeWithOptions(ctx, srv, mcpserver.HTTPOptions{
		Addr: cfg.HTTPAddr, Token: cfg.HTTPToken,
		CertFile: cfg.TLSCert, KeyFile: cfg.TLSKey,
		Store:           st,
		AllowOpen:       cfg.HTTPAllowOpen,
		RateLimitPerMin: cfg.RateLimitPerMin,
	}); err != nil {
		fatalf("mcp: %v", err)
	}
	log.Infof("остановлено")
}
