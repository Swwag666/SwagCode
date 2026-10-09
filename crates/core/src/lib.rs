/*! Домен-ядро SwagCod.

**Принципиальное ограничение (D-011): этот крейт не зависит от `tauri`.**
Иначе невозможно гонять headless-тесты, бенчи и CLI — а значит невозможно
мерить бюджет производительности из DECISIONS.md §2. Tauri живёт только в
`crates/app` и является тонким адаптером поверх [`bus`].

Слои ядра:
- [`bus`] — шина событий. Единственный путь, которым что-либо доходит до UI.
- [`session`] — домен-типы сессии и хода агента.
- [`turn`] — state machine хода.
*/

pub mod bus;
pub mod context;
pub mod node_store;
pub mod rules;
pub mod semantic;
pub mod session;
pub mod store;
pub mod tasks;
pub mod turn;

pub use bus::{Bus, BusError, Event, EventKind};
pub use rules::{CommandRules, RuleVerdict};
pub use session::{Session, SessionId, SessionStatus, TurnId, TurnRecord};
pub use turn::{
    builtin_tool_specs, ApprovalDecision, ApprovalPolicy, ToolOutcome, TurnConfig, TurnMachine,
    TurnOutcome, TurnReport, TurnStep, DEFAULT_MAX_ITERATIONS, MAX_TOOL_OUTPUT_BYTES,
};
