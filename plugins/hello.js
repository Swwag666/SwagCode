/* Демо-плагин SwagCod (E-4): обычный JS в каталоге plugins/.
 *
 * Файл исполняется в песочнице vm sidecar-процесса (plugin-server.js):
 * сеть и fs недоступны, handler обязан отвечать в дедлайн (синхронный
 * код убивается по таймауту). Из хранилища плагин видит ТОЛЬКО prefs,
 * только на чтение: second-аргумент handler — { prefs }.
 *
 * Модель вызывает инструмент по имени js:hello. При политике OnDangerous
 * вызов уходит на подтверждение человеку, журнал пишет имя с префиксом js:.
 */
swagcod.define({
  name: 'hello',
  description: 'Демо-плагин: здоровается на русском или английском',
  parameters: {
    type: 'object',
    properties: {
      name: { type: 'string', description: 'Кого приветствовать' },
      lang: { type: 'string', enum: ['ru', 'en'], description: 'Язык приветствия' },
    },
  },
  handler: (args) => {
    const name = String(args.name || '').trim() || 'мир';
    return args.lang === 'en' ? `Hello, ${name}!` : `Привет, ${name}!`;
  },
});
