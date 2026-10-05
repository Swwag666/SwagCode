package main

import (
	"os"
	"strings"
	"testing"
)

// Инвариант офлайн-сьюта: провайдер прокси в setOfflineProbeEnv обязан
// смотреть на локальный адрес. Регрессия этой строки тихо возвращает
// poolcheck-тесты на публичный api.proxyscrape.com: с сетью тесты
// проходят и не выдают зависимость, без сети - падают EOF'ом, а в
// самолёте (CI без внешней сети) - рушат весь пакет. Живой замер ДО:
// TestPoolcheckFlagsOverrideConfigValues падал «ERROR: пул:
// proxyscrape/http: ... EOF» с рук внешнего сервиса.
func TestSetOfflineProbeEnvKeepsProviderLocal(t *testing.T) {
	dir := t.TempDir()
	setOfflineProbeEnv(t, dir)

	url := os.Getenv("VOIDSEARCH_PROXY_PROVIDER_URL")
	if url == "" {
		t.Fatal("провайдер прокси не подменён: тесты уйдут на публичный endpoint")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("провайдер %q не локальный: офлайн-прогон зависит от внешней сети", url)
	}
}
