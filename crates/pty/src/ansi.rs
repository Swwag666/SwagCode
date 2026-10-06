/*! ANSI-парсер терминального потока (этап B-2).

State-machine над байтами: Ground / Esc / Csi / Osc. Печатные прогоны
отдаются одним срезом `&str`, а не посимвольно, поэтому аллокаций на байт
нет — память живёт только в границах последовательностей. UTF-8 собирается
в маленьком хвосте: обрезанный многобайтовый символ доживает до следующего
фидa, а не превращается в replacement-шум.

Парсер не рисует экран: он раскладывает поток на операции [`Op`], а UI и
эмулятор экрана строят из них что хотят. Это намеренно: одна и та же
разборка корм и вкладку терминала, и будущий полноэкранный эмулятор.
*/

use serde::Serialize;

/// Операция потока для потребителя (UI, эмулятор экрана).
#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(tag = "t", rename_all = "snake_case")]
pub enum Op {
    /// Печатный прогон текста (может содержать \r\n\t).
    Text { s: String },
    /// Смена атрибутов SGR: None означает «не меняли».
    Sgr {
        fg: Option<u8>,
        bg: Option<u8>,
        bold: Option<bool>,
        dim: Option<bool>,
        reset: bool,
    },
    /// Курсорные и стирающие CSI: эмулятор экрана применит, лог-вью игнорирует.
    Csi { kind: String, params: Vec<i64> },
    /// OSC-строка (заголовок окна и прочее): отдаём сырьём.
    Osc { payload: String },
}

/// Приёмник операций: парсер не знает, кто потребитель.
pub trait AnsiSink {
    fn op(&mut self, op: Op);
}

/// Простейший приёмник-коллектор для тестов и лог-вью.
#[derive(Default, Debug)]
pub struct VecSink(pub Vec<Op>);

impl AnsiSink for VecSink {
    fn op(&mut self, op: Op) {
        self.0.push(op);
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Default)]
enum State {
    #[default]
    Ground,
    Esc,
    Csi,
    Osc,
}

/// Парсер с внутренним хвостом: последовательность или UTF-8-символ,
/// разрезанные между фидами, доживают до следующего без потерь.
#[derive(Default, Debug)]
pub struct AnsiParser {
    state: State,
    /// Байты незавершённого UTF-8-символа (не больше 3).
    utf8_tail: Vec<u8>,
    /// Параметры и промежуточные байты текущей CSI/OSC.
    seq: Vec<u8>,
    /// Печатный прогон между последовательностями: растёт до границы.
    run: String,
}

impl AnsiParser {
    pub fn new() -> Self {
        Self::default()
    }

    fn flush_run<S: AnsiSink>(&mut self, sink: &mut S) {
        if !self.run.is_empty() {
            let s = std::mem::take(&mut self.run);
            sink.op(Op::Text { s });
        }
    }

    /// Прогнать очередной кусок байт. Границы кусков могут резать что угодно.
    pub fn feed<S: AnsiSink>(&mut self, data: &[u8], sink: &mut S) {
        let mut i = 0;
        while i < data.len() {
            let b = data[i];
            match self.state {
                State::Ground => {
                    if b == 0x1b {
                        self.flush_run(sink);
                        self.state = State::Esc;
                        i += 1;
                    } else if b == 0x08 || b == 0x07 {
                        // BEL и BS в лог-вью шумят: пропускаем молча.
                        i += 1;
                    } else {
                        // Печатные и пробельные, включая \r\n\t: копим прогоном.
                        // UTF-8: байты >= 0x80 копим в прогон как есть — строка
                        // валидна, пока кусок не разрезал символ; разрезанный
                        // символ уезжает в хвост ниже.
                        let start = i;
                        while i < data.len() && data[i] != 0x1b && data[i] != 0x07 && data[i] != 0x08 {
                            i += 1;
                        }
                        let chunk = &data[start..i];
                        // Хвост прошлого фида + текущий кусок: валидируем вместе.
                        if !self.utf8_tail.is_empty() {
                            let mut joined = std::mem::take(&mut self.utf8_tail);
                            joined.extend_from_slice(chunk);
                            let (valid, rest) = split_utf8(&joined);
                            self.run.push_str(valid);
                            self.utf8_tail = rest.to_vec();
                        } else {
                            let (valid, rest) = split_utf8(chunk);
                            self.run.push_str(valid);
                            self.utf8_tail = rest.to_vec();
                        }
                    }
                }
                State::Esc => {
                    self.state = match b {
                        b'[' => State::Csi,
                        b']' => State::Osc,
                        // Одиночные escape-последовательности (ESC c и т.п.)
                        // в лог-вью не несут смысла: возвращаемся в Ground.
                        _ => State::Ground,
                    };
                    self.seq.clear();
                    i += 1;
                }
                State::Csi => {
                    if (0x40..=0x7e).contains(&b) {
                        let op = csi_op(&self.seq, b);
                        self.seq.clear();
                        self.state = State::Ground;
                        if let Some(op) = op {
                            sink.op(op);
                        }
                        i += 1;
                    } else {
                        self.seq.push(b);
                        i += 1;
                    }
                }
                State::Osc => {
                    if b == 0x07 {
                        let payload = String::from_utf8_lossy(&self.seq).into_owned();
                        self.seq.clear();
                        self.state = State::Ground;
                        sink.op(Op::Osc { payload });
                        i += 1;
                    } else if b == 0x1b {
                        // ST (ESC \): следующий байт доедаем как терминатор.
                        self.state = State::Esc;
                        let payload = String::from_utf8_lossy(&self.seq).into_owned();
                        self.seq.clear();
                        sink.op(Op::Osc { payload });
                        i += 1;
                    } else {
                        self.seq.push(b);
                        i += 1;
                    }
                }
            }
        }
    }

    /// Догнать хвосты в конце потока: незакрытый прогон тоже текст.
    pub fn finish<S: AnsiSink>(&mut self, sink: &mut S) {
        if !self.utf8_tail.is_empty() {
            let joined = std::mem::take(&mut self.utf8_tail);
            let (valid, _) = split_utf8(&joined);
            self.run.push_str(valid);
        }
        self.flush_run(sink);
        self.state = State::Ground;
        self.seq.clear();
    }
}

/// Разделить буфер на валидный UTF-8 и неполный хвост (до 3 байт).
fn split_utf8(buf: &[u8]) -> (&str, &[u8]) {
    match std::str::from_utf8(buf) {
        Ok(s) => (s, &[]),
        Err(e) => {
            let valid = e.valid_up_to();
            // SAFETY-эквивалент: valid_up_to гарантирует валидность префикса.
            let s = std::str::from_utf8(&buf[..valid]).unwrap_or("");
            (s, &buf[valid..])
        }
    }
}

/// Разобрать тело CSI (параметры и финальный байт) в операцию.
fn csi_op(seq: &[u8], final_byte: u8) -> Option<Op> {
    // Параметры: всё до первого байта из 0x3c-0x3f включительно игнорируем
    // как private-маркеры, цифры и ';' копим.
    let param_bytes: Vec<u8> = seq
        .iter()
        .copied()
        .filter(|b| b.is_ascii_digit() || *b == b';')
        .collect();
    let param_str = String::from_utf8_lossy(&param_bytes);
    let params: Vec<i64> = param_str
        .split(';')
        .filter(|p| !p.is_empty())
        .filter_map(|p| p.parse::<i64>().ok())
        .collect();

    match final_byte {
        b'm' => Some(sgr_op(&params)),
        b'A' => Some(Op::Csi { kind: "cuu".into(), params }),
        b'B' => Some(Op::Csi { kind: "cud".into(), params }),
        b'C' => Some(Op::Csi { kind: "cuf".into(), params }),
        b'D' => Some(Op::Csi { kind: "cub".into(), params }),
        b'H' | b'f' => Some(Op::Csi { kind: "cup".into(), params }),
        b'J' => Some(Op::Csi { kind: "ed".into(), params }),
        b'K' => Some(Op::Csi { kind: "el".into(), params }),
        b'S' => Some(Op::Csi { kind: "su".into(), params }),
        b'T' => Some(Op::Csi { kind: "sd".into(), params }),
        // Неизвестные CSI молча съедаем: лог-вью не должно ломаться на
        // последовательностях, которых мы ещё не знаем.
        _ => None,
    }
}

/// SGR: разбираем ходовые атрибуты, остальное игнорируем без паники.
fn sgr_op(params: &[i64]) -> Op {
    let mut fg = None;
    let mut bg = None;
    let mut bold = None;
    let mut dim = None;
    let mut reset = false;
    let mut i = 0;
    while i < params.len() {
        match params[i] {
            0 => reset = true,
            1 => bold = Some(true),
            2 => dim = Some(true),
            22 => {
                bold = Some(false);
                dim = Some(false);
            }
            30..=37 => fg = Some((params[i] - 30) as u8),
            39 => fg = None,
            40..=47 => bg = Some((params[i] - 40) as u8),
            49 => bg = None,
            90..=97 => fg = Some((params[i] - 90 + 8) as u8),
            100..=107 => bg = Some((params[i] - 100 + 8) as u8),
            _ => {}
        }
        i += 1;
    }
    if params.is_empty() {
        reset = true;
    }
    Op::Sgr { fg, bg, bold, dim, reset }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(data: &[u8]) -> Vec<Op> {
        let mut p = AnsiParser::new();
        let mut sink = VecSink::default();
        p.feed(data, &mut sink);
        p.finish(&mut sink);
        sink.0
    }

    #[test]
    fn plain_text_is_one_run() {
        let ops = parse(b"hello world\r\n");
        assert_eq!(ops, vec![Op::Text { s: "hello world\r\n".into() }]);
    }

    #[test]
    fn sgr_color_parsed_and_text_split() {
        let ops = parse(b"\x1b[31mred\x1b[0m plain");
        assert_eq!(
            ops,
            vec![
                Op::Sgr { fg: Some(1), bg: None, bold: None, dim: None, reset: false },
                Op::Text { s: "red".into() },
                Op::Sgr { fg: None, bg: None, bold: None, dim: None, reset: true },
                Op::Text { s: " plain".into() },
            ]
        );
    }

    #[test]
    fn sequence_split_across_feeds_survives() {
        let mut p = AnsiParser::new();
        let mut sink = VecSink::default();
        p.feed(b"\x1b[3", &mut sink);
        p.feed(b"1mred", &mut sink);
        p.finish(&mut sink);
        assert_eq!(
            sink.0,
            vec![
                Op::Sgr { fg: Some(1), bg: None, bold: None, dim: None, reset: false },
                Op::Text { s: "red".into() },
            ]
        );
    }

    #[test]
    fn utf8_split_across_feeds_survives() {
        let mut p = AnsiParser::new();
        let mut sink = VecSink::default();
        let s = "привет".as_bytes();
        p.feed(&s[..4], &mut sink); // режем многобайтовый символ
        p.feed(&s[4..], &mut sink);
        p.finish(&mut sink);
        let text: String = sink
            .0
            .iter()
            .filter_map(|o| match o {
                Op::Text { s } => Some(s.clone()),
                _ => None,
            })
            .collect();
        assert_eq!(text, "привет");
    }

    #[test]
    fn osc_title_captured_until_bel() {
        let ops = parse(b"\x1b]0;SwagCod: bash\x07after");
        assert_eq!(
            ops,
            vec![
                Op::Osc { payload: "0;SwagCod: bash".into() },
                Op::Text { s: "after".into() },
            ]
        );
    }

    #[test]
    fn cursor_and_erase_csi_carry_params() {
        let ops = parse(b"\x1b[2J\x1b[12;4H");
        assert_eq!(ops[0], Op::Csi { kind: "ed".into(), params: vec![2] });
        assert_eq!(ops[1], Op::Csi { kind: "cup".into(), params: vec![12, 4] });
    }

    #[test]
    fn unknown_csi_is_swallowed_silently() {
        let ops = parse(b"a\x1b[?25hb");
        assert_eq!(
            ops,
            vec![Op::Text { s: "a".into() }, Op::Text { s: "b".into() }]
        );
    }

    #[test]
    fn bold_and_dim_flags() {
        let ops = parse(b"\x1b[1;2m x");
        assert_eq!(
            ops[0],
            Op::Sgr { fg: None, bg: None, bold: Some(true), dim: Some(true), reset: false }
        );
    }
}
