/*! PTY-подсистема: ConPTY на Windows, forkpty на Unix.

Этап 2. Здесь будет то, что даёт главный выигрыш в скорости против
Electron + node-pty + xterm.js: собственный ANSI-парсер, кольцевой буфер
вывода и flow control, чтобы вывод 50 МБ/с не дропал кадры UI.

Сейчас — заготовка модуля и контракт, чтобы `core` и UI могли
компилироваться против него.
*/

/// Требуемая пропускная способность вывода без дропа фреймов (DECISIONS.md §2).
pub const TARGET_THROUGHPUT_BYTES_PER_SEC: usize = 50 * 1024 * 1024;

/// Размер кольцевого буфера скроллбека на одну сессию терминала.
///
/// Ограничен намеренно: неограниченный скроллбек на бурном выводе билда —
/// это ровно та утечка, из-за которой хост распухает за часы работы.
pub const DEFAULT_SCROLLBACK_BYTES: usize = 8 * 1024 * 1024;

/// Кольцевой буфер вывода терминала фиксированного объёма.
///
/// Старые данные перезаписываются новыми, аллокаций после заполнения нет —
/// поэтому память сессии ограничена сверху и не растёт со временем.
#[derive(Debug)]
pub struct RingBuffer {
    buf: Vec<u8>,
    /// Позиция записи.
    write: usize,
    /// Сколько байт занято (может быть меньше `buf.len()` до первого оборота).
    filled: usize,
    /// Сколько байт всего было записано за жизнь буфера — для диагностики.
    total_written: u64,
}

impl RingBuffer {
    pub fn new(capacity: usize) -> Self {
        assert!(capacity > 0, "ёмкость буфера должна быть положительной");
        Self {
            buf: vec![0u8; capacity],
            write: 0,
            filled: 0,
            total_written: 0,
        }
    }

    pub fn capacity(&self) -> usize {
        self.buf.len()
    }

    pub fn len(&self) -> usize {
        self.filled
    }

    pub fn is_empty(&self) -> bool {
        self.filled == 0
    }

    pub fn total_written(&self) -> u64 {
        self.total_written
    }

    /// Дописать данные. При переполнении старое затирается — аллокаций нет.
    pub fn push(&mut self, data: &[u8]) {
        let cap = self.buf.len();
        self.total_written = self.total_written.saturating_add(data.len() as u64);

        if data.len() >= cap {
            // Данные больше всего буфера: оставляем только хвост.
            let tail = &data[data.len() - cap..];
            self.buf.copy_from_slice(tail);
            self.write = 0;
            self.filled = cap;
            return;
        }

        let first = (cap - self.write).min(data.len());
        self.buf[self.write..self.write + first].copy_from_slice(&data[..first]);
        if first < data.len() {
            let rest = data.len() - first;
            self.buf[..rest].copy_from_slice(&data[first..]);
        }
        self.write = (self.write + data.len()) % cap;
        self.filled = (self.filled + data.len()).min(cap);
    }

    /// Текущее содержимое в хронологическом порядке.
    pub fn snapshot(&self) -> Vec<u8> {
        if self.filled < self.buf.len() {
            return self.buf[..self.filled].to_vec();
        }
        // Буфер обернулся: хвост (от write до конца) идёт раньше начала.
        let mut out = Vec::with_capacity(self.buf.len());
        out.extend_from_slice(&self.buf[self.write..]);
        out.extend_from_slice(&self.buf[..self.write]);
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_buffer_is_empty() {
        let b = RingBuffer::new(16);
        assert!(b.is_empty());
        assert_eq!(b.len(), 0);
        assert_eq!(b.capacity(), 16);
    }

    #[test]
    fn push_keeps_data_in_order() {
        let mut b = RingBuffer::new(16);
        b.push(b"hello ");
        b.push(b"world");
        assert_eq!(b.snapshot(), b"hello world".to_vec());
        assert_eq!(b.len(), 11);
    }

    #[test]
    fn overflow_drops_oldest_keeps_capacity() {
        let mut b = RingBuffer::new(8);
        b.push(b"01234567"); // ровно заполнили
        assert_eq!(b.len(), 8);
        b.push(b"ab"); // оборот: уходят '0','1'
        assert_eq!(b.len(), 8, "ёмкость не должна расти");
        assert_eq!(b.snapshot(), b"234567ab".to_vec());
    }

    #[test]
    fn data_larger_than_capacity_keeps_tail() {
        let mut b = RingBuffer::new(4);
        b.push(b"abcdefgh");
        assert_eq!(b.snapshot(), b"efgh".to_vec());
        assert_eq!(b.len(), 4);
    }

    #[test]
    fn memory_is_bounded_after_many_writes() {
        // Это и есть защита от распухания на длинной сессии.
        let mut b = RingBuffer::new(DEFAULT_SCROLLBACK_BYTES);
        let chunk = vec![b'x'; 64 * 1024];
        for _ in 0..1000 {
            b.push(&chunk);
        }
        assert_eq!(b.len(), DEFAULT_SCROLLBACK_BYTES);
        assert_eq!(b.snapshot().len(), DEFAULT_SCROLLBACK_BYTES);
        assert_eq!(b.total_written(), 1000 * 64 * 1024);
    }

    #[test]
    fn wraparound_snapshot_is_contiguous() {
        let mut b = RingBuffer::new(10);
        b.push(b"12345678");
        b.push(b"90ab");
        assert_eq!(b.snapshot(), b"34567890ab".to_vec());
        assert_eq!(b.snapshot().len(), 10);
    }

    #[test]
    fn repeated_wraparounds_stay_consistent() {
        let mut b = RingBuffer::new(7);
        for i in 0..50u8 {
            b.push(&[i]);
        }
        let snap = b.snapshot();
        assert_eq!(snap.len(), 7);
        // Последние 7 записанных значений.
        assert_eq!(snap, vec![43, 44, 45, 46, 47, 48, 49]);
    }

    #[test]
    fn empty_push_is_noop() {
        let mut b = RingBuffer::new(8);
        b.push(b"abc");
        b.push(b"");
        assert_eq!(b.snapshot(), b"abc".to_vec());
    }

    #[test]
    #[should_panic(expected = "ёмкость буфера должна быть положительной")]
    fn zero_capacity_is_rejected() {
        RingBuffer::new(0);
    }

    #[test]
    fn target_throughput_matches_budget() {
        assert_eq!(TARGET_THROUGHPUT_BYTES_PER_SEC, 50 * 1024 * 1024);
    }
}
