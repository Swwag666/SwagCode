package searchers

import (
	"strings"
	"testing"
)

func TestRerankTitleOverlapWins(t *testing.T) {
	in := []Result{
		{Title: "Погода завтра", URL: "https://a.example/1", Source: "ddg"},
		{Title: "Leak database dump", URL: "https://b.example/2", Snippet: "leak", Source: "ddg"},
	}
	out := Rerank(in, "leak database", "fast")
	if out[0].URL != "https://b.example/2" {
		t.Errorf("топ неверен: %+v", out[0])
	}
	if out[0].Rank != 1 || out[1].Rank != 2 {
		t.Errorf("ранги не переназначены: %d %d", out[0].Rank, out[1].Rank)
	}
}

func TestRerankOnionBonusOnlyDeep(t *testing.T) {
	in := []Result{
		{Title: "Leak database forum clear", URL: "https://c.example/1", Snippet: "leak database", Source: "ddg"},
		{Title: "Leak database", URL: "http://abcdefabcdefabcd.onion/1", Source: "torch"},
	}
	deep := Rerank(in, "leak database", "deep")
	if !strings.HasSuffix(hostOf(deep[0].URL), ".onion") {
		t.Errorf("в deep onion обязан быть первым: %+v", deep[0])
	}
	fast := Rerank(in, "leak database", "fast")
	if strings.HasSuffix(hostOf(fast[0].URL), ".onion") {
		t.Errorf("в fast бонуса onion быть не должно: %+v", fast[0])
	}
}

func TestRerankStableOnTies(t *testing.T) {
	in := []Result{
		{Title: "Second", URL: "https://b.example/2", Source: "s"},
		{Title: "First", URL: "https://a.example/1", Source: "s"},
	}
	out := Rerank(in, "zzz qqq", "fast")
	// Очки равны (0+1 за сниппет? сниппетов нет, оба 0): порядок исходный.
	if out[0].URL != "https://b.example/2" {
		t.Errorf("стабильность нарушена: %+v", out[0])
	}
}

func TestRerankDiversityPenalty(t *testing.T) {
	var in []Result
	for i := 0; i < 5; i++ {
		in = append(in, Result{Title: "Leak x", URL: "https://mono.example/" + string(rune('a'+i)), Source: "mono"})
	}
	in = append(in, Result{Title: "Leak y", URL: "https://other.example/1", Source: "other"})
	out := Rerank(in, "leak", "fast")
	// Первые три mono без штрафа, 4-5й тонут ниже конкурента.
	if out[3].Source == "mono" && out[3].URL == "https://mono.example/d" {
		t.Errorf("доминация движка не урезана: %v", out[3])
	}
}

func TestRerankEmptyTitlePenalty(t *testing.T) {
	in := []Result{
		{Title: "", URL: "https://a.example/1", Source: "s"},
		{Title: "Leak", URL: "https://b.example/2", Source: "s"},
	}
	out := Rerank(in, "leak", "fast")
	if out[0].URL != "https://b.example/2" {
		t.Errorf("безымянный результат первый: %+v", out[0])
	}
}

func TestRerankEmpty(t *testing.T) {
	if out := Rerank(nil, "q", "fast"); len(out) != 0 {
		t.Errorf("пустой вход дал %+v", out)
	}
}
