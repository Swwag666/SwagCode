package main

import (
	"flag"
	"fmt"

	"voidsearchswag/internal/config"
	"voidsearchswag/internal/netx"
)

// cmdTorDaemon поднимает tor и держит его живым, пока процесс не остановят.
// Остальные команды находят демон через опубликованный файл эндпоинта и
// стартуют мгновенно вместо 20-25 секунд bootstrap на каждый вызов.
//
// Без этого каждая команда CLI платила полную цену поднятия tor: search,
// classify, probe и discover по отдельности греют цепи заново. Для одного
// вызова это терпимо, для скрипта из десятка вызовов - минуты простоя.
func cmdTorDaemon(args []string) {
	fs := flag.NewFlagSet("tord", flag.ExitOnError)
	detach := fs.Bool("survive", false, "tor переживёт закрытие этого процесса")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fatalf("конфиг: %v", err)
	}
	log := stderrLogger{}
	nc := cfg.NetxConfig(log)
	// Рекурсия переиспользования: tord сам является источником эндпоинта,
	// поэтому подключаться к опубликованному ранее он не должен.
	nc.TorNoReuse = true
	nc.TorSocksAddr = ""
	nc.TorControlAddr = ""
	if *detach {
		// Без привязки к нашему pid tor не умрёт вместе с процессом.
		nc.TorOwnProcess = false
	}
	if nc.TorBinary == "" {
		nc.TorBinary = netx.FindTorBinary()
	}
	if nc.TorBinary == "" {
		fatalf("tor-бинарь не найден: запусти voidsearchswag setup")
	}

	rot, err := netx.StartTor(nc)
	if err != nil {
		fatalf("tor: %v", err)
	}
	// Регистрация сразу после запуска, до любых операций, которые могут
	// завершиться аварийно. Порядок LIFO, поэтому rot.Close регистрируется
	// первым и выполнится последним: файл эндпоинта убирается раньше, чем tor
	// остановлен, и другие команды не подхватят мёртвый адрес.
	//
	// Без регистрации fatalf между этой точкой и ожиданием сигнала оставил бы
	// tor-процесс сиротой, держащим socks-порт: повторный tord упал бы с
	// «address already in use».
	offRot := AtExit("tor", rot.Close)
	offEndpoint := AtExit("файл эндпоинта tor", netx.RemoveTorEndpoint)
	defer func() {
		offEndpoint()
		offRot()
	}()

	socks, control := torEndpointFrom(rot)
	path, err := netx.SaveTorEndpoint(socks, control)
	if err != nil {
		log.Warnf("эндпоинт не опубликован: %v", err)
	}

	fmt.Printf("tor поднят: socks=%s", socks)
	if control != "" {
		fmt.Printf(" control=%s", control)
	}
	fmt.Println()
	if path != "" {
		fmt.Printf("эндпоинт: %s\n", path)
	}
	fmt.Println("остальные команды подключатся к нему сами")
	fmt.Printf("явное подключение: VOIDSEARCH_TOR_SOCKS=%s\n", socks)
	fmt.Println("остановка: Ctrl+C")

	// Сигналы обрабатывает NotifyShutdown, поставленный в main: он опустошает
	// реестр и завершает процесс.
	//
	// Прежняя версия держала собственный signal.Notify и делала очистку после
	// <-sig. С появлением общего обработчика это превратилось в гонку: обе
	// горутины получали сигнал, и os.Exit(130) из общего обработчика мог
	// опередить локальный rot.Close, оставив tor сиротой ровно в том сценарии,
	// который правка призвана устранить.
	fmt.Println("tor живёт, пока процесс не остановят")
	select {}
}
