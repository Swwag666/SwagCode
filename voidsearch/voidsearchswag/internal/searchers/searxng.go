package searchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
)

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// SearXNG - опциональный пользовательский метапоисковик без ключей:
// свой или чужой публичный инстанс отдаёт JSON по ?format=json.
// Включается только заданным VOIDSEARCH_SEARXNG_URL, иначе движка нет
// вовсе - чужой инстанс по умолчанию был бы внешней зависимостью.
type SearXNG struct {
	Client  *httpc.Client
	Log     netx.Logger
	BaseURL string
}

func (s *SearXNG) Name() string { return "searxng" }

func (s *SearXNG) base() string {
	return strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
}

func (s *SearXNG) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if s.Client == nil {
		return nil, errors.New("searxng: клиент не задан")
	}
	if s.base() == "" {
		return nil, errors.New("searxng: инстанс не задан (VOIDSEARCH_SEARXNG_URL)")
	}
	target := s.base() + "/search?q=" + url.QueryEscape(query) + "&format=json"
	resp, err := s.Client.Fetch(ctx, httpc.Request{URL: target, Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	if err := httpc.Classify(resp); err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("searxng: HTTP %d", resp.Status)
	}
	return parseSearXNG(resp.Body, s.Name())
}

func parseSearXNG(body []byte, source string) ([]Result, error) {
	var sr searxngResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("searxng: разбор JSON: %w", err)
	}
	out := make([]Result, 0, len(sr.Results))
	for _, r := range sr.Results {
		href := strings.TrimSpace(r.URL)
		u, perr := url.Parse(href)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		out = append(out, Result{
			Title:   collapse(r.Title),
			URL:     href,
			Snippet: collapse(r.Content),
			Source:  source,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("searxng: результатов нет")
	}
	return out, nil
}
