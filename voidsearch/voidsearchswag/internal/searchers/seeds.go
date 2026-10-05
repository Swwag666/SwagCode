package searchers

import (
	"net/url"
	"strings"
)

type Seed struct {
	Name     string
	Base     string
	Path     string
	Selector string
	Category string
	ViaTor   bool
}

var defaultSeeds = []Seed{
	{
		Name:     "torch",
		Base:     "http://torchdeedp3i2jigzjdmfpn5ttjhthh5wbmda2rr3jvqjg5p77c54dqd.onion",
		Path:     "/search?query={q}",
		Selector: "article.result h2.result-title a, h2.result-title a",
		Category: "general",
		ViaTor:   true,
	},
	{
		Name:     "tornet",
		Base:     "http://tornetupfu7gcgidt33ftnungxzyfq2pygui5qdoyss34xbgx2qruzid.onion",
		Path:     "/search?q={q}",
		Selector: "div.results .item h2.title a, div.results h2.title a",
		Category: "general",
		ViaTor:   true,
	},
	{
		Name:     "tor66",
		Base:     "http://tor66sewebgixwhcqfnp5inzp5x5uohhdy3kvtnyfxc2e5mxiuh34iid.onion",
		Path:     "/search?q={q}",
		Selector: "div.sresults b a, div.sresults a[href*='.onion']",
		Category: "general",
		ViaTor:   true,
	},
	{
		Name:     "ahmia",
		Base:     "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion",
		Path:     "/search/?q={q}",
		Selector: ".result h4 a, .result a[href*='.onion']",
		Category: "general",
		ViaTor:   true,
	},
}

func DefaultSeeds() []Seed {
	out := make([]Seed, len(defaultSeeds))
	copy(out, defaultSeeds)
	return out
}

func EnginesFromSeeds(seeds []Seed) []*OnionEngine {
	out := make([]*OnionEngine, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, &OnionEngine{
			Name_:    s.Name,
			Base:     s.Base,
			Path:     s.Path,
			Selector: s.Selector,
		})
	}
	return out
}

func ParseSeeds(spec string) ([]Seed, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return DefaultSeeds(), nil
	}
	var out []Seed
	// Имена движков обязаны быть уникальны: пул здоровья хранит записи в
	// map по имени, и два движка с одним именем перетирают друг друга -
	// отчёт показывает один вместо двух.
	names := map[string]bool{}
	for _, chunk := range strings.Split(spec, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		parts := strings.Split(chunk, "|")
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		base := strings.TrimSpace(parts[1])
		// Пустое имя или база дают нерабочий движок: url.Parse("") ошибки
		// не возвращает, поэтому мусор вида "|||" раньше проходил дальше и
		// превращался в движок, который не может построить ни одного URL.
		if name == "" || base == "" {
			continue
		}
		if names[name] {
			continue
		}
		s := Seed{Name: name, Base: base, Path: "/search?q={q}", Selector: "a[href*='.onion']", ViaTor: true}
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			s.Path = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 && strings.TrimSpace(parts[3]) != "" {
			s.Selector = strings.TrimSpace(parts[3])
		}
		if len(parts) > 4 && strings.TrimSpace(parts[4]) != "" {
			s.Category = strings.TrimSpace(parts[4])
		}
		if !strings.HasSuffix(s.Base, ".onion") && !strings.Contains(s.Base, ".onion/") {
			s.ViaTor = false
		}
		// База обязана иметь хост: без него target() склеит относительный
		// путь и запрос уйдёт в никуда.
		u, err := url.Parse(s.Base)
		if err != nil || u.Host == "" {
			continue
		}
		names[name] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return DefaultSeeds(), nil
	}
	return out, nil
}
