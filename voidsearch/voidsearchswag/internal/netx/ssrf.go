package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrPrivateTarget возвращается, когда публичный интерфейс просят сходить в
// служебную, приватную или зарезервированную сеть.
var ErrPrivateTarget = errors.New("цель в служебной или приватной сети")

// ErrBadScheme возвращается на адрес, который нельзя запросить как веб-страницу:
// другая схема, отсутствие хоста, нечитаемый URL.
var ErrBadScheme = errors.New("неподдерживаемая схема адреса")

// lookupIPs вынесен переменной, чтобы тесты могли подменить разрешение имён и не
// зависеть от DNS машины.
var lookupIPs = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// reservedNets дополняет то, что Go не помечает сам: IsLoopback, IsPrivate,
// IsLinkLocalUnicast и IsMulticast покрывают очевидное, но диапазоны CGNAT,
// тестовые сети документации и зарезервированные блоки остаются открытыми, а
// запрос к ним из публичного интерфейса ничем не лучше запроса к 127.0.0.1.
var reservedNets = mustReservedNets()

func mustReservedNets() []struct {
	net *net.IPNet
	why string
} {
	defs := []struct {
		cidr string
		why  string
	}{
		{"0.0.0.0/8", "блок «эта сеть»"},
		{"100.64.0.0/10", "CGNAT провайдера"},
		{"192.0.0.0/24", "служебный блок IETF"},
		{"192.0.2.0/24", "тестовая сеть документации"},
		{"198.18.0.0/15", "блок измерений"},
		{"198.51.100.0/24", "тестовая сеть документации"},
		{"203.0.113.0/24", "тестовая сеть документации"},
		{"255.255.255.255/32", "широковещательный адрес"},
		{"240.0.0.0/4", "зарезервированный блок"},
		{"64:ff9b::/96", "префикс NAT64"},
		{"100::/64", "блок Discard-Only"},
		{"2001::/23", "служебный блок IETF для Teredo и измерений"},
		{"2001:2::/48", "тестовая сеть документации IPv6"},
		{"2001:db8::/32", "тестовая сеть документации IPv6"},
	}
	out := make([]struct {
		net *net.IPNet
		why string
	}, 0, len(defs))
	for _, d := range defs {
		_, n, err := net.ParseCIDR(d.cidr)
		if err != nil {
			panic("netx: неверный CIDR " + d.cidr + ": " + err.Error())
		}
		out = append(out, struct {
			net *net.IPNet
			why string
		}{n, d.why})
	}
	return out
}

// BlockedReason объясняет, почему адрес не годится в цель публичного запроса.
// Пустая строка означает, что адрес разрешён.
//
// IPv4-mapped адреса приводятся к IPv4 до проверок: ::ffff:127.0.0.1 обязан
// ловиться так же, как 127.0.0.1, иначе запись имени в другой нотации снимала бы
// весь барьер.
func BlockedReason(ip net.IP) string {
	if ip == nil {
		return "адрес не разобран"
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() {
		return "неопределённый адрес"
	}
	if ip.IsLoopback() {
		return "петлевой адрес"
	}
	if ip.IsPrivate() {
		return "приватная сеть"
	}
	if ip.IsLinkLocalUnicast() {
		return "link-local адрес"
	}
	if ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return "локальный multicast"
	}
	if ip.IsMulticast() {
		return "multicast"
	}
	for _, r := range reservedNets {
		if r.net.Contains(ip) {
			return r.why
		}
	}
	return ""
}

// CheckPublicTarget проверяет, что URL годится в цель запроса, который приходит
// извне: схема http или https, хост присутствует, а разрешённые адреса не
// служебные.
//
// Барьер нужен на публичных входах (инструмент fetch, разбор страницы по
// адресу), а не в сессии транспорта. Внутри продукта приватные адреса
// легальны: пиры синхронизации могут жить в одной локальной сети, список
// прокси отдаёт локальный стенд, а tor-сокет всегда 127.0.0.1. Поэтому фильтр
// стоит там, где адрес диктует внешний вызывающий, и не ломает внутренние пути.
//
// Имя разрешается здесь же, а не оставляется транспорту: без этого
// http://localhost:8080 проходил бы проверку по строке «localhost», хотя
// резолвится в 127.0.0.1. Проверяются ВСЕ адреса имени, а не первый: запись с
// двумя A-записями, публичной и петлевой, иначе выбирала бы удобную.
//
// Граница: проверка выполняется до запроса, поэтому редирект на приватный адрес
// и переназначение DNS между проверкой и соединением этим барьером не ловятся.
// Полный контроль требует проверки в диалере транспорта.
func CheckPublicTarget(ctx context.Context, rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return fmt.Errorf("%w: пустой адрес", ErrBadScheme)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%w: адрес не разобран: %v", ErrBadScheme, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: %q, поддерживаются только http и https", ErrBadScheme, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: в адресе нет хоста", ErrBadScheme)
	}
	if ip := net.ParseIP(host); ip != nil {
		if why := BlockedReason(ip); why != "" {
			return fmt.Errorf("%w: %s - %s", ErrPrivateTarget, host, why)
		}
		return nil
	}
	ips, err := lookupIPs(ctx, host)
	if err != nil {
		return fmt.Errorf("имя %s не разрешается: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("имя %s не разрешилось ни в один адрес", host)
	}
	for _, ip := range ips {
		if why := BlockedReason(ip); why != "" {
			return fmt.Errorf("%w: %s разрешается в %s - %s", ErrPrivateTarget, host, ip, why)
		}
	}
	return nil
}
