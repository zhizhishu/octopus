const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { computeBalancePreview, parseAmount, roundBalance, formatUSD } = load('src/components/modules/user/lib/balance-preview.ts');

// 调整余额弹窗的一行预览「现在 x → 改完 y」。规则要和后端 /api/v1/user/balance 一致，
// 否则预览说能减、点确定却被拒（或反过来），管理员会以为钱算错了。

test('add: next = current + amount', () => {
    const p = computeBalancePreview(12.5, 'add', '7.5');
    assert.deepEqual(p, { ok: true, amount: 7.5, next: 20, delta: 7.5 });
});

test('add/subtract: amount must be > 0', () => {
    for (const mode of ['add', 'subtract']) {
        assert.equal(computeBalancePreview(10, mode, '0').error, 'not_positive');
        assert.equal(computeBalancePreview(10, mode, '-3').error, 'not_positive');
        assert.equal(computeBalancePreview(10, mode, '-3').next, 10, 'failed preview keeps current balance');
    }
});

test('subtract: cannot go below zero, exact amount allowed', () => {
    assert.equal(computeBalancePreview(5, 'subtract', '5.01').error, 'not_enough');
    assert.deepEqual(computeBalancePreview(5, 'subtract', '5'), { ok: true, amount: 5, next: 0, delta: -5 });
    assert.deepEqual(computeBalancePreview(5, 'subtract', '1.25'), { ok: true, amount: 1.25, next: 3.75, delta: -1.25 });
});

test('set: replaces balance, zero allowed, negative rejected, delta is the difference', () => {
    assert.deepEqual(computeBalancePreview(5, 'set', '0'), { ok: true, amount: 0, next: 0, delta: -5 });
    assert.deepEqual(computeBalancePreview(5, 'set', '8'), { ok: true, amount: 8, next: 8, delta: 3 });
    assert.equal(computeBalancePreview(5, 'set', '-1').error, 'negative');
});

test('empty and junk input', () => {
    assert.equal(computeBalancePreview(5, 'add', '').error, 'empty');
    assert.equal(computeBalancePreview(5, 'add', '   ').error, 'empty');
    for (const raw of ['abc', '1.2.3', '1e3', 'NaN', 'Infinity', '--1']) {
        assert.equal(computeBalancePreview(5, 'add', raw).error, 'invalid', raw);
    }
});

test('parseAmount accepts $ and thousands separators', () => {
    assert.equal(parseAmount('$1,234.50'), 1234.5);
    assert.equal(parseAmount('.5'), 0.5);
    assert.equal(parseAmount('  3 '), 3);
    assert.equal(parseAmount(''), null);
    assert.ok(Number.isNaN(parseAmount('x')));
});

test('float tails are rounded like the backend (6 decimals)', () => {
    assert.equal(computeBalancePreview(0.1, 'add', '0.2').next, 0.3);
    assert.equal(roundBalance(1.0000004), 1);
    assert.equal(roundBalance(1.0000006), 1.000001);
    // 非法的当前余额（undefined/NaN）当 0 处理，不让预览变成 NaN。
    assert.equal(computeBalancePreview(Number.NaN, 'add', '2').next, 2);
});

test('formatUSD', () => {
    assert.equal(formatUSD(0), '$0.00');
    assert.equal(formatUSD(1234.5), '$1,234.50');
    assert.equal(formatUSD(0.0042), '$0.0042');
    assert.equal(formatUSD(-7), '-$7.00');
    assert.equal(formatUSD(undefined), '$0.00');
});
