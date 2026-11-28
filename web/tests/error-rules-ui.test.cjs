// Run from web/: node --test tests/error-rules-ui.test.cjs
// feat/error-rules-ui: structured error-rules editor validation + serialization
// mirror backend internal/model/circuit_rules.go ParseCircuitErrorRules semantics.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');
const { readFileSync } = require('node:fs');

const mod = load(require('node:path').join(__dirname, '../src/components/modules/setting/ErrorRulesEditor.tsx'), {
    'next-intl': { useTranslations: () => (k) => k },
    'lucide-react': new Proxy({}, { get: () => 'icon' }),
    '@/components/ui/button': { Button: 'button' },
    '@/components/ui/input': { Input: 'input' },
});
const { validateRule, rulesToJson, parseRulesJson } = mod;

test('validateRule: valid legacy-hold rule passes', () => {
    assert.equal(validateRule({ name: 'x', keywords: ['insufficient_quota'], scope: 'key', cooldown_seconds: 1800 }), null);
});
test('validateRule: valid effect rule passes', () => {
    assert.equal(validateRule({ name: 'x', status_codes: [429], effect: 'cooldown_channel', cooldown_seconds: 60 }), null);
});
test('validateRule: empty condition rejected', () => {
    assert.match(validateRule({ name: 'x' }), /needs status_codes or keywords/);
});
test('validateRule: bad status code rejected', () => {
    assert.match(validateRule({ name: 'x', status_codes: [99], keywords: ['a'] }), /100\.\.599/);
    assert.match(validateRule({ name: 'x', status_codes: [600], keywords: ['a'] }), /100\.\.599/);
});
test('validateRule: legacy rule without scope rejected', () => {
    assert.match(validateRule({ name: 'x', keywords: ['a'], cooldown_seconds: 10 }), /need scope/);
});
test('validateRule: cooldown out of range rejected', () => {
    assert.match(validateRule({ name: 'x', keywords: ['a'], scope: 'key', cooldown_seconds: 0 }), /1\.\.86400/);
    assert.match(validateRule({ name: 'x', keywords: ['a'], scope: 'key', cooldown_seconds: 90000 }), /1\.\.86400/);
});
test('validateRule: too many codes/keywords rejected', () => {
    assert.match(validateRule({ name: 'x', status_codes: Array(21).fill(429), keywords: ['a'] }), /at most 20/);
    assert.match(validateRule({ name: 'x', keywords: Array(21).fill('a'), scope: 'key', cooldown_seconds: 10 }), /at most 20/);
});
test('rulesToJson: empty name backfilled, keywords lowercased/trimmed, empty arrays dropped', () => {
    const json = JSON.parse(rulesToJson([{ name: '  ', keywords: [' Foo ', ''], status_codes: [], cooldown_seconds: 600, scope: 'key' }]));
    assert.equal(json[0].name, 'rule');
    assert.deepEqual(json[0].keywords, ['foo']);
    assert.equal(json[0].status_codes, undefined);
});
test('parseRulesJson + rulesToJson round-trip survives disabled flag', () => {
    const src = '[{"name":"a","keywords":["x"],"scope":"key","cooldown_seconds":10,"disabled":true}]';
    const round = rulesToJson(parseRulesJson(src));
    assert.equal(JSON.parse(round)[0].disabled, true);
});
test('CircuitBreaker wires structured editor and policy keyword setting', () => {
    const src = readFileSync(require('node:path').join(__dirname, '../src/components/modules/setting/CircuitBreaker.tsx'), 'utf8');
    assert.match(src, /ErrorRulesEditor value=\{rules\}/);
    assert.match(src, /relay_content_policy_keywords/);
    // 精确化（pack-1128 合并）：request-audit 的审查 JSON textarea 与规则编辑器同页共存，
    // 原来「textarea 之后不得出现 RULES_PLACEHOLDER」的全文件序断言会误伤它。真正的
    // 不变量没变：规则不再用 textarea 编辑（结构化 ErrorRulesEditor 接管）。
    assert.doesNotMatch(src, /<textarea[^>]*value=\{rules\}/);
});
