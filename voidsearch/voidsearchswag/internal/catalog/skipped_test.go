package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// filePage поднимает сервер, который на главной отдаёт одну ссылку на файл
// содержимого. HEAD отдаёт размер, как это делает настоящий хост.
func filePage(t *testing.T) (host string, closeFn func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="/data/book.epub">книга</a>`))
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "2048")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

func TestCollectCountsFilesItFailedToSave(t *testing.T) {
	// Поле Skipped объявлено в отчёте с начала и сериализуется в JSON как
	// "skipped", но не увеличивалось нигде в пакете: всегда ноль. Случай, который
	// оно обязано описывать, при этом существует - запись файла не прошла.
	//
	// Ошибка записи уходила только в служебный лог, который по умолчанию скрыт,
	// а отчёт выглядел как «ссылок 1, файлов 0». Читается это как «источник без
	// файлов», хотя файл был найден и не записался. В JSON к тому же стояло
	// "skipped": 0, то есть машина получала утверждение «пропусков не было»
	// вместо «пропуски не считались».
	host, closeFn := filePage(t)
	defer closeFn()

	st := newStore(t)
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))

	// Закрытая база - самый прямой способ сделать запись невозможной, не
	// подменяя драйвер и не ломая схему: AddFile начнёт возвращать ошибку на
	// каждой ссылке.
	st.Close()

	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatalf("сбор вернул ошибку: %v", err)
	}
	if rep.Links != 1 {
		t.Fatalf("ссылок %d, ожидала 1: фикстура не нашла файл, тест ничего не проверяет", rep.Links)
	}
	if rep.Saved != 0 {
		t.Errorf("сохранено %d, ожидала 0 при закрытой базе", rep.Saved)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped = %d, ожидала 1: незаписанный файл обязан быть посчитан", rep.Skipped)
	}
}

func TestCollectDoesNotCountSavedFilesAsSkipped(t *testing.T) {
	// Обратная сторона: на здоровой базе пропусков нет, и счётчик обязан
	// остаться нулём. Иначе он начнёт пугать отказом там, где всё записалось.
	host, closeFn := filePage(t)
	defer closeFn()

	st := newStore(t)
	defer st.Close()
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))

	rep, err := c.Collect(context.Background(), []string{host}, "t")
	if err != nil {
		t.Fatalf("сбор вернул ошибку: %v", err)
	}
	if rep.Saved != 1 {
		t.Errorf("сохранено %d, ожидала 1", rep.Saved)
	}
	if rep.Skipped != 0 {
		t.Errorf("Skipped = %d, ожидала 0 на здоровой базе", rep.Skipped)
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d, ожидала 0: хост ответил", rep.Failed)
	}
}

func TestCollectCountsFailedHosts(t *testing.T) {
	// Отказ хоста считается в Failed и в JSON отдаётся, поэтому текстовый отчёт
	// обязан показывать то же число. Здесь проверяется источник данных для
	// печати: два недоступных хоста дают Failed = 2 при Saved = 0.
	st := newStore(t)
	defer st.Close()
	c := NewCollector(newRealClient(t), st, nil, nil, fastCfg(Config{Depth: 1}))

	rep, err := c.Collect(context.Background(),
		[]string{"127.0.0.1:1", "127.0.0.1:2"}, "t")
	if err != nil {
		t.Fatalf("сбор вернул ошибку: %v", err)
	}
	if rep.Hosts != 2 {
		t.Errorf("Hosts = %d, ожидала 2", rep.Hosts)
	}
	if rep.Failed != 2 {
		t.Errorf("Failed = %d, ожидала 2", rep.Failed)
	}
	if rep.Saved != 0 {
		t.Errorf("Saved = %d, ожидала 0", rep.Saved)
	}
}
