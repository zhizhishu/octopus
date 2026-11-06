// Run from web/: node --test tests/passive-verdict.test.cjs
// 手机日志页的「被动」徽章从「证据不足/未评估/未分析」改成一句结论：
//   - 判定顺序（error / 三处都没证据 / 明确不适用 / 可疑 / 干净）
//   - strong 排在 medium 前、weak 不触发可疑、探针命中单独计数
//   - 线索码 → 翻译 key，未知码落到 passiveReasonUnknown
//   - 三种语言都有全部文案；Item.tsx 的 memo 比较器带上了被动三件套
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const pv = load('src/lib/passive-verdict.ts');

const ITEM = path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'Item.tsx');

/** 一条已经分析过、什么都没发现的记录（各用例在它基础上加料）。 */
function cleanRow(extra = {}) {
    return {
        passive_summary: { version: 'v2', status: 'insufficient_evidence', reason: 'no_baseline' },
        ...extra,
    };
}

test('顺序 1：error 非空 → 不出徽章（失败请求判断不了）', () => {
    assert.equal(pv.passiveVerdict({ ...cleanRow(), error: 'upstream timeout' }), null);
    // 就算同时带着 strong 线索也不出——失败的那次回复不是证据。
    assert.equal(pv.passiveVerdict({
        error: 'connection reset',
        passive_summary: { status: 'insufficient_evidence', reason: 'ok' },
        passive_source: { hints: [{ platform: 'aws', strength: 'strong', code: 'header_aws' }] },
        passive_probes: [{ code: 'token_arithmetic' }],
    }), null);
    // 空白 error 不算「非空」。
    assert.deepEqual(pv.passiveVerdict({ ...cleanRow(), error: '   ' }), { kind: 'clean' });
    assert.deepEqual(pv.passiveVerdict({ ...cleanRow(), error: '' }), { kind: 'clean' });
});

test('顺序 2：passive_summary / passive_source / passive_probes 都没有 → 不出徽章', () => {
    assert.equal(pv.passiveVerdict({}), null);
    assert.equal(pv.passiveVerdict({ error: '', passive_summary: null, passive_source: null }), null);
    // 空探针数组等于没有证据（旧记录反序列化出来就是 []）。
    assert.equal(pv.passiveVerdict({ passive_probes: [] }), null);
    // passive_source 只给了个空对象（没有 hints / header_families）算「有字段但没线索」：
    // 按规格第 2 步的字面意思（字段在就不算「都没有」）落地为 clean。
    assert.deepEqual(pv.passiveVerdict({ passive_probes: [], passive_source: {} }), { kind: 'clean' });
});

test('顺序 3：not_applicable 且 error/canceled/out_of_range → 不出徽章', () => {
    for (const reason of ['error', 'canceled', 'out_of_range']) {
        const row = { passive_summary: { version: 'v2', status: 'not_applicable', reason } };
        assert.equal(pv.passiveVerdict(row), null, reason);
        // 其它被动证据也救不回来：这条请求本身就不在适用范围内。
        assert.equal(pv.passiveVerdict({ ...row, passive_probes: [{ code: 'x' }] }), null, reason);
    }
    // 其它 not_applicable 原因（如 blank / non_text）不是「判断不了」，照常给结论。
    for (const reason of ['blank', 'non_text', 'multiple_choices', 'unknown_finish', 'tool_output',
        'unparseable', 'oversized_envelope', 'oversized_text', 'opaque_response']) {
        assert.deepEqual(
            pv.passiveVerdict({ passive_summary: { version: 'v2', status: 'not_applicable', reason } }),
            { kind: 'clean' },
            reason,
        );
    }
});

test('顺序 4：strong/medium 线索或探针命中 → suspicious', () => {
    const strong = pv.passiveVerdict(cleanRow({
        passive_source: { hints: [{ platform: 'aws', strength: 'strong', code: 'model_prefix_kiro' }] },
    }));
    assert.deepEqual(strong, { kind: 'suspicious', reasons: ['model_prefix_kiro'], probeCount: 0 });

    const medium = pv.passiveVerdict(cleanRow({
        passive_source: { hints: [{ platform: 'google', strength: 'medium', code: 'signature_vertex' }] },
    }));
    assert.deepEqual(medium, { kind: 'suspicious', reasons: ['signature_vertex'], probeCount: 0 });

    // 只有探针命中：reasons 为空，由 probeCount 说话。
    const probeOnly = pv.passiveVerdict(cleanRow({
        passive_probes: [{ code: 'token_arithmetic' }, { code: 'cache_exceeds_prompt' }],
    }));
    assert.deepEqual(probeOnly, { kind: 'suspicious', reasons: [], probeCount: 2 });
});

test('reasons 里 strong 排在 medium 之前，且去重', () => {
    const verdict = pv.passiveVerdict(cleanRow({
        passive_source: {
            hints: [
                { platform: 'google', strength: 'medium', code: 'tool_id_vertex' },
                { platform: 'aws', strength: 'strong', code: 'model_prefix_kiro' },
                { platform: 'aws', strength: 'medium', code: 'tool_id_vertex' },
                { platform: 'aws', strength: 'strong', code: 'model_prefix_kiro' },
            ],
        },
    }));
    assert.equal(verdict.kind, 'suspicious');
    assert.deepEqual(verdict.reasons, ['model_prefix_kiro', 'tool_id_vertex']);
});

test('weak 线索不触发可疑（只在弹层里列出）', () => {
    const verdict = pv.passiveVerdict(cleanRow({
        passive_source: {
            hints: [{ platform: 'openai', strength: 'weak', code: 'header_openai_cross_vendor' }],
        },
    }));
    assert.deepEqual(verdict, { kind: 'clean' });
    // 但 weak 线索在弹层里要列出来（排序结果保留它）。
    assert.deepEqual(
        pv.sortPassiveHints([{ platform: 'openai', strength: 'weak', code: 'header_openai_cross_vendor' }])
            .map((h) => h.code),
        ['header_openai_cross_vendor'],
    );
});

test('顺序 5：有摘要、没线索、没探针 → clean', () => {
    assert.deepEqual(pv.passiveVerdict(cleanRow()), { kind: 'clean' });
    assert.deepEqual(pv.passiveVerdict({ passive_summary: { status: 'insufficient_evidence', reason: 'x' } }), { kind: 'clean' });
    // passive_source 只有响应头类别（不是线索）也是 clean。
    assert.deepEqual(pv.passiveVerdict(cleanRow({ passive_source: { header_families: ['openai'] } })), { kind: 'clean' });
});

test('线索码 → 翻译 key，未知码落到 passiveReasonUnknown', () => {
    const expected = {
        model_prefix_kiro: 'passiveReasonKiro',
        model_prefix_bedrock: 'passiveReasonBedrock',
        tool_id_bedrock: 'passiveReasonAws',
        header_aws: 'passiveReasonAws',
        response_id_vertex: 'passiveReasonVertex',
        tool_id_vertex: 'passiveReasonVertex',
        signature_vertex: 'passiveReasonVertex',
        header_openai_cross_vendor: 'passiveReasonOpenAICross',
        header_anthropic_cross_vendor: 'passiveReasonAnthropicCross',
        model_switch_mid_stream: 'passiveReasonModelSwitch',
    };
    for (const [code, key] of Object.entries(expected)) {
        assert.equal(pv.PASSIVE_REASON_LABEL_KEY[code], key, code);
        assert.equal(pv.passiveReasonLabelKey(code), key, code);
    }
    assert.equal(pv.passiveReasonLabelKey('a_code_from_the_future'), 'passiveReasonUnknown');
    assert.equal(pv.passiveReasonLabelKey(''), 'passiveReasonUnknown');
    assert.equal(pv.passiveReasonLabelKey(undefined), 'passiveReasonUnknown');
    // 探针单独命中时徽章上那句话的 key 也在。
    assert.equal(pv.PASSIVE_REASON_PROBE_KEY, 'passiveReasonProbe');
});

test('三种语言都有全部被动结论文案', () => {
    const keys = ['passiveVerdictClean', 'passiveVerdictSuspicious', 'passiveVerdictCaveat',
        'passiveHeaderFamilies', 'passiveReasonKiro', 'passiveReasonBedrock', 'passiveReasonAws',
        'passiveReasonVertex', 'passiveReasonOpenAICross', 'passiveReasonAnthropicCross',
        'passiveReasonModelSwitch', 'passiveReasonProbe', 'passiveReasonUnknown'];
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const json = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8'));
        for (const k of keys) assert.ok(json.log.card[k], `${lang} log.card.${k}`);
        assert.match(json.log.card.passiveVerdictSuspicious, /\{reason\}/, lang);
    }
});

test('memo 比较器带上了被动三件套（漏一个就会出现「点了没反应」）', () => {
    const source = readFileSync(ITEM, 'utf8');
    const start = source.indexOf('export function logCardPropsEqual');
    assert.ok(start > 0, '没找到 logCardPropsEqual');
    const end = source.indexOf('\n}', start);
    assert.ok(end > start, '没找到比较器结尾');
    const comparator = source.slice(start, end);
    assert.match(comparator, /prev\.log\.passive_summary === next\.log\.passive_summary/);
    assert.match(comparator, /prev\.log\.passive_source === next\.log\.passive_source/);
    assert.match(comparator, /prev\.log\.passive_probes === next\.log\.passive_probes/);
});

test('卡片只留一个被动徽标：旧的三个独立徽标槽位已去掉', () => {
    const source = readFileSync(ITEM, 'utf8');
    // 徽章合并成一个结论：独立的来源证据/探针槽位不再渲染。
    assert.doesNotMatch(source, /probeSlot/);
    assert.doesNotMatch(source, /sourceSlot/);
    // 旧的三态徽章文案不再被引用（locale 里的旧 key 保留不删）。
    assert.doesNotMatch(source, /passiveBadgeForLog/);
    assert.match(source, /passiveVerdict\(log\)/);
});
