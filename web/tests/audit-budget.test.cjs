// Run from web/: node --test tests/audit-budget.test.cjs
// 检测第三期：后台检测面板的额度逻辑 + 「面板确实接上了这 6 个设置键」的源码契约。
//   - 已用量只认今天的数：跨天 / 坏 JSON / 负数一律 0，绝不编数字
//   - 轮数只认 1-4，上限只认正整数且封顶 100000（越界等于替用户多花钱）
//   - 布尔设置只认 "true"/"false"，其余用 fallback
//   - 面板绑的是后端已有的键名，不另造键
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const budget = load('src/lib/audit-budget.ts');

const PANEL = path.join(__dirname, '..', 'src', 'components', 'modules', 'model-audit', 'BackgroundAuditPanel.tsx');
const INDEX = path.join(__dirname, '..', 'src', 'components', 'modules', 'model-audit', 'index.tsx');

// 前 4 个是可写的配置键，最后一个是只读的用量。被动检测开关在同页 PassiveProbeCard，面板里刻意不重复。
const KEYS = [
    'model_audit_enabled',
    'model_audit_followup_enabled',
    'model_audit_channel_daily_rounds',
    'model_audit_daily_request_limit',
    'model_audit_budget_usage',
];

test('parseBudgetUsage：只有今天的用量算数，跨天/坏 JSON/负数一律 0', () => {
    const today = '2026-01-05';
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-05","requests":37}', today), 37);
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-05","requests":0}', today), 0);
    // 跨天：昨天的用量今天不算（后台按本地日期滚动额度）。
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-04","requests":37}', today), 0);
    assert.equal(budget.parseBudgetUsage('{"requests":37}', today), 0);
    // 坏 JSON / 空 / 非对象：一律 0，不抛异常。
    assert.equal(budget.parseBudgetUsage(undefined, today), 0);
    assert.equal(budget.parseBudgetUsage('', today), 0);
    assert.equal(budget.parseBudgetUsage('{oops', today), 0);
    assert.equal(budget.parseBudgetUsage('null', today), 0);
    assert.equal(budget.parseBudgetUsage('[]', today), 0);
    assert.equal(budget.parseBudgetUsage('37', today), 0);
    // requests 不是非负数：0（负数不能当成「没用过」之外的意思）。
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-05","requests":-1}', today), 0);
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-05","requests":"37"}', today), 0);
    assert.equal(budget.parseBudgetUsage('{"date":"2026-01-05","requests":null}', today), 0);
});

test('clampRounds：只有 1-4 是轮数，其余按 1', () => {
    assert.equal(budget.clampRounds('1'), 1);
    assert.equal(budget.clampRounds('2'), 2);
    assert.equal(budget.clampRounds('3'), 3);
    assert.equal(budget.clampRounds('4'), 4);
    assert.equal(budget.clampRounds(' 3 '), 3);
    for (const raw of [undefined, '', '0', '5', '9', '-2', '2.5', 'two']) {
        assert.equal(budget.clampRounds(raw), 1, `rounds ${String(raw)} falls back to 1`);
    }
});

test('clampLimit：非正整数 → 200，超过 100000 按 100000', () => {
    assert.equal(budget.clampLimit('200'), 200);
    assert.equal(budget.clampLimit('1'), 1);
    assert.equal(budget.clampLimit('5000'), 5000);
    assert.equal(budget.clampLimit(' 300 '), 300);
    assert.equal(budget.clampLimit('100000'), 100000);
    // 天花板：再大就不是「少量 token 抽检」了。
    assert.equal(budget.clampLimit('200000'), 100000);
    assert.equal(budget.clampLimit('99999999999999999999'), 100000);
    for (const raw of [undefined, '', '0', '-5', '2.5', 'abc']) {
        assert.equal(budget.clampLimit(raw), 200, `limit ${String(raw)} falls back to 200`);
    }
});

test('settingBool：只有 "true"/"false" 算数，其余用 fallback', () => {
    assert.equal(budget.settingBool('true', false), true);
    assert.equal(budget.settingBool('false', true), false);
    // 认不出的写法（旧值、"1"、空串）一律用调用方给的 fallback，不猜。
    for (const raw of [undefined, '', '1', '0', 'on', 'TRUE', ' true']) {
        assert.equal(budget.settingBool(raw, true), true, `settingBool(${String(raw)}) keeps fallback true`);
        assert.equal(budget.settingBool(raw, false), false, `settingBool(${String(raw)}) keeps fallback false`);
    }
});

test('todayLocal：返回本地 YYYY-MM-DD，不是 UTC 日期', () => {
    assert.equal(budget.todayLocal(new Date(2026, 0, 5, 23, 30)), '2026-01-05');
    assert.equal(budget.todayLocal(new Date(2026, 11, 31, 0, 0)), '2026-12-31');
    assert.equal(budget.todayLocal(new Date(2026, 8, 9, 12, 0)), '2026-09-09');
    assert.match(budget.todayLocal(), /^\d{4}-\d{2}-\d{2}$/);
});

test('后台检测面板绑的是这 6 个键，检测页挂载了面板', () => {
    const panel = readFileSync(PANEL, 'utf8');
    for (const key of KEYS) {
        assert.ok(panel.includes(key), `panel must bind ${key}`);
    }
    // 默认值与收窄逻辑只在 lib 里写一次，组件不另造一套。
    assert.match(panel, /from '@\/lib\/audit-budget'/);
    assert.match(panel, /@\/components\/ui\/switch/, 'switches use the project Switch component');
    // 跟查是勾选项：照本模块现有用法用原生 checkbox（检测页探针勾选也是这么写的）。
    assert.match(panel, /type="checkbox"/);
    // 花钱那句不能少：关着的时候后台什么都不会自己发。
    assert.match(panel, /抽检会真的消耗上游额度；关着时后台不会自己发任何检测请求。/);

    const index = readFileSync(INDEX, 'utf8');
    assert.match(index, /import \{ BackgroundAuditPanel \} from '\.\/BackgroundAuditPanel'/);
    assert.match(index, /<BackgroundAuditPanel \/>/);
});
