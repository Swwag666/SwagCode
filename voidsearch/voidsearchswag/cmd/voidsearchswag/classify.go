package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"voidsearchswag/internal/classifier"
	"voidsearchswag/internal/config"
	"voidsearchswag/internal/parser"
)

func cmdClassify(args []string) {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	noTor := fs.Bool("no-tor", false, "не поднимать tor")
	jsonOut := fs.Bool("json", false, "вывод в JSON")
	parseFlags(fs, args)
	if fs.NArg() == 0 {
		fatalf("нужен URL: voidsearchswag classify <url>")
	}
	rawURL := fs.Arg(0)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	if *noTor {
		cfg.Tor = "off"
	}
	log := stderrLogger{}
	_, engine, cleanup := openStoreAndEngine(cfg, log)
	defer AtExit("поисковое ядро", AsError(cleanup))()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := engine.FetchURL(ctx, rawURL)
	if err != nil {
		fatalf("fetch: %v", err)
	}
	body := string(resp.Body)
	v := classifier.Classify(rawURL, parser.TitleOf(body), "", body)
	if *jsonOut {
		writeJSON(v)
		return
	}
	fmt.Printf("тип: %s\nкачество: %.1f\nпопулярность: %.1f\nпричина: %s\n", v.Type, v.Quality, v.Popularity, v.Reason)
}
