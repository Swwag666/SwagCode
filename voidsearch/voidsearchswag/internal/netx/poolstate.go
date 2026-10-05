package netx

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	stateDeadTTL   = 30 * 24 * time.Hour
	stateBannedTTL = 6 * time.Hour
	stateStrikeTTL = 6 * time.Hour

	stateStrikeBan = 3

	stateMaxEntries = 20000

	stateSeenTTL = 24 * time.Hour
)

type entryState struct {
	ExitIP   string    `json:"ip,omitempty"`
	SeenAt   time.Time `json:"seen_at,omitempty"`
	DeadAt   time.Time `json:"dead_at,omitempty"`
	BanAt    time.Time `json:"ban_at,omitempty"`
	BanUntil time.Time `json:"ban_until,omitempty"`
	StrikeAt time.Time `json:"strike_at,omitempty"`
	Strikes  int       `json:"strikes,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func (e entryState) expiredLocked(now time.Time) bool {
	if !e.DeadAt.IsZero() && now.Sub(e.DeadAt) < stateDeadTTL {
		return false
	}
	if !e.BanUntil.IsZero() && now.Before(e.BanUntil) {
		return false
	}
	if e.Strikes > 0 && !e.StrikeAt.IsZero() && now.Sub(e.StrikeAt) < stateStrikeTTL {
		return false
	}
	if e.ExitIP != "" && !e.SeenAt.IsZero() && now.Sub(e.SeenAt) < stateSeenTTL {
		return false
	}
	return true
}

type stateFile struct {
	Updated   time.Time             `json:"updated"`
	Proxies   map[string]entryState `json:"proxies"`
	BannedIP  map[string]time.Time  `json:"banned_ip"`
	BannedNet map[string]time.Time  `json:"banned_net"`
}

type PoolState struct {
	mu    sync.Mutex
	path  string
	st    stateFile
	dirty bool
	log   Logger
}

func LoadPoolState(path string, log Logger) *PoolState {
	ps := &PoolState{path: path, log: log, st: stateFile{
		Proxies:   map[string]entryState{},
		BannedIP:  map[string]time.Time{},
		BannedNet: map[string]time.Time{},
	}}
	if path == "" {
		return ps
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ps
	}
	var f stateFile
	if json.Unmarshal(b, &f) != nil {
		if log != nil {
			log.Warnf("файл состояния %s битый - начинаю с чистого", path)
		}
		return ps
	}
	if f.Proxies != nil {
		ps.st.Proxies = f.Proxies
	}
	if f.BannedIP != nil {
		ps.st.BannedIP = f.BannedIP
	}
	if f.BannedNet != nil {
		ps.st.BannedNet = f.BannedNet
	}
	now := time.Now()
	kept := ps.pruneLocked(now)
	if log != nil {
		log.Infof("состояние пула: %s, записей %d (после pruning %d)", filepath.Base(path), len(f.Proxies), kept)
	}
	return ps
}

func (p *PoolState) pruneLocked(now time.Time) int {
	for k, e := range p.st.Proxies {
		if e.expiredLocked(now) {
			delete(p.st.Proxies, k)
		}
	}
	for k, until := range p.st.BannedIP {
		if now.After(until) {
			delete(p.st.BannedIP, k)
		}
	}
	for k, until := range p.st.BannedNet {
		if now.After(until) {
			delete(p.st.BannedNet, k)
		}
	}
	if len(p.st.Proxies) > stateMaxEntries {
		type kv struct {
			k string
			t time.Time
		}
		list := make([]kv, 0, len(p.st.Proxies))
		for k, e := range p.st.Proxies {
			t := e.DeadAt
			if e.BanAt.After(t) {
				t = e.BanAt
			}
			if e.StrikeAt.After(t) {
				t = e.StrikeAt
			}
			if e.SeenAt.After(t) {
				t = e.SeenAt
			}
			list = append(list, kv{k, t})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].t.Before(list[j].t) })
		for _, x := range list[:len(list)-stateMaxEntries] {
			delete(p.st.Proxies, x.k)
		}
	}
	return len(p.st.Proxies)
}

func (p *PoolState) Prune() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.pruneLocked(time.Now())
	p.dirty = true
	return n
}

func (p *PoolState) Skip(spec, exitIP string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if e, ok := p.st.Proxies[spec]; ok {
		if !e.DeadAt.IsZero() && now.Sub(e.DeadAt) < stateDeadTTL {
			return "мёртв (" + e.Reason + ")"
		}
		if !e.BanUntil.IsZero() && now.Before(e.BanUntil) {
			return "забанен целью до " + e.BanUntil.Local().Format("15:04")
		}
	}
	if exitIP != "" {
		if until, ok := p.st.BannedIP[exitIP]; ok && now.Before(until) {
			return "выход " + exitIP + " под баном"
		}
		if n := netSlash24(exitIP); n != "" {
			if until, ok := p.st.BannedNet[n]; ok && now.Before(until) {
				return "диапазон " + n + " под баном"
			}
		}
	}
	return ""
}

func (p *PoolState) MarkDead(spec, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	e := p.st.Proxies[spec]
	e.DeadAt = now
	e.Reason = clip(reason, 120)
	e.Strikes, e.StrikeAt = 0, time.Time{}
	p.st.Proxies[spec] = e
	p.dirty = true
}

func (p *PoolState) MarkBanned(spec, exitIP, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	e := p.st.Proxies[spec]
	e.BanAt = now
	e.BanUntil = now.Add(stateBannedTTL)
	e.Reason = clip(reason, 120)
	p.st.Proxies[spec] = e
	if exitIP != "" {
		p.st.BannedIP[exitIP] = now.Add(stateBannedTTL)
		if n := netSlash24(exitIP); n != "" {
			p.st.BannedNet[n] = now.Add(stateBannedTTL)
		}
	}
	p.dirty = true
}

func (p *PoolState) MarkSuspect(spec, exitIP, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	e := p.st.Proxies[spec]
	if e.Strikes == 0 || now.Sub(e.StrikeAt) > stateStrikeTTL {
		e.Strikes = 0
	}
	e.Strikes++
	e.StrikeAt = now
	e.Reason = clip(reason, 120)
	if e.Strikes >= stateStrikeBan {
		e.BanAt = now
		e.BanUntil = now.Add(stateBannedTTL)
		e.Strikes = 0
		if exitIP != "" {
			p.st.BannedIP[exitIP] = now.Add(stateBannedTTL)
			if n := netSlash24(exitIP); n != "" {
				p.st.BannedNet[n] = now.Add(stateBannedTTL)
			}
		}
	}
	p.st.Proxies[spec] = e
	p.dirty = true
}

func (p *PoolState) RememberExit(spec, exitIP string) {
	if exitIP == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.st.Proxies[spec]
	if e.ExitIP == exitIP {
		return
	}
	e.ExitIP = exitIP
	e.SeenAt = time.Now()
	p.st.Proxies[spec] = e
	p.dirty = true
}

func (p *PoolState) ExitIP(spec string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.Proxies[spec].ExitIP
}

func (p *PoolState) Counts() (dead, banned, striking int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, e := range p.st.Proxies {
		switch {
		case !e.DeadAt.IsZero() && now.Sub(e.DeadAt) < stateDeadTTL:
			dead++
		case !e.BanUntil.IsZero() && now.Before(e.BanUntil):
			banned++
		case e.Strikes > 0:
			striking++
		}
	}
	return
}

func (p *PoolState) Path() string { return p.path }

func (p *PoolState) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dirty || p.path == "" {
		return nil
	}
	p.st.Updated = time.Now()
	p.pruneLocked(p.st.Updated)
	b, err := json.MarshalIndent(&p.st, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		_ = os.Remove(p.path)
		if err2 := os.Rename(tmp, p.path); err2 != nil {
			return err2
		}
	}
	p.dirty = false
	return nil
}

func netSlash24(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	v4 := parsed.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
}
