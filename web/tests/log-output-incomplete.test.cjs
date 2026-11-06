// Run from web/: node --test tests/log-output-incomplete.test.cjs
//
// 「输出不完整」只在最终那次成功尝试上加标记：竞速落选/失败的尝试一律不算。
// 顶层布尔（后端计算，完整与摘要 JSON 都带）权威；缺失时才回落到尝试上的原因码。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const { logOutputIncomplete, attemptShowsOutputIncomplete } = load('src/components/modules/log/output-incomplete.ts');

const MARK = 'upstream_missing_terminal';
const success = (reason) => ({ status: 'success', output_incomplete_reason: reason });
const failed = (reason) => ({ status: 'failed', output_incomplete_reason: reason });

test('top-level boolean is authoritative for rows without attempts (summary JSON)', () => {
    assert.equal(logOutputIncomplete({ output_incomplete: true }), true);
    assert.equal(logOutputIncomplete({ output_incomplete: false }), false);
});

test('old records with neither field show no marker', () => {
    assert.equal(logOutputIncomplete({}), false);
    assert.equal(logOutputIncomplete({ attempts: [] }), false);
    assert.equal(logOutputIncomplete({ attempts: [success(undefined), failed(undefined)] }), false);
});

test('fallback: the selected successful attempt carries the reason when the boolean is absent', () => {
    assert.equal(logOutputIncomplete({ attempts: [failed(undefined), success(MARK)] }), true);
    assert.equal(attemptShowsOutputIncomplete([failed(undefined), success(MARK)], 1), true);
});

test('losing and failed attempts never mark the row', () => {
    // 竞速落选：后一次成功且没标记 -> 整行不算不完整，落选那次也不标注
    const race = [success(MARK), success(undefined)];
    assert.equal(logOutputIncomplete({ attempts: race }), false);
    assert.equal(attemptShowsOutputIncomplete(race, 0), false, 'race loser is not the final attempt');
    // 只有失败尝试带原因 -> 失败不是这条规则的适用对象
    assert.equal(logOutputIncomplete({ attempts: [success(undefined), failed(MARK)] }), false);
    assert.equal(logOutputIncomplete({ attempts: [failed(MARK)] }), false);
});

test('only the recognized reason code counts', () => {
    assert.equal(logOutputIncomplete({ attempts: [success('')] }), false);
    assert.equal(logOutputIncomplete({ attempts: [success('   ')] }), false);
    assert.equal(logOutputIncomplete({ attempts: [success('some_future_reason')] }), false);
    assert.equal(logOutputIncomplete({ attempts: [success(` ${MARK} `)] }), true);
});

test('an explicit boolean wins over the attempt fallback', () => {
    assert.equal(logOutputIncomplete({ output_incomplete: false, attempts: [success(MARK)] }), false);
    assert.equal(logOutputIncomplete({ output_incomplete: true, attempts: [success(undefined)] }), true);
});

test('locales carry the badge and the neutral explanation in all three languages', () => {
    const card = (name) => JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${name}.json`), 'utf8')).log.card;
    const zhHans = card('zh_hans');
    const zhHant = card('zh_hant');
    const en = card('en');
    assert.equal(zhHans.outputIncomplete, '输出不完整');
    assert.equal(zhHant.outputIncomplete, '輸出不完整');
    assert.equal(en.outputIncomplete, 'Output incomplete');
    assert.match(zhHans.outputIncompleteExplain, /回复可能不完整/);
    for (const entry of [zhHans, zhHant, en]) {
        assert.ok(entry.outputIncomplete, 'missing badge label -> MISSING_MESSAGE');
        assert.ok(entry.outputIncompleteExplain, 'missing explanation -> MISSING_MESSAGE');
    }
});
