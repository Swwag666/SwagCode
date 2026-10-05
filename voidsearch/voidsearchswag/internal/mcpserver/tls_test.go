package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voidsearchswag/internal/metrics"
)

// selfSignedCert генерирует пару cert/key для тестов TLS.
func selfSignedCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "voidsearch-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	certOut.Close()
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatal(err)
	}
	keyOut.Close()
	return certPath, keyPath
}

func tlsClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // тестовый клиент к тестовому серверу
		},
	}
}

func TestServeTLSNeedsPair(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Только cert без ключа - ошибка конфигурации, а не висящий сервер.
	if err := ServeWithOptions(ctx, srv, HTTPOptions{Addr: freeAddr(t), CertFile: "/tmp/x.pem"}); err == nil {
		t.Error("TLS без ключа принят")
	}
}

func TestServeTLSAndMetricsGated(t *testing.T) {
	mc := metrics.New()
	mc.Inc("search_total")
	mc.Inc("search_total")
	srv := New(Deps{Version: "test", Started: time.Now()})
	cert, key := selfSignedCert(t)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeWithOptions(ctx, srv, HTTPOptions{
			Addr: addr, Token: "s3cret", CertFile: cert, KeyFile: key, Metrics: mc,
		})
	}()
	base := "https://" + addr
	cl := tlsClient()
	// Ждём готовности TLS-сервера.
	var ok bool
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if resp, err := cl.Get(base + "/health"); err == nil {
			resp.Body.Close()
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("TLS-сервер не поднялся")
	}
	// /metrics без токена - 401.
	if resp, err := cl.Get(base + "/metrics"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("/metrics без токена: %d, ожидала 401", resp.StatusCode)
		}
	}
	// /metrics с токеном - счётчики сервера.
	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["search_total"] != float64(2) {
		t.Errorf("счётчик потерян: %+v", out)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("Serve не остановился по cancel")
	}
}

func TestMetricsOpenWithoutToken(t *testing.T) {
	srv := New(Deps{Version: "test", Started: time.Now()})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, addr) }()
	base := "http://" + addr
	var ok bool
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("сервер не поднялся")
	}
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics без auth на открытом сервере: %d", resp.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Error("Serve не остановился по cancel")
	}
}
