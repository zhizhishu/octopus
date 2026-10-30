const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { readTokenizer, readTokenizerRecording, describeTokenizer, formatShare } = load(
    'src/components/modules/model-audit/tokenizer.ts',
);
const posted = [];
const { AUDIT_PROBE_IDS, AUDIT_STANDARD_PROBE_IDS, AUDIT_OPT_IN_PROBE_IDS, useSaveTokenizerBaseline } = load('src/api/endpoints/model-audit.ts', {
    '@tanstack/react-query': { useQuery: (o) => o, useMutation: (o) => o, useQueryClient: () => ({ invalidateQueries() {} }) },
    '../client': { apiClient: { post: async (url, body) => { posted.push([url, body]); return {}; } } },
    '@/lib/logger': { logger: { error() {} } },
});

function reportWith(id, data, extra = {}) {
    return { results: [{ probe_id: 'liveness', ok: true, data: {} }, { probe_id: id, ok: true, data, ...extra }] };
}

test('分词器两项只能手动勾选，不进标准方案', () => {
    assert.ok(AUDIT_PROBE_IDS.includes('tokenizer_id'));
    assert.ok(AUDIT_PROBE_IDS.includes('tokenizer_baseline'));
    assert.deepEqual([...AUDIT_OPT_IN_PROBE_IDS].sort(), ['tokenizer_baseline', 'tokenizer_id']);
    assert.ok(!AUDIT_STANDARD_PROBE_IDS.includes('tokenizer_id'));
    assert.ok(!AUDIT_STANDARD_PROBE_IDS.includes('tokenizer_baseline'));
    assert.equal(AUDIT_STANDARD_PROBE_IDS.length, AUDIT_PROBE_IDS.length - 2);
});

test('不一致：给出测得的分词器与中转估算提示', () => {
    const s = readTokenizer(
        reportWith('tokenizer_id', {
            kind: 'exact', top_class: 'o200k', top_series: 'o200k', top_lab: 'OpenAI', confidence: 0.998,
            claim_status: 'inconsistent', claim_probability: 0.001, claim_vendor: 'Anthropic',
            usable: true, complete: true, baseline_drift: false, relay_estimate_possible: true, requests: 12,
            bank_classes: 62, bank_generated_at: '2026-09-30',
            candidates: [{ id: 'o200k', series: 'o200k', lab: 'OpenAI', exact: 0.998, related: 0.001 }],
        }),
    );
    assert.equal(s.kind, 'exact');
    assert.equal(s.claim, 'inconsistent');
    assert.equal(s.requests, 12);
    assert.equal(s.candidates.length, 1);
    const text = describeTokenizer(s);
    assert.match(text, /o200k（OpenAI）/);
    assert.match(text, /不符/);
    assert.match(text, /中转站本地估算/);
});

test('漂移/中途失败：只作参考，不报不符', () => {
    const s = readTokenizer(reportWith('tokenizer_id', { kind: 'exact', top_series: 'Qwen3.5', claim_status: 'inconsistent', usable: false, baseline_drift: true }));
    const text = describeTokenizer(s);
    assert.match(text, /仅供参考/);
    assert.match(text, /基线/);
    assert.doesNotMatch(text, /不符/);
});

test('探针报错与缺失', () => {
    assert.equal(readTokenizer({ results: [] }), null);
    const s = readTokenizer(reportWith('tokenizer_id', {}, { ok: false, error: 'upstream 502' }));
    assert.equal(describeTokenizer(s), 'upstream 502');
});

test('无映射型号：说明无法核对', () => {
    const s = readTokenizer(reportWith('tokenizer_id', { kind: 'related', top_series: 'Llama 3', usable: true }));
    assert.match(describeTokenizer(s), /无法核对/);
    assert.match(describeTokenizer(s), /变体/);
});

test('录制结果：可录制与拒绝原因', () => {
    const ok = readTokenizerRecording(reportWith('tokenizer_baseline', { recordable: true, counts: { a: 1, b: 2 }, target_model: 'claude-x', bank_generated_at: '2026-09-30' }));
    assert.equal(ok.recordable, true);
    assert.equal(ok.probeCount, 2);
    assert.equal(ok.targetModel, 'claude-x');
    const bad = readTokenizerRecording(reportWith('tokenizer_baseline', { recordable: false, reason: '两次基线计数不同' }));
    assert.equal(bad.recordable, false);
    assert.equal(bad.reason, '两次基线计数不同');
    const failed = readTokenizerRecording(reportWith('tokenizer_baseline', { recordable: true }, { ok: false, error: 'boom' }));
    assert.equal(failed.recordable, false);
});

test('百分比格式', () => {
    assert.equal(formatShare(0.999), '>99%');
    assert.equal(formatShare(0.001), '<1%');
    assert.equal(formatShare(0.42), '42%');
    assert.equal(formatShare(undefined), '');
});

test('保存基线走管理接口，带 result_id', async () => {
    const m = useSaveTokenizerBaseline();
    await m.mutationFn({ result_id: 9, label: '官方直连' });
    assert.deepEqual(posted.at(-1), ['/api/v1/model-audit/tokenizer-baselines', { result_id: 9, label: '官方直连' }]);
});
