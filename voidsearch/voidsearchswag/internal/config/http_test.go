package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadReadsHTTPToken(t *testing.T) {
	t.Setenv("VOIDSEARCH_HTTP_TOKEN", "s3cret")
	t.Setenv("VOIDSEARCH_HTTP_ADDR", ":8800")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPToken != "s3cret" {
		t.Errorf("HTTPToken=%q", c.HTTPToken)
	}
	if c.HTTPAddr != ":8800" {
		t.Errorf("HTTPAddr=%q", c.HTTPAddr)
	}
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOIDSEARCH_CONFIG", p)
	return p
}

func TestConfigFileAppliesUnsetKeys(t *testing.T) {
	writeConfigFile(t, `{"VOIDSEARCH_RESULT_LIMIT": "77", "VOIDSEARCH_TOR": "off"}`)
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "")
	t.Setenv("VOIDSEARCH_TOR", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit != 77 {
		t.Errorf("ResultLimit=%d, ожидала 77 из файла", c.ResultLimit)
	}
	if c.TorEnabled() {
		t.Error("tor из файла не выключен")
	}
}

func TestConfigEnvWinsOverFile(t *testing.T) {
	writeConfigFile(t, `{"VOIDSEARCH_RESULT_LIMIT": "77"}`)
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "55")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit != 55 {
		t.Errorf("ResultLimit=%d, ожидала 55 из окружения", c.ResultLimit)
	}
}

func TestConfigFileIgnoresGarbage(t *testing.T) {
	writeConfigFile(t, `{"VOIDSEARCH_RESULT_LIMIT": "66", "MUSOR": "x", "": "y"}`)
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit != 66 {
		t.Errorf("ResultLimit=%d", c.ResultLimit)
	}
}

func TestConfigFileBrokenJSONIgnored(t *testing.T) {
	writeConfigFile(t, `{битый`)
	t.Setenv("VOIDSEARCH_RESULT_LIMIT", "")

	// Битый файл не должен ронять загрузку: дефолты остаются.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ResultLimit != 20 {
		t.Errorf("ResultLimit=%d, ожидала дефолт 20", c.ResultLimit)
	}
}

func TestBackgroundDefaults(t *testing.T) {
	t.Setenv("VOIDSEARCH_HUNT_BG", "")
	t.Setenv("VOIDSEARCH_DISCOVER_BG", "")
	t.Setenv("VOIDSEARCH_SEARXNG_URL", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.HuntBG || !c.DiscoverBG {
		t.Error("фоновые циклы выключены по умолчанию")
	}
	if c.HuntInterval != 10*time.Minute {
		t.Errorf("HuntInterval=%v", c.HuntInterval)
	}
	if c.DiscoverBGInterval != 24*time.Hour {
		t.Errorf("DiscoverBGInterval=%v", c.DiscoverBGInterval)
	}
	if c.SearXNGURL != "" {
		t.Errorf("SearXNGURL=%q, ожидала пусто", c.SearXNGURL)
	}
}
