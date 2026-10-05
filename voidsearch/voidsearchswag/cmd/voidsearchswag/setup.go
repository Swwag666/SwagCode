package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/netx"
	"voidsearchswag/internal/searchers"
	"voidsearchswag/internal/setup"
	"voidsearchswag/internal/store"
)

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	noVerify := fs.Bool("no-verify", false, "не проверять sha256 скачанного tor-бандла")
	skipCheck := fs.Bool("skip-check", false, "не проверять bootstrap tor после установки")
	offline := fs.Bool("offline", false, "не качать tor и chromium: только база; бинарь tor обязан уже лежать в vendor")
	noBrowser := fs.Bool("no-browser", false, "не ставить chromium для stealth-режима")
	warmPool := fs.Bool("warm-pool", false, "прогреть прокси-пул ProxyScrape (долго, минуты)")
	parseFlags(fs, args)

	log := stderrLogger{}
	ctx := context.Background()

	if err := os.MkdirAll(netx.DefaultDataDir(), 0o755); err != nil {
		fatalf("каталог данных: %v", err)
	}
	log.Infof("каталог данных: %s", netx.DefaultDataDir())

	bin := ""
	if *offline {
		bin = netx.VendorDir() + string(os.PathSeparator) + netx.TorBinaryName()
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			fatalf("offline: бинаря нет (%s) — убери --offline или положи tor вручную", bin)
		}
		log.Infof("offline: tor уже на месте: %s", bin)
	} else {
		var err error
		bin, err = setup.EnsureTor(ctx, !*noVerify, log.Infof)
		if err != nil {
			fatalf("tor: %v", err)
		}
	}

	st, err := store.Open(netx.DefaultDBPath())
	if err != nil {
		fatalf("база: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		fatalf("миграции: %v", err)
	}
	st.Close()
	log.Infof("база готова: %s", netx.DefaultDBPath())

	// Chromium для stealth-режима: rod докачивает его сам при первом поиске,
	// но это ~150МБ посреди задачи агента. Греем заранее; без chromium
	// stealth честно деградирует на HTTP-движки, установка не падает.
	if *offline || *noBrowser {
		log.Infof("браузер пропущен (флаг): stealth будет без cloaked-рендера")
	} else {
		bcfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		br := searchers.NewBrowser("", bcfg.ChromePath, true, log)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if err := br.Warmup(wctx); err != nil {
			cancel()
			br.Close()
			log.Warnf("браузер не встал (%v): stealth будет без cloaked-рендера; свой chromium задай через VOIDSEARCH_CHROME_PATH", err)
		} else {
			cancel()
			br.Close()
			log.Infof("браузер готов: stealth-режим с cloaked-рендером")
		}
	}

	// Прогрев прокси-пула долгий и зависит от публичного провайдера,
	// поэтому только по явному флагу. Без него транспорт tor/direct,
	// пул догреется по команде poolcheck.
	if *warmPool && !*offline {
		// warmProxyPool общая с poolcheck, но setup до этапа 154 звал её с
		// литералами «http», «all», 40, 400, true: конфиг этой команды не
		// касался вовсе. Оператор, задавший VOIDSEARCH_PROXYSCRAPE_PROTOCOL
		// или VOIDSEARCH_PROXY_SKIP_VERIFY, получал прогрев по своим правилам
		// через poolcheck и по чужим - через установку, а расхождение между
		// двумя путями к одному пулу нигде не печаталось. С этапа 155 setup
		// строит тот же netx.Config, что и транспорт pool, поэтому правило одно
		// на обе команды и на сам транспорт. Слоя флага здесь нет, потому что
		// своих флагов на эти поля у setup нет.
		wcfg, err := config.Load()
		if err != nil {
			fatalf("конфиг: %v", err)
		}
		wnc := wcfg.NetxConfig(log)
		wprov, wpc := poolParams(wnc)
		wlines := poolParamsLines(wprov, wpc)
		log.Infof("прогрев пула: %s", wlines[0])
		log.Infof("поставщик: %s", wlines[1])
		pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		pool, err := warmProxyPool(pctx, log, wnc)
		cancel()
		if err != nil {
			log.Warnf("пул не прогрет (%v): добей вручную через poolcheck", err)
		} else {
			live, total, _ := pool.Stats()
			pool.SaveState()
			log.Infof("пул прогрет: живых %d из %d, state сохранён", live, total)
		}
	}

	if !*skipCheck {
		cfg := netx.DefaultConfig()
		cfg.TorBinary = bin
		cfg.Logger = log
		rot, err := netx.StartTor(cfg)
		if err != nil {
			fatalf("tor bootstrap: %v", err)
		}
		// Регистрация до проверки: если ниже что-то вызовет fatalf, tor
		// останется сиротой и будет держать socks-порт, из-за чего повторный
		// setup упадёт с «address already in use».
		offRot := AtExit("tor", rot.Close)
		log.Infof("tor поднят: %s", rot.TransportSpec())
		rctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		if err := rot.Rotate(rctx); err != nil {
			log.Warnf("NEWNYM не ответил: %v", err)
		} else {
			log.Infof("NEWNYM ок, цепь сменилась")
		}
		cancel()
		// offRot выполняет rot.Close и снимает регистрацию: явный второй вызов
		// Close был бы двойным закрытием.
		offRot()
	}
	log.Infof("setup завершён")
	printMCPSnippet()
}

// printMCPSnippet печатает готовый кусок конфига MCP-клиента с абсолютным
// путём к текущему бинарю: вставил в claude_desktop_config.json - агент
// получил весь поисковый стек. Печатается в stdout, логи идут в stderr.
func printMCPSnippet() {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "voidsearchswag(.exe)"
	}
	const tokenNote = "для http-транспорта добавь \"args\": [\"run\", \"--http\", \":8800\", \"--token\", \"...\" ]"
	fmt.Printf(`
Подключение агента (claude_desktop_config.json):

{
  "mcpServers": {
    "voidsearchswag": {
      "command": %q,
      "args": ["run"]
    }
  }
}

%s
Проверка: search "golang context timeout" --mode auto
Доки: docs/MCP.md (инструменты), docs/INTEGRATE.md (интеграция)
`, exe, tokenNote)
}
