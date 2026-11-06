// Run from web/: node --test tests/audit-highlights.test.cjs
// 检测结果页第二期：结论下方的要点行 + 「渠道来源」卡。
//   - 百分比只透传后端给的 percent，前端绝不自己算
//   - 未知 tone 当 info；没有 highlights 就整块不渲染（空数组）
//   - provenance 缺失 / observed 不是正数 → 不出卡；strong 排在 medium 前；只有 weak 不算可疑
//   - 新探针 system_prompt 在探针总表和标准方案里，但不在快速方案里
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const hl = load('src/lib/audit-highlights.ts');

const auditApi = load('src/api/endpoints/model-audit.ts', {
    '@tanstack/react-query': { useQuery: (o) => o, useMutation: (o) => o, useQueryClient: () => ({ invalidateQueries() {} }) },
    '../client': { apiClient: { get: async () => ({}), post: async () => ({}) } },
    '@/lib/logger': { logger: { error() {} } },
});

const INDEX = path.join(__dirname, '..', 'src', 'components', 'modules', 'model-audit', 'index.tsx');
const CARD = path.join(__dirname, '..', 'src', 'components', 'modules', 'model-audit', 'ProvenanceCard.tsx');

test('highlightRows：没有 highlights → 空数组（界面据此不渲染）', () => {
    assert.deepEqual(hl.highlightRows(null), []);
    assert.deepEqual(hl.highlightRows(undefined), []);
    assert.deepEqual(hl.highlightRows({}), []);
    assert.deepEqual(hl.highlightRows({ highlights: null }), []);
    assert.deepEqual(hl.highlightRows({ highlights: [] }), []);
});

test('highlightRows：percent 只透传后端给的值，绝不自造', () => {
    const rows = hl.highlightRows({
        highlights: [
            { kind: 'agreement', text: '8 项里 7 项和正品一致', tone: 'ok', percent: 87.5 },
            { kind: 'no_percent', text: '这条后端没给百分比', tone: 'warn' },
            { kind: 'zero', text: '0% 也要照实显示', tone: 'info', percent: 0 },
        ],
    });
    assert.equal(rows.length, 3);
    assert.equal(rows[0].percentText, '87.5%');
    // 没有 percent 就是 undefined：前端不许自己算一个数字出来。
    assert.equal(rows[1].percentText, undefined);
    assert.equal(rows[2].percentText, '0%');
    // 非数字 / NaN 一律当没给。
    assert.equal(hl.highlightRows({ highlights: [{ kind: 'x', text: 't', tone: 'ok', percent: '80' }] })[0].percentText, undefined);
    assert.equal(hl.highlightRows({ highlights: [{ kind: 'x', text: 't', tone: 'ok', percent: NaN }] })[0].percentText, undefined);
});

test('highlightRows：未知 tone 当 info，空文字的行丢掉', () => {
    const rows = hl.highlightRows({
        highlights: [
            { kind: 'a', text: '认不出的色', tone: 'weird' },
            { kind: 'b', text: '没写色' },
            { kind: 'c', text: '   ' },
            { kind: 'd', text: '正常', tone: 'warn' },
        ],
    });
    assert.deepEqual(rows.map((row) => row.tone), ['info', 'info', 'warn']);
    assert.deepEqual(rows.map((row) => row.text), ['认不出的色', '没写色', '正常']);
    // key 稳定且不重复（同 kind 也能区分）。
    assert.equal(new Set(rows.map((row) => row.key)).size, rows.length);
});

test('TONE_DOT_CLASS：三档齐全，warn 琥珀、ok 绿、info 灰', () => {
    assert.match(hl.TONE_DOT_CLASS.warn, /amber-/);
    assert.match(hl.TONE_DOT_CLASS.ok, /emerald-/);
    assert.match(hl.TONE_DOT_CLASS.info, /muted-foreground/);
    assert.deepEqual(Object.keys(hl.TONE_DOT_CLASS).sort(), ['info', 'ok', 'warn']);
});

test('provenanceView：provenance 缺失或 observed 不是正数 → 不出卡', () => {
    assert.equal(hl.provenanceView(null), null);
    assert.equal(hl.provenanceView(undefined), null);
    assert.equal(hl.provenanceView({}), null);
    assert.equal(hl.provenanceView({ provenance: null }), null);
    assert.equal(hl.provenanceView({ provenance: {} }), null);
    assert.equal(hl.provenanceView({ provenance: { observed: 0 } }), null);
    assert.equal(hl.provenanceView({ provenance: { observed: -3 } }), null);
    assert.equal(hl.provenanceView({ provenance: { observed: NaN } }), null);
    // 有线索但一份回复都没看过：仍然不出卡（没样本就没有结论）。
    assert.equal(hl.provenanceView({ provenance: { observed: 0, hints: [{ platform: 'aws', strength: 'strong', code: 'header_aws' }] } }), null);
});

test('provenanceView：strong 排在 medium 前，weak 垫底', () => {
    const view = hl.provenanceView({
        provenance: {
            observed: 12,
            header_families: ['aws', 'cloudflare', ' ', ''],
            hints: [
                { platform: 'openai', strength: 'weak', code: 'header_openai_cross_vendor' },
                { platform: 'google', strength: 'medium', code: 'signature_vertex' },
                { platform: 'aws', strength: 'strong', code: 'header_aws', evidence: 'x-amzn-requestid' },
            ],
        },
    });
    assert.ok(view);
    assert.deepEqual(view.hints.map((hint) => hint.strength), ['strong', 'medium', 'weak']);
    assert.deepEqual(view.hints.map((hint) => hint.code), ['header_aws', 'signature_vertex', 'header_openai_cross_vendor']);
    assert.equal(view.hints[0].evidence, 'x-amzn-requestid');
    // 空白类别剔掉，但不改动顺序。
    assert.deepEqual(view.headerFamilies, ['aws', 'cloudflare']);
    assert.equal(view.observed, 12);
    assert.equal(view.suspicious, true);
});

test('provenanceView：只有 weak 线索 → suspicious=false（weak 不够格）', () => {
    const view = hl.provenanceView({
        provenance: { observed: 3, hints: [{ platform: 'openai', strength: 'weak', code: 'header_openai_cross_vendor' }] },
    });
    assert.ok(view);
    assert.equal(view.suspicious, false);
    // weak 仍然要列出来备查（只是不触发那句「疑似」）。
    assert.equal(view.hints.length, 1);
});

test('provenanceView：没有线索但有样本 → 不可疑，头类别照旧给', () => {
    const view = hl.provenanceView({ provenance: { observed: 5, header_families: ['openai'] } });
    assert.deepEqual(view, { suspicious: false, hints: [], headerFamilies: ['openai'], observed: 5 });
    // 强度认不出来时当 weak：不认识的东西不许触发「疑似」。
    const weird = hl.provenanceView({ provenance: { observed: 2, hints: [{ platform: 'x', strength: '???', code: 'header_aws' }] } });
    assert.equal(weird.hints[0].strength, 'weak');
    assert.equal(weird.suspicious, false);
});

test('新探针 system_prompt：在探针总表和标准方案里，不在手动项里', () => {
    assert.ok(auditApi.AUDIT_PROBE_IDS.includes('system_prompt'));
    assert.ok(auditApi.AUDIT_STANDARD_PROBE_IDS.includes('system_prompt'));
    assert.ok(!auditApi.AUDIT_OPT_IN_PROBE_IDS.includes('system_prompt'));
    const source = readFileSync(INDEX, 'utf8');
    assert.match(source, /'system_prompt'/);
    // 快速方案（QUICK_IDS）不加它：快速方案只答「通不通、有没有被改、有没有被截断」。
    const quick = source.match(/const QUICK_IDS[^\n]*\n/);
    assert.ok(quick, '没找到 QUICK_IDS');
    assert.doesNotMatch(quick[0], /system_prompt/);
});

test('界面接上了两块新内容：要点行与渠道来源卡', () => {
    const source = readFileSync(INDEX, 'utf8');
    assert.match(source, /highlightRows\(report\?\.report\)/);
    assert.match(source, /provenanceView\(report\?\.report\)/);
    assert.match(source, /TONE_DOT_CLASS\[row\.tone\]/);
    assert.match(source, /row\.percentText/);
    assert.match(source, /ProvenanceCard/);
    const card = readFileSync(CARD, 'utf8');
    // 平台/原因文案复用第一期的映射，不另写一套。
    assert.match(card, /PASSIVE_REASON_LABEL_KEY/);
    assert.match(card, /useTranslations\('log\.card'\)/);
    // 边界那句话必须原样在卡里。
    assert.match(card, /只能发现线索，不能证明是官方正品/);
});
