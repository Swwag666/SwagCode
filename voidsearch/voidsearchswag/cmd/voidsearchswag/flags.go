package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// isBoolFlag сообщает, принимает ли флаг значение. Пакет flag не даёт прямого
// способа, но булевы флаги реализуют IsBoolFlag, и именно по нему пакет
// решает, не считать ли следующий аргумент значением.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// hoistFlags переставляет флаги, стоящие после первого позиционного аргумента,
// в начало, чтобы пакет flag их увидел.
//
// flag прекращает разбор на первом аргументе без дефиса, поэтому
// `search -mode fast "запрос" -json` молча искал строку «запрос -json» и
// печатал обычный текст вместо JSON. Пользователь узнавал об этом только
// заметив строку «запрос: ...» в выводе. Оба независимых пользовательских
// теста споткнулись именно здесь, и это самый частый способ получить
// неправильный ответ, ничего не нарушив.
//
// Переносятся только токены, которые точно совпали с именем известного флага
// этого FlagSet. Часть запроса, начинающаяся с дефиса («-5 градусов»,
// «c--»), остаётся на месте: иначе исправление порядка аргументов само
// исказило бы запрос. Неизвестный токен с дефисом тоже не трогаем - пусть flag
// сообщит о неизвестном флаге своей обычной внятной ошибкой.
func hoistFlags(fs *flag.FlagSet, args []string) []string {
	var head, tail []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			tail = append(tail, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value := ""
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}
		f := fs.Lookup(name)
		if f == nil {
			tail = append(tail, a)
			continue
		}
		if hasValue {
			head = append(head, "-"+name+"="+value)
			continue
		}
		head = append(head, "-"+name)
		if isBoolFlag(f) {
			continue
		}
		// Небулев флаг забирает следующий токен как значение. Если токена
		// нет, не подставляем ничего - flag сообщит о пропущенном значении.
		if i+1 < len(args) {
			i++
			head = append(head, args[i])
		}
	}
	return append(head, tail...)
}

// strayFlags находит в позиционных аргументах токены, похожие на флаги.
//
// hoistFlags переносит только известные флаги, и это правильно: часть запроса
// может начинаться с дефиса. Но неизвестный токен с дефисом после запроса
// package flag просто не видит - разбор уже остановлен, и «-turbo» молча
// становится частью поисковой строки. Это та же тихая подмена, от которой
// защищает hoistFlags, поэтому о ней надо предупреждать, а не гадать.
//
// Одиночный «-» и разделитель «--» флагами не считаются.
func strayFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if len(a) < 2 || !strings.HasPrefix(a, "-") {
			continue
		}
		if a == "--" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// warnStrayFlags печатает предупреждение, если пользователь поставил флаг
// после запроса и флаг не распознался. Возвращает число подозрительных
// токенов, чтобы вызывающий мог решить, продолжать ли.
func warnStrayFlags(fs *flag.FlagSet, args []string) int {
	stray := strayFlags(fs.Args())
	if len(stray) == 0 {
		return 0
	}
	fmt.Fprintf(os.Stderr, "WARNING: аргументы %v после запроса распознаны как часть запроса, а не как флаги.\n", stray)
	fmt.Fprintf(os.Stderr, "         известные флаги этой команды: ")
	first := true
	fs.VisitAll(func(f *flag.Flag) {
		if !first {
			fmt.Fprint(os.Stderr, ", ")
		}
		first = false
		fmt.Fprintf(os.Stderr, "-%s", f.Name)
	})
	fmt.Fprintln(os.Stderr)
	return len(stray)
}
