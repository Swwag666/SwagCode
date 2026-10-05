package discover

import (
	"context"
	"reflect"
	"testing"
)

// Живой замер до правки на HEAD 40a742b, очередь из пяти непроверенных адресов,
// где три невалидны: probe --limit 5 --json напечатал total=2 и results=2, и ни
// одно поле отчёта не упомянуло три потерянных адреса. На очереди, где невалидны
// все пять, отчёт сказал «живых 0 из 0», а команда завершилась с кодом 0.
func TestProbeCountsRejectedHosts(t *testing.T) {
	p := &Prober{}
	rep := p.Probe(context.Background(), []string{
		"http://unknaaaaaaaaaaaa.onion/",
		"http://bad0host1aaaaaaa.onion/",
		"unknaaaaaaaaaaaa.onion",
	})
	if rep.Total != 1 {
		t.Errorf("Total = %d, хочу 1: в волне один корректный адрес", rep.Total)
	}
	if rep.Rejected != 2 {
		t.Errorf("Rejected = %d, хочу 2: невалидный хост и повтор того же адреса", rep.Rejected)
	}
	want := []string{"http://bad0host1aaaaaaa.onion/", "unknaaaaaaaaaaaa.onion"}
	if !reflect.DeepEqual(rep.RejectedAddrs, want) {
		t.Errorf("RejectedAddrs = %v, хочу %v в порядке выборки", rep.RejectedAddrs, want)
	}
}

func TestProbeKeepsRejectedEmptyWhenAllValid(t *testing.T) {
	p := &Prober{}
	rep := p.Probe(context.Background(), []string{
		"http://unknaaaaaaaaaaaa.onion/",
		"http://unknaaaaaaaaaaab.onion/",
	})
	if rep.Total != 2 {
		t.Errorf("Total = %d, хочу 2", rep.Total)
	}
	if rep.Rejected != 0 {
		t.Errorf("Rejected = %d, хочу 0 на полностью корректной выборке", rep.Rejected)
	}
	if rep.RejectedAddrs != nil {
		t.Errorf("RejectedAddrs = %v, хочу пустой список", rep.RejectedAddrs)
	}
}

// Пустая выборка - это «проверять нечего», а не «всё отброшено»: счётчик
// отброшенных обязан остаться нулевым, иначе код возврата сообщил бы о провале
// там, где очередь пуста.
func TestProbeReportsNothingRejectedOnEmptyInput(t *testing.T) {
	p := &Prober{}
	rep := p.Probe(context.Background(), nil)
	if rep.Total != 0 || rep.Rejected != 0 {
		t.Errorf("на пустой выборке Total=%d Rejected=%d, хочу нули", rep.Total, rep.Rejected)
	}
	if rep.Elapsed == "" {
		t.Error("на пустой выборке Elapsed не заполнен")
	}
}

// Вся выборка отброшена: волна не состоялась, но отчёт обязан назвать и число, и
// сами адреса, чтобы оператор знал, что чистить в пуле.
func TestProbeNamesEveryRejectedHostWhenNoneValid(t *testing.T) {
	in := []string{
		"http://bad0host1aaaaaaa.onion/",
		"http://bad8host9aaaaaaa.onion/",
		"http://short.onion/",
		"http://notonion.example/",
		"http://toolonglabel0123456789abcdef.onion/",
	}
	p := &Prober{}
	rep := p.Probe(context.Background(), in)
	if rep.Total != 0 {
		t.Errorf("Total = %d, хочу 0: до волны не дошёл никто", rep.Total)
	}
	if rep.Rejected != len(in) {
		t.Errorf("Rejected = %d, хочу %d", rep.Rejected, len(in))
	}
	if !reflect.DeepEqual(rep.RejectedAddrs, in) {
		t.Errorf("RejectedAddrs = %v, хочу %v", rep.RejectedAddrs, in)
	}
}
