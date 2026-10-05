package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type proxyInfo struct {
	scheme string
	host   string
	user   string
	pass   string
}

func (p proxyInfo) isSocks() bool { return p.scheme == "socks5" || p.scheme == "socks4" }

func parseProxySpec(spec string) (proxyInfo, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return proxyInfo{}, errors.New("пустой адрес прокси")
	}
	var pi proxyInfo
	if i := strings.Index(s, "://"); i >= 0 {
		pi.scheme = strings.ToLower(s[:i])
		s = s[i+3:]
	} else {
		pi.scheme = "socks5"
	}
	switch pi.scheme {
	case "http", "https", "socks5", "socks4", "socks5h":
	default:
		return proxyInfo{}, fmt.Errorf("неизвестная схема прокси %q", pi.scheme)
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		pi.user, pi.pass, _ = strings.Cut(s[:i], ":")
		s = s[i+1:]
	}
	if pi.user == "" && pi.pass == "" {
		if parts := strings.Split(s, ":"); len(parts) == 4 {
			s = parts[0] + ":" + parts[1]
			pi.user, pi.pass = parts[2], parts[3]
		}
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return proxyInfo{}, fmt.Errorf("адрес прокси %q: %w", spec, err)
	}
	pi.host = s
	return pi, nil
}

func parseProxyURL(spec string) (*url.URL, error) {
	pi, err := parseProxySpec(spec)
	if err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: pi.scheme, Host: pi.host}
	if pi.user != "" || pi.pass != "" {
		u.User = url.UserPassword(pi.user, pi.pass)
	}
	return u, nil
}

func socksDial(pi proxyInfo, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(dctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(dctx, "tcp", pi.host)
		if err != nil {
			return nil, fmt.Errorf("socks5 connect: %w", err)
		}
		dl, ok := dctx.Deadline()
		if !ok {
			dl = time.Now().Add(timeout)
		}
		if err := conn.SetDeadline(dl); err != nil {
			conn.Close()
			return nil, err
		}
		if pi.scheme == "socks4" {
			err = socks4Connect(conn, addr, pi.user)
		} else {
			err = socks5RoundTrip(conn, addr, pi.user, pi.pass)
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func socks5RoundTrip(conn net.Conn, addr, user, pass string) error {
	if user != "" || pass != "" {
		if _, err := conn.Write([]byte{0x05, 0x02, 0x00, 0x02}); err != nil {
			return fmt.Errorf("socks5 hello: %w", err)
		}
	} else if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("socks5 hello: %w", err)
	}
	sel := make([]byte, 2)
	if _, err := readFull(conn, sel); err != nil {
		return fmt.Errorf("socks5 select: %w", err)
	}
	if sel[0] != 0x05 {
		return fmt.Errorf("socks5: ответ версии %d", sel[0])
	}
	switch sel[1] {
	case 0xFF:
		return errors.New("socks5: нет подходящего метода")
	case 0x00:
	case 0x02:
		if err := socks5Auth(conn, user, pass); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5: метод %d не поддерживается", sel[1])
	}
	return socks5Connect(conn, addr)
}

func socks5Auth(conn net.Conn, user, pass string) error {
	u, p := []byte(user), []byte(pass)
	if len(u) > 255 || len(p) > 255 {
		return errors.New("socks5: креды слишком длинные")
	}
	buf := append([]byte{0x01, byte(len(u))}, u...)
	buf = append(buf, byte(len(p)))
	buf = append(buf, p...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5 auth: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 auth answer: %w", err)
	}
	if resp[1] != 0x00 {
		return errors.New("socks5: авторизация отклонена")
	}
	return nil
}

func socks5Connect(conn net.Conn, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5 addr: %w", err)
	}
	req := append([]byte{0x05, 0x01, 0x00}, encodeSocksAddr(host)...)
	pn, err := strconv.Atoi(port)
	if err != nil || pn <= 0 || pn > 65535 {
		return fmt.Errorf("socks5: плохой порт в %q", addr)
	}
	req = append(req, byte(pn>>8), byte(pn))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 request: %w", err)
	}
	head := make([]byte, 4)
	if _, err := readFull(conn, head); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: %s", socksErr(head[1]))
	}
	return skipBoundAddr(conn, head[3])
}

func encodeSocksAddr(host string) []byte {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return append([]byte{0x01}, v4...)
		}
		return append([]byte{0x04}, ip...)
	}
	if len(host) > 255 {
		host = host[:255]
	}
	return append([]byte{0x03, byte(len(host))}, host...)
}

func socks4Connect(conn net.Conn, addr, user string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks4 addr: %w", err)
	}
	pn, err := strconv.Atoi(portStr)
	if err != nil || pn <= 0 || pn > 65535 {
		return fmt.Errorf("socks4: плохой порт в %q", addr)
	}
	req := []byte{0x04, 0x01, byte(pn >> 8), byte(pn)}
	var ip4 []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			ip4 = v4
		} else {
			return errors.New("socks4: IPv6 не поддерживается")
		}
	}
	if ip4 != nil {
		req = append(req, ip4...)
	} else {
		req = append(req, 0, 0, 0, 1)
	}
	req = append(req, user...)
	req = append(req, 0)
	if ip4 == nil {
		req = append(req, host...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks4 request: %w", err)
	}
	resp := make([]byte, 8)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks4 reply: %w", err)
	}
	if resp[1] != 0x5a {
		return fmt.Errorf("socks4: код отказа 0x%02x", resp[1])
	}
	return nil
}

func skipBoundAddr(conn net.Conn, atyp byte) error {
	n := 0
	switch atyp {
	case 0x01:
		n = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := readFull(conn, l); err != nil {
			return err
		}
		n = int(l[0])
	case 0x04:
		n = 16
	default:
		return fmt.Errorf("socks5: тип адреса %d", atyp)
	}
	if _, err := readFull(conn, make([]byte, n+2)); err != nil {
		return fmt.Errorf("socks5 bound addr: %w", err)
	}
	return nil
}

func socksErr(b byte) string {
	switch b {
	case 0x01:
		return "запрещено политикой"
	case 0x02:
		return "общий отказ"
	case 0x03:
		return "сеть недоступна"
	case 0x04:
		return "хост недоступен"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL истёк"
	case 0x07:
		return "команда не поддерживается"
	case 0x08:
		return "тип адреса не поддерживается"
	default:
		return fmt.Sprintf("код 0x%02x", b)
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
