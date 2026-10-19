/*! Ночной автономный прогон (rev77): контроллер раундов.

Владелец формулирует задачу на ночь, остальное прогон делает сам: ревизии,
ворота, DECISIONS, коммит. Но «сам» не значит «без ограничений» - у прогона есть
цель, кап раундов и бюджет в долларах, и这三个 величины должны где-то
проверяться ДО того, как очередной ход уехал в API.

## Почему модуль чистый, без sleep и таймеров

Расписание и потоки - это оболочка, а решение принимает контроллер: по плану и
состоянию он говорит «продолжать» или называет причину остановки. Чистая функция
проверяется тестами без часов и без сети; обвязку (кто, когда и как вызывает
ходы) строят поверх, и её поломка не касается логики остановки.

## Порядок проверок

Бюджет проверяется ПЕРВЫМ: если деньги кончились, число оставшихся раундов уже
неважно - остановка по деньгам честнее остановки по счётчику, потому что её
причина видна владельцу утром в отчёте.
!*/

/// План ночного прогона: что делать и в каких пределах.
#[derive(Debug, Clone, PartialEq)]
pub struct NightPlan {
    /// Цель словами владельца, без редактуры: она попадает в каждый запрос.
    pub objective: String,
    /// Кап раундов: сколько ходов прогону разрешено сделать максимум.
    pub max_rounds: u32,
    /// Бюджет прогона в долларах. Считается по фактической цене ходов
    /// (`turn_cost_usd`), не по оценкам до начала.
    pub budget_usd: f64,
}

/// Состояние прогона на момент решения о следующем раунде.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct NightState {
    /// Сколько раундов уже сделано.
    pub rounds_done: u32,
    /// Сколько долларов уже потрачено по фактическим ценам ходов.
    pub spent_usd: f64,
}

/// Решение контроллера: продолжать или остановиться - и почему.
#[derive(Debug, Clone, PartialEq)]
pub enum NightVerdict {
    /// Ходов и денег достаточно: раунд можно запускать.
    Continue,
    /// Достигнут кап раундов.
    StopRounds { done: u32 },
    /// Достигнут бюджет: потрачено `spent` из план.
    StopBudget { spent: f64 },
}

impl NightPlan {
    /// План с проверкой здравого смысла: нулевой кап или бюджет - это не
    /// «осторожный прогон», а прогон, который не начнётся. Такой план
    /// отклоняется сразу, чтобы не молчать до первого хода.
    pub fn new(
        objective: impl Into<String>,
        max_rounds: u32,
        budget_usd: f64,
    ) -> Result<Self, String> {
        let objective = objective.into();
        if objective.trim().is_empty() {
            return Err("ночной прогон: цель пустая".to_string());
        }
        if max_rounds == 0 {
            return Err("ночный прогон: кап раундов должен быть > 0, задан 0".to_string());
        }
        /* Порядок важен: NaN сначала отсеивается как нечисло, и только потом
        проверяется знак - иначе NaN проскочил бы сравнение с нулём. */
        if !budget_usd.is_finite() {
            return Err("ночной прогон: бюджет не число".to_string());
        }
        if budget_usd <= 0.0 {
            return Err(format!(
                "ночной прогон: бюджет должен быть > 0, задан {budget_usd}"
            ));
        }
        Ok(Self {
            objective,
            max_rounds,
            budget_usd,
        })
    }

    /// Сколько раундов ещё разрешено при `done` сделанных.
    pub fn rounds_left(&self, done: u32) -> u32 {
        self.max_rounds.saturating_sub(done)
    }
}

/// Решение о следующем раунде. Бюджет проверяется раньше капа: причина
/// остановки по деньгам важнее причины по счётчику.
pub fn verdict(plan: &NightPlan, state: &NightState) -> NightVerdict {
    if state.spent_usd >= plan.budget_usd {
        return NightVerdict::StopBudget {
            spent: state.spent_usd,
        };
    }
    if state.rounds_done >= plan.max_rounds {
        return NightVerdict::StopRounds {
            done: state.rounds_done,
        };
    }
    NightVerdict::Continue
}

/// Инструкция одного раунда: она уезжает текстом хода.
///
/// Модель должна знать, чем ограничена, - иначе она распланирует работу на
/// двадцать ходов, а ей осталось два. Числа в тексте честные: сколько раундов и
/// денег осталось ПОСЛЕ этого, а не сколько было в начале.
pub fn round_prompt(plan: &NightPlan, state: &NightState) -> String {
    let rounds_left = plan.rounds_left(state.rounds_done).saturating_sub(1);
    let rounds_word = if rounds_left == 1 {
        "1 раунд".to_string()
    } else {
        format!("{rounds_left} раундов")
    };
    let budget_left = plan.budget_usd - state.spent_usd;
    format!(
        "Ночной прогон. Цель: {}.\n\
         Это раунд {} из {}; после него останется {} и примерно ${:.2}.\n\
         Правила раунда: сделай одну законченную ревизию - код, ворота \
         (cargo test, clippy, fmt), запись в DECISIONS.md, коммит. \
         Не начинай то, что не закончишь в этом раунде: незаконченное \
         останется в worktree без присмотра.",
        plan.objective,
        state.rounds_done + 1,
        plan.max_rounds,
        rounds_word,
        budget_left,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn plan() -> NightPlan {
        NightPlan::new("закрыть дыры", 10, 5.0).unwrap()
    }

    #[test]
    fn plan_rejects_silent_no_op_runs() {
        /* Нулевой кап или бюджет - прогон, который не начнётся: отклоняем
        сразу, а не молчим до первого хода. */
        assert!(NightPlan::new("", 10, 5.0).is_err());
        assert!(NightPlan::new("цель", 0, 5.0).is_err());
        assert!(NightPlan::new("цель", 10, 0.0).is_err());
        assert!(NightPlan::new("цель", 10, f64::NAN).is_err());
        assert!(NightPlan::new("  ", 1, 1.0).is_err());
        assert!(NightPlan::new("цель", 1, 0.01).is_ok());
    }

    #[test]
    fn verdict_continues_while_both_limits_hold() {
        let p = plan();
        assert_eq!(
            verdict(
                &p,
                &NightState {
                    rounds_done: 0,
                    spent_usd: 0.0
                }
            ),
            NightVerdict::Continue
        );
        assert_eq!(
            verdict(
                &p,
                &NightState {
                    rounds_done: 9,
                    spent_usd: 4.99
                }
            ),
            NightVerdict::Continue
        );
    }

    #[test]
    fn budget_stops_before_rounds_when_both_hit() {
        /* Двойное переполнение: причина - деньги, она важнее счётчика. */
        let p = plan();
        assert_eq!(
            verdict(
                &p,
                &NightState {
                    rounds_done: 10,
                    spent_usd: 5.0
                }
            ),
            NightVerdict::StopBudget { spent: 5.0 }
        );
        assert_eq!(
            verdict(
                &p,
                &NightState {
                    rounds_done: 10,
                    spent_usd: 0.1
                }
            ),
            NightVerdict::StopRounds { done: 10 }
        );
        assert_eq!(
            verdict(
                &p,
                &NightState {
                    rounds_done: 3,
                    spent_usd: 5.5
                }
            ),
            NightVerdict::StopBudget { spent: 5.5 }
        );
    }

    #[test]
    fn rounds_left_saturates() {
        let p = plan();
        assert_eq!(p.rounds_left(0), 10);
        assert_eq!(p.rounds_left(9), 1);
        assert_eq!(p.rounds_left(99), 0, "перебор раундов не уходит в минус");
    }

    #[test]
    fn prompt_states_the_goal_and_honest_remaining_numbers() {
        let p = plan();
        let s = round_prompt(
            &p,
            &NightState {
                rounds_done: 2,
                spent_usd: 1.25,
            },
        );
        assert!(s.contains("закрыть дыры"), "цель попадает в текст: {s}");
        assert!(s.contains("раунд 3 из 10"), "{s}");
        /* После этого раунда останется 7, а не «осталось 8»: числа честные. */
        assert!(s.contains("7 раундов"), "{s}");
        assert!(s.contains("$3.75"), "{s}");
        assert!(s.contains("DECISIONS"), "дисциплина раунда названа: {s}");
        assert!(s.contains("Не начинай"), "{s}");
    }

    #[test]
    fn prompt_on_the_last_round_says_zero_left() {
        let p = plan();
        let s = round_prompt(
            &p,
            &NightState {
                rounds_done: 9,
                spent_usd: 4.9,
            },
        );
        assert!(s.contains("0 раундов"), "{s}");
    }
}
