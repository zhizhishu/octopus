const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const v = load('src/lib/model-audit-verdict.ts');

test('verdict headlines match Design 1 mapping', () => {
    assert.equal(v.verdictHeadline('none'), '没发现问题');
    assert.equal(v.verdictHeadline('low'), '有小疑点');
    assert.equal(v.verdictHeadline('medium'), '有可疑点，建议复核');
    assert.equal(v.verdictHeadline('high'), '有可疑点，建议复核');
    assert.equal(v.verdictHeadline('unknown'), '数据不够，没法判断');
    assert.equal(v.verdictHeadline('none', { headline: '自定义' }), '自定义');
});

test('review count ignores score-0 reference findings (tokenizer never 待复核)', () => {
    assert.equal(v.countReviewFindings([{ score: 0 }, { score: 0 }, { score: 25 }]), 1);
    assert.equal(v.countReviewFindings([{ score: 0, title: '分词器不符' }]), 0);
    assert.equal(v.countReviewFindings([]), 0);
});

test('chip mapping matches backend ChipForResult', () => {
    assert.equal(v.chipForResult('done', 'none'), 'ok');
    assert.equal(v.chipForResult('done', 'low'), 'ok');
    assert.equal(v.chipForResult('done', 'medium'), 'warn');
    assert.equal(v.chipForResult('done', 'high'), 'warn');
    assert.equal(v.chipForResult('done', 'unknown'), 'fail');
    assert.equal(v.chipForResult('failed', ''), 'fail');
    assert.equal(v.chipForResult('pending', ''), 'none');
    assert.equal(v.chipLabel('warn'), '待复核');
    assert.equal(v.chipLabel('none'), '没检测过');
});

test('formatReportTime uses the report timestamp, not wall clock alone', () => {
    const iso = '2026-10-30T10:21:00+08:00';
    const text = v.formatReportTime(iso);
    assert.ok(text.includes('10:21') || text.includes('02:21') || /\d{1,2}:\d{2}/.test(text), text);
});

test('historyBucket separates suspect and failed', () => {
    assert.equal(v.historyBucket('done', 'high'), 'suspect');
    assert.equal(v.historyBucket('failed', ''), 'failed');
    assert.equal(v.historyBucket('done', 'unknown'), 'failed');
    assert.equal(v.historyBucket('done', 'none'), 'all');
});
