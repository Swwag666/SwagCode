package netx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TorEndpoint описывает уже запущенный tor, к которому могут подключаться
// другие процессы.
type TorEndpoint struct {
	Socks   string    `json:"socks"`
	Control string    `json:"control,omitempty"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// TorEndpointPath возвращает путь к файлу с адресами живого tor. Лежит в
// data-каталоге рядом с tor-data, чтобы все процессы одного пользователя
// находили его без настройки.
func TorEndpointPath() string {
	return filepath.Join(DefaultDataDir(), "tor-endpoint.json")
}

// SaveTorEndpoint публикует адреса демона, чтобы следующие вызовы CLI
// подключались к нему вместо собственного bootstrap.
//
// Ошибка записи не считается фатальной для вызывающего: демон поднят и
// работает, потерять от этого возможность им пользоваться было бы глупо.
// Поэтому путь возвращается вместе с ошибкой, а решение принимает вызывающий.
func SaveTorEndpoint(socks, control string) (string, error) {
	path := TorEndpointPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("каталог эндпоинта tor: %w", err)
	}
	ep := TorEndpoint{
		Socks:   socks,
		Control: control,
		PID:     os.Getpid(),
		Started: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return path, fmt.Errorf("сериализация эндпоинта tor: %w", err)
	}
	// Запись через временный файл: процесс, читающий эндпоинт одновременно с
	// нами, должен увидеть либо старую версию целиком, либо новую, но не
	// половину JSON.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return path, fmt.Errorf("запись эндпоинта tor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return path, fmt.Errorf("подмена эндпоинта tor: %w", err)
	}
	return path, nil
}

// LoadTorEndpoint читает опубликованный эндпоинт. Отсутствующий файл - не
// ошибка, а нормальное состояние «демона никто не поднимал».
func LoadTorEndpoint() (TorEndpoint, error) {
	data, err := os.ReadFile(TorEndpointPath())
	if err != nil {
		return TorEndpoint{}, err
	}
	var ep TorEndpoint
	if err := json.Unmarshal(data, &ep); err != nil {
		return TorEndpoint{}, fmt.Errorf("разбор эндпоинта tor: %w", err)
	}
	if strings.TrimSpace(ep.Socks) == "" {
		return TorEndpoint{}, fmt.Errorf("в эндпоинте tor нет socks-адреса")
	}
	return ep, nil
}

// RemoveTorEndpoint убирает файл при остановке демона. Неудача не критична:
// следующая проверка всё равно упрётся в недоступный порт и поднимет свой tor.
func RemoveTorEndpoint() error {
	if err := os.Remove(TorEndpointPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LiveTorEndpoint возвращает эндпоинт только если socks-порт действительно
// отвечает. Файл мог остаться от давно упавшего процесса, и доверять ему
// вслепую значит получить серию таймаутов вместо быстрого старта.
func LiveTorEndpoint(timeout time.Duration) (TorEndpoint, bool) {
	ep, err := LoadTorEndpoint()
	if err != nil {
		return TorEndpoint{}, false
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if err := probeTCP(normalizeHostPort(ep.Socks), timeout); err != nil {
		return TorEndpoint{}, false
	}
	return ep, true
}
