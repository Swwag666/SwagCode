package searchers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"voidsearchswag/internal/httpc"
	"voidsearchswag/internal/netx"
)

// AhmiaClear - clearnet-мост в onion-выдачу: https://ahmia.fi индексирует
// .onion, а его clearnet-фронт отдаёт те же результаты без tor.
// Это опора deep-режима: поиск по даркнету работает даже когда tor-цепи
// лежат или демон не поднят. Сам поиск жжёт только clearnet-канал,
// через tor идут лишь открытие найденных адресов и пробы.
// Разбор - тем же parseOnion с ahmia-селектором: структура выдачи у
// clearnet-фронта и onion-зеркала одна.
type AhmiaClear struct {
	Client  *httpc.Client
	Log     netx.Logger
	BaseURL string
}

func (a *AhmiaClear) Name() string { return "ahmia-clear" }

// ahmiaTimeout - потолок одного поиска через мост. Живой backend отвечает
// за секунды; висящий (504 через ~30с) режется здесь, а не держит deep.
var ahmiaTimeout = 25 * time.Second

func (a *AhmiaClear) base() string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	}
	return "https://ahmia.fi"
}

func (a *AhmiaClear) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if a.Client == nil {
		return nil, errors.New("ahmia-clear: клиент не задан")
	}
	// Свой потолок поверх контекста: backend ahmia висит до своего 504
	// (~30с), и без капа мёртвый мост держит весь deep-прогон. Живой поиск
	// отвечает за секунды, урезание его не задевает.
	ctx, cancel := context.WithTimeout(ctx, ahmiaTimeout)
	defer cancel()
	// Форма поиска требует скрытый anti-spam токен: без него /search/
	// отвечает 302 на главную. Токен читается с главной каждый раз -
	// хардкодить его нельзя, он может смениться.
	tokenName, tokenValue, err := a.searchToken(ctx)
	if err != nil {
		return nil, err
	}
	target := a.base() + "/search/?q=" + url.QueryEscape(query)
	if tokenName != "" {
		target += "&" + url.QueryEscape(tokenName) + "=" + url.QueryEscape(tokenValue)
	}
	resp, err := a.Client.Fetch(ctx, httpc.Request{URL: target, Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("ahmia-clear: %w", err)
	}
	if err := httpc.Classify(resp); err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("ahmia-clear: HTTP %d", resp.Status)
	}
	return parseOnion(resp.Body, a.Name(), ".result h4 a, .result a[href*='.onion']")
}

// searchToken забирает главную и вытаскивает скрытое поле формы поиска.
// Пустой токен - не ошибка: тогда поиск идёт без него, а 302/504 честно
// вернутся из Search как fail движка.
func (a *AhmiaClear) searchToken(ctx context.Context) (string, string, error) {
	resp, err := a.Client.Fetch(ctx, httpc.Request{URL: a.base() + "/", Method: http.MethodGet})
	if err != nil {
		return "", "", fmt.Errorf("ahmia-clear: главная: %w", err)
	}
	if !resp.OK() {
		return "", "", fmt.Errorf("ahmia-clear: главная: HTTP %d", resp.Status)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return "", "", fmt.Errorf("ahmia-clear: разбор главной: %w", err)
	}
	sel := doc.Find(`form[action="/search/"] input[type="hidden"]`).First()
	if sel.Length() == 0 {
		sel = doc.Find(`input[type="hidden"][name]`).First()
	}
	if sel.Length() == 0 {
		return "", "", nil
	}
	name, _ := sel.Attr("name")
	value, _ := sel.Attr("value")
	return strings.TrimSpace(name), strings.TrimSpace(value), nil
}
