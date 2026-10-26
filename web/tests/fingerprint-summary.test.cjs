const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { readFingerprint, describeDecision, formatProbability } = load(
    'src/components/modules/model-audit/fingerprint.ts',
);

// 这些用例盯的是同一件事：后端算出来的指纹结论必须能在检测页上"看得见"。
// 以前它只体现为一行「随机数指纹 · 未见异常」，具体像哪个模型被埋在原始 JSON 里。

function reportWith(data, extra = {}) {
    return {
        results: [
            { probe_id: 'liveness', ok: true, data: {} },
            { probe_id: 'fingerprint', ok: true, data, ...extra },
        ],
    };
}

test('三条都有效：给出最像的模型、家族、候选与概率', () => {
    const summary = readFingerprint(
        reportWith({
            fingerprint_decision: 'scorable',
            fingerprint_prediction: 'claude-sonnet-4.6',
            fingerprint_prediction_name: 'Claude Sonnet 4.6',
            fingerprint_family_name: 'Claude',
            fingerprint_probability: 0.834,
            fingerprint_used_outputs: 3,
            fingerprint_reference_built_at: '2026-10-13T16:21:11Z',
            fingerprint_probability_scope: 'reference-closed-set',
            fingerprint_candidates: [
                { model: 'claude-sonnet-4.6', display_name: 'Claude Sonnet 4.6', family_name: 'Claude', score: 0.71, probability: 0.834 },
                { model: 'claude-opus-4.8', display_name: 'Claude Opus 4.8', family_name: 'Claude', score: 0.68, probability: 0.12 },
            ],
        }),
    );

    assert.equal(summary.decision, 'scorable');
    assert.equal(summary.predictionName, 'Claude Sonnet 4.6');
    assert.equal(summary.familyName, 'Claude');
    assert.equal(summary.usedOutputs, 3);
    assert.equal(summary.insufficient, false);
    assert.equal(summary.candidates.length, 2);
    assert.equal(summary.candidates[0].displayName, 'Claude Sonnet 4.6');
    assert.equal(summary.candidates[1].probability, 0.12);
    assert.equal(formatProbability(summary.probability), '83%');
});

test('有效回答不足三条：只给排名，不给置信度', () => {
    const summary = readFingerprint(
        reportWith({
            fingerprint_decision: 'partial',
            fingerprint_prediction_name: 'GLM-4.6',
            fingerprint_used_outputs: 2,
            fingerprint_candidates: [{ model: 'glm-4.6', display_name: 'GLM-4.6', family_name: 'GLM', score: 0.5 }],
        }),
    );

    assert.equal(summary.decision, 'partial');
    assert.equal(summary.probability, undefined);
    assert.equal(formatProbability(summary.probability), '');
    assert.match(describeDecision(summary), /不给置信度/);
});

test('证据不足：带出后端原因，并且不能被当成"通过"', () => {
    const summary = readFingerprint(
        reportWith({
            fingerprint_decision: 'unscorable',
            insufficient_evidence: true,
            reason: '有效整数少于下限',
        }),
    );

    assert.equal(summary.insufficient, true);
    assert.equal(summary.reason, '有效整数少于下限');
    assert.match(describeDecision(summary), /证据不足.*有效整数少于下限/);
});

test('分析失败：把失败原因当内容显示，而不是整块消失', () => {
    const summary = readFingerprint(
        reportWith({ fingerprint_decision: '', fingerprint_error: '加载挑战题库失败: EOF' }),
    );

    assert.ok(summary, '失败也要有内容，不能返回 null');
    assert.equal(summary.errorText, '加载挑战题库失败: EOF');
    assert.match(describeDecision(summary), /这次没测成/);
});

test('没跑这项探针：返回 null，界面不摆空壳', () => {
    assert.equal(readFingerprint({ results: [{ probe_id: 'liveness', ok: true, data: {} }] }), null);
    assert.equal(readFingerprint(null), null);
    assert.equal(readFingerprint({}), null);
});

test('脏数据不炸：坏候选被跳过，字符串数字仍能读', () => {
    const summary = readFingerprint(
        reportWith({
            fingerprint_decision: 'scorable',
            fingerprint_used_outputs: '3',
            fingerprint_candidates: [
                { model: '', display_name: '坏项', score: 0.9 },
                { model: 'no-score' },
                { model: 'qwen3-max', display_name: 'Qwen3 Max', score: '0.42' },
            ],
        }),
    );

    assert.equal(summary.usedOutputs, 3);
    assert.equal(summary.candidates.length, 1);
    assert.equal(summary.candidates[0].model, 'qwen3-max');
    assert.equal(summary.candidates[0].score, 0.42);
});

test('概率文案：极小值不显示成 0%', () => {
    assert.equal(formatProbability(0.0004), '<0.1%');
    assert.equal(formatProbability(0.045), '4.5%');
    assert.equal(formatProbability(1), '100%');
});
