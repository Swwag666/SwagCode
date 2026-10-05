package netx

import (
	"net"
	"strings"
)

// AddrIsLoopback сообщает, привяжется ли слушатель с таким адресом только к
// петлевому интерфейсу.
//
// Адресом слушателя бывает ":8080" (пустой хост означает все интерфейсы),
// "0.0.0.0:8080", "[::]:8080", "127.0.0.1:8080", "localhost:8080" или
// "192.168.0.18:8080". Пустой хост и неопределённый адрес считаются
// не-loopback: слушатель на них доступен всей сети, а не только машине.
//
// Имя localhost принимается без резолвинга. Резолвить адрес слушателя до bind
// смысла нет, а ошибка резолвинга не должна превращать локальную настройку в
// открытую, поэтому нерезолвимое имя трактуется как не-loopback и требует
// токена.
//
// Адрес без порта разбирается как хост целиком: net.SplitHostPort на таком
// адресе возвращает ошибку, а слушатель HTTP с адресом без порта поднимется на
// всех интерфейсах указанного хоста.
func AddrIsLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
