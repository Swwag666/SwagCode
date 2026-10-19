/*! Цена хода: токены в деньги.

В приложении считаются токены (`turns.est_in/est_out`), но не деньги, а владельцу
нужен именно счёт: токены сами по себе не говорят, во что обошёлся ночной прогон.

## Что здесь есть и чего нет

Здесь только арифметика и таблица цен. Ничего не ходит в сеть и ничего не
спрашивает у провайдера: цена считается из уже посчитанных токенов и названия
модели.

Таблица **справочная**. Цены меняются, у провайдеров бывают скидки за кэш и за
объём, поэтому цифра здесь - ориентир, а не счёт к оплате. Источник истины -
прайс провайдера; расходится - правится таблица, а не формула.

## Неизвестная модель - это не ноль

Модели нет в таблице - цена [`None`], и [`cost_note`] говорит «неизвестна».
Ноль был бы ложью в опасную сторону: бесплатный ночной прогон выглядит как
разрешённый, а на деле просто не посчитан.
!*/

/// Цена модели за миллион токенов, в долларах США.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Rate {
    /// Искомый фрагмент названия модели, в нижнем регистре.
    ///
    /// Не только начало: у Anthropic семейство стоит в середине имени
    /// (`claude-3-5-sonnet-20241022`), и совпадение по началу промахивалось бы.
    pub needle: &'static str,
    /// Долларов за миллион входных токенов.
    pub in_per_1m: f64,
    /// Долларов за миллион выходных токенов.
    pub out_per_1m: f64,
}

/// Справочные цены (доллары за миллион токенов).
///
/// Порядок не важен: побеждает самый длинный совпавший фрагмент, поэтому
/// `gpt-4o-mini` перебивает `gpt-4o`, а `deepseek-reasoner` - `deepseek`.
pub const RATES: &[Rate] = &[
    Rate {
        needle: "opus",
        in_per_1m: 15.0,
        out_per_1m: 75.0,
    },
    Rate {
        needle: "sonnet",
        in_per_1m: 3.0,
        out_per_1m: 15.0,
    },
    Rate {
        needle: "haiku",
        in_per_1m: 0.80,
        out_per_1m: 4.0,
    },
    Rate {
        needle: "gpt-4o-mini",
        in_per_1m: 0.15,
        out_per_1m: 0.60,
    },
    Rate {
        needle: "gpt-4o",
        in_per_1m: 2.50,
        out_per_1m: 10.0,
    },
    Rate {
        needle: "deepseek-reasoner",
        in_per_1m: 0.55,
        out_per_1m: 2.19,
    },
    Rate {
        needle: "deepseek-chat",
        in_per_1m: 0.27,
        out_per_1m: 1.10,
    },
];

/// Цена модели за миллион токенов, если модель есть в таблице.
///
/// Совпадение по фрагменту без учёта регистра: провайдеры дописывают к имени дату
/// версии (`claude-3-5-sonnet-20241022`), а семейство у Anthropic стоит в
/// середине имени, поэтому и `starts_with`, и точное равенство промахивались бы.
pub fn rate_for(model: &str) -> Option<Rate> {
    let needle = model.trim().to_ascii_lowercase();
    if needle.is_empty() {
        return None;
    }
    RATES
        .iter()
        .filter(|r| needle.contains(r.needle))
        /* Самый длинный фрагмент: `gpt-4o-mini` не должен считаться по `gpt-4o`. */
        .max_by_key(|r| r.needle.len())
        .copied()
}

/// Стоимость хода в долларах.
///
/// `None` - модели нет в таблице (см. [`rate_for`]). Ноль токенов - это honest
/// `Some(0.0)`: запрос не стоил денег, и это известно точно.
pub fn cost_usd(model: &str, tokens_in: u64, tokens_out: u64) -> Option<f64> {
    let rate = rate_for(model)?;
    let cost = (tokens_in as f64 / 1_000_000.0) * rate.in_per_1m
        + (tokens_out as f64 / 1_000_000.0) * rate.out_per_1m;
    // Отрицательных сумм не бывает, а NaN из таблицы не должен доехать до отчёта.
    if cost.is_finite() {
        Some(cost.max(0.0))
    } else {
        None
    }
}

/// Деньги для человека: `$0.0123` для мелких сумм, `$1.23` для крупных.
pub fn format_usd(amount: f64) -> String {
    if !amount.is_finite() {
        return "цена неизвестна".to_string();
    }
    if amount > 0.0 && amount < 0.10 {
        format!("${amount:.4}")
    } else {
        format!("${amount:.2}")
    }
}

/// Готовая строка для журнала: «цена: ~$0.0123» или честное «неизвестна».
///
/// Формат один для всех мест отчёта, чтобы цифра в журнале и цифра в интерфейсе
/// не расходилась в написании.
pub fn cost_note(model: &str, tokens_in: u64, tokens_out: u64) -> String {
    match cost_usd(model, tokens_in, tokens_out) {
        Some(v) => format!("цена: ~{}", format_usd(v)),
        None => "цена: неизвестна (модели нет в таблице цен)".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cost_is_linear_in_tokens() {
        // Ровно миллион входных токенов у sonnet - ровно его входная цена.
        let a = cost_usd("claude-sonnet-4", 1_000_000, 0).unwrap();
        assert!((a - 3.0).abs() < 1e-9, "вход: {a}");
        let b = cost_usd("claude-sonnet-4", 0, 1_000_000).unwrap();
        assert!((b - 15.0).abs() < 1e-9, "выход: {b}");
        // И вдвое меньше токенов - вдвое меньше денег.
        let half = cost_usd("claude-sonnet-4", 500_000, 0).unwrap();
        assert!((half - 1.5).abs() < 1e-9, "половина: {half}");
    }

    #[test]
    fn zero_tokens_is_zero_money_not_unknown() {
        assert_eq!(cost_usd("gpt-4o", 0, 0), Some(0.0));
    }

    #[test]
    fn unknown_model_has_no_price() {
        assert!(rate_for("some-random-model").is_none());
        assert!(cost_usd("some-random-model", 1000, 1000).is_none());
        assert!(rate_for("").is_none(), "пустое имя - не модель");
    }

    #[test]
    fn longest_prefix_wins() {
        /* Дата версии в имени не должна промахиваться, а mini не должен
        считаться по цене старшей модели: ошибка здесь завысила бы счёт в
        семнадцать раз. */
        let sonnet = rate_for("claude-3-5-sonnet-20241022").unwrap();
        assert_eq!(sonnet.needle, "sonnet");
        let mini = rate_for("gpt-4o-mini-2024-07-18").unwrap();
        assert_eq!(mini.needle, "gpt-4o-mini");
        /* Запись gpt-4o стоит в таблице позже mini, и порядок не должен влиять:
        сортировки по позиции нет, есть выбор самого длинного совпадения. */
        let full = rate_for("gpt-4o").unwrap();
        assert_eq!(full.needle, "gpt-4o");
        assert!(mini.in_per_1m < full.in_per_1m);
    }

    #[test]
    fn matching_ignores_case_and_spaces() {
        assert_eq!(
            rate_for("  Claude-Sonnet-4 ").map(|r| r.needle),
            Some("sonnet")
        );
    }

    #[test]
    fn small_amounts_keep_four_digits() {
        assert_eq!(format_usd(0.0123), "$0.0123");
        assert_eq!(format_usd(0.0), "$0.00");
        assert_eq!(format_usd(1.234), "$1.23");
    }

    #[test]
    fn note_about_unknown_model_does_not_say_zero() {
        /* Ноль вместо «неизвестна» читался бы как «прогон бесплатный» - это
        ошибка в опасную сторону. */
        let note = cost_note("model-nobody-knows", 10_000, 2_000);
        assert!(note.contains("неизвестна"), "{note}");
        assert!(!note.contains("$0"), "ноль не выдаётся за цену: {note}");
    }

    #[test]
    fn note_for_known_model_carries_the_amount() {
        let note = cost_note("gpt-4o", 1_000_000, 0);
        assert_eq!(note, "цена: ~$2.50");
    }
}
