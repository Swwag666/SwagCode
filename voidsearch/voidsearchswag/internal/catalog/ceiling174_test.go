package catalog

import (
	"context"
	"strings"
	"testing"
)

// Этап 174: оба потолка в одном прогоне. Прежний код писал обе причины в
// одно поле LimitHit вторым включением поверх первого: потолок файлов,
// сработавший после потолка хостов, затирал его, и ответ терял одно из
// двух состоявшихся событий. Составная строка обязана называть оба.

func TestCollectBothCeilingsInOneRun(t *testing.T) {
	st := newStore(t)
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(`<a href="/f`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`.zip">x</a>`)
	}
	f := &fakeClient{body: b.String()}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxHosts: 1, MaxFiles: 2}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion", v2b + ".onion", "aaaaaaaaaaaaaaaa.onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hosts != 1 {
		t.Errorf("потолок хостов не сработал: hosts=%d", rep.Hosts)
	}
	if rep.Saved != 2 {
		t.Errorf("потолок файлов не сработал: saved=%d", rep.Saved)
	}
	if rep.LimitHit != "достигнут потолок хостов; достигнут потолок файлов" {
		t.Fatalf("limit_hit=%q: оба события обязаны стоять одновременно, а не перезаписывать друг друга", rep.LimitHit)
	}
}

func TestCollectHostsCeilingAlone(t *testing.T) {
	st := newStore(t)
	f := &fakeClient{body: `<a href="/a.zip">a</a>`}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxHosts: 1}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion", v2b + ".onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.LimitHit != "достигнут потолок хостов" {
		t.Fatalf("limit_hit=%q: потолок один - строка обязана быть чистой, без «;»", rep.LimitHit)
	}
}

func TestCollectFilesCeilingAlone(t *testing.T) {
	st := newStore(t)
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString(`<a href="/f`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`.zip">x</a>`)
	}
	f := &fakeClient{body: b.String()}
	c := NewCollector(f, st, nil, nil, fastCfg(Config{MaxFiles: 2}))
	rep, err := c.Collect(context.Background(), []string{v2a + ".onion"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.LimitHit != "достигнут потолок файлов" {
		t.Fatalf("limit_hit=%q: потолок один - строка обязана быть чистой, без «;»", rep.LimitHit)
	}
}
