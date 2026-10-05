package searchers

import "strings"

// DropQueryMiss делит выдачу на результаты с хоть одним токеном запроса и
// все остальные. Deep-индексы охотно возвращают по любому запросу свои
// витрины, у которых с запросом нет ничего общего: жалоба смоук-агентов на
// v0.1.0 - запрос «acme corp leak database» приносил leak-маркеты без
// единого слова из запроса, и мусор стоял в выдаче рядом с честными
// находками.
//
// Сравнение подстроками, а не морфологией: у «acme corp leak database»
// токены короткие и точные, и Contains по нижнему регистру ловит их во всех
// формах. Морфология добила бы разве что русские падежи, а ценой стала бы
// зависимость от стеммера и ложные пропуски на составных словах.
//
// Снятие фильтра при пустом kept - решение вызывающего, не функции: «нет ни
// одного совпадения» и «выдача пуста» - разные состояния, и подменять одно
// другим на уровне сравнения строк нельзя.
func DropQueryMiss(in []Result, query string) (kept, dropped []Result) {
	toks := queryTokens(query)
	if len(toks) == 0 || len(in) == 0 {
		return in, nil
	}
	for _, r := range in {
		if resultHitsQuery(r, toks) {
			kept = append(kept, r)
		} else {
			dropped = append(dropped, r)
		}
	}
	return kept, dropped
}

// resultHitsQuery проверяет пересечение результата с токенами запроса по
// тем же трём полям, что и score в реранке: заголовок, сниппет, адрес.
// Совпадение в любом одном поле достаточно: адрес вида
// http://acmecorp.onion/markets релевантен запросу «acme» даже с пустым
// заголовком, и требовать слово в тексте страницы значило бы выкидывать
// рабочие ссылки из скудных каталогов.
func resultHitsQuery(r Result, toks []string) bool {
	title := strings.ToLower(r.Title)
	snip := strings.ToLower(r.Snippet)
	uri := strings.ToLower(r.URL)
	for _, t := range toks {
		if strings.Contains(title, t) ||
			strings.Contains(snip, t) ||
			strings.Contains(uri, t) {
			return true
		}
	}
	return false
}
