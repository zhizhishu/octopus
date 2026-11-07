const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const {
    describeThroughput,
    assessThroughput,
    throughputReasonLabelKey,
    throughputEndpointEligible,
    throughputOutputIsEstimated,
} = load('src/components/modules/log/throughput.ts');

// 2026-10-27 起的新口径（astra 裁决，库内实测驱动）：
// 生成窗口不可采信 → 不显示（null），绝不退回全程平均冒充生成速度。
// 阈值与 internal/op/throughput.go 一一对应，改一边必须同步另一边。
// 闸门只声明「这组数字不足以支撑展示」，不断言原因（缓冲/排队/重试/思考都只是可能）。

test('正常流式窗口：只算首字之后的生成窗口', () => {
    // 1000 个输出 token，总耗时 10s，其中首字占 2s → 生成 8s → 125 t/s
    const r = describeThroughput({ output_tokens: 1000, use_time: 10000, ftut: 2000 });
    assert.deepEqual(r, { text: '125' });
});

test('爆表原型：ftut 落在 use_time-1ms（窗口只剩 1ms）→ 不显示', () => {
    // 实测数据：5874 token / 24133ms / ftut 24132ms，1ms 窗口会算出 5,874,000 tok/s。
    assert.equal(describeThroughput({ output_tokens: 5874, use_time: 24133, ftut: 24132 }), null);
});

test('生成窗口占比不足 25% → 不显示', () => {
    // 窗口 1s 达标但只占总耗时 10%：首字落在 90% 处，窗口不可采信。
    assert.equal(describeThroughput({ output_tokens: 500, use_time: 10000, ftut: 9000 }), null);
});

test('输出不足 100 token → 不显示（样本太短，窗口不足以代表生成速度）', () => {
    assert.equal(describeThroughput({ output_tokens: 73, use_time: 46271, ftut: 3995 }), null);
    assert.equal(describeThroughput({ output_tokens: 99, use_time: 5000, ftut: 1000 }), null);
});

test('首字缺失 / 脏数据 → 不显示，绝不退回全程平均', () => {
    // 旧行为是退回 output/use_time 冒充生成速度——那是更隐蔽的骗法，已废除。
    assert.equal(describeThroughput({ output_tokens: 500, use_time: 5000, ftut: 0 }), null);
    assert.equal(describeThroughput({ output_tokens: 500, use_time: 5000 }), null);
    assert.equal(describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 4000 }), null);
    assert.equal(describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 9000 }), null);
});

test('超过当前展示上限 → 不显示（当前观测不足以采信，不推断成因）', () => {
    assert.equal(describeThroughput({ output_tokens: 5000, use_time: 3000, ftut: 1000 }), null);
});

test('有错误的日志不显示生成速度，与后端 relayLogTPSSample 一致', () => {
    const ok = { output_tokens: 1000, use_time: 10000, ftut: 2000 };
    assert.deepEqual(describeThroughput(ok), { text: '125' });
    assert.equal(describeThroughput({ ...ok, error: 'upstream failed' }), null);
    assert.equal(describeThroughput({ ...ok, error: '  boom  ' }), null);
    // 空白错误等于没错误，和 strings.TrimSpace 一样。
    assert.deepEqual(describeThroughput({ ...ok, error: '   ' }), { text: '125' });
    assert.deepEqual(describeThroughput({ ...ok, error: '' }), { text: '125' });
});

test('算不出来就返回 null，让界面整块不渲染——不拿 0 占位', () => {
    assert.equal(describeThroughput({ output_tokens: 0, use_time: 5000 }), null);
    assert.equal(describeThroughput({ output_tokens: 100, use_time: 0 }), null);
    assert.equal(describeThroughput({}), null);
    assert.equal(describeThroughput({ output_tokens: -5, use_time: 5000 }), null);
});

test('小数值不许被舍成 0.0，极小的给下限写法（在有效窗口内）', () => {
    // 100 token / 11s ≈ 9.09 → 小于 10 给两位小数
    assert.equal(describeThroughput({ output_tokens: 100, use_time: 12000, ftut: 1000 }).text, '9.09');
    // 100 token / 12000s ≈ 0.008 → 低于可显示精度（窗口 12000s 占总耗时 25%，有效）
    assert.equal(
        describeThroughput({ output_tokens: 100, use_time: 48000000, ftut: 36000000 }).text,
        '<0.01',
    );
});

// --- 详细判定：算不出来时到底为什么（界面用它翻人话，不外露机器码） -------
// 一份真值表：每个拒绝条件一个原因码，且拒绝顺序固定。界面显示「TPS —」+ 这条
// 原因；「显示不出来」不等于「这一行没有这个指标」。

const GOOD = { output_tokens: 1000, use_time: 10000, ftut: 2000 };

function reason(log) {
    const assessed = assessThroughput(log);
    assert.equal(assessed.available, false, `expected unavailable for ${JSON.stringify(log)}`);
    return assessed.reason;
}

test('可采信窗口：给数字与数值，不让调用方自己再算一遍', () => {
    assert.deepEqual(assessThroughput(GOOD), { available: true, text: '125', tps: 125 });
});

test('原因码真值表：一个拒绝条件一个原因，顺序固定', () => {
    assert.equal(reason({ ...GOOD, error: 'upstream 500' }), 'request_failed');
    assert.equal(reason({ output_tokens: 0, use_time: 10000, ftut: 2000 }), 'output_missing');
    assert.equal(reason({ use_time: 10000, ftut: 2000 }), 'output_missing');
    assert.equal(reason({ ...GOOD, output_tokens: 99 }), 'output_too_short');
    assert.equal(reason({ ...GOOD, use_time: 999 }), 'duration_invalid');
    assert.equal(reason({ output_tokens: 1000, ftut: 200 }), 'duration_invalid');
    assert.equal(reason({ output_tokens: 1000, use_time: 10000 }), 'first_token_missing');
    assert.equal(reason({ ...GOOD, ftut: 10000 }), 'first_token_missing');
    assert.equal(reason({ ...GOOD, ftut: 12000 }), 'first_token_missing');
    assert.equal(reason({ output_tokens: 5874, use_time: 24133, ftut: 24132 }), 'generation_window_short');
    assert.equal(reason({ output_tokens: 500, use_time: 10000, ftut: 9000 }), 'generation_share_low');
    assert.equal(reason({ output_tokens: 5000, use_time: 3000, ftut: 1000 }), 'implausible_speed');
});

// D1 回归：管理员载荷带 error，普通用户摘要载荷只有 error_code/error_status（没有 error 字段）。
// 同一条失败行必须得到同一个结论——不能管理员看到「TPS —」、用户看到编出来的速度。
// 顺带：重试后成功（warn）照常可算，闸门只拒 error。
test('同一条失败行：管理员视图与用户摘要视图给出同一原因', () => {
    const timings = { output_tokens: 1000, use_time: 10000, ftut: 2000 };
    const adminRow = { ...timings, error: 'upstream returned 500', error_code: 'server_error', error_status: 500 };
    const summaryRow = { ...timings, error_code: 'server_error', error_status: 500 };
    assert.equal(reason(adminRow), 'request_failed');
    assert.equal(reason(summaryRow), 'request_failed');
    assert.deepEqual(assessThroughput(adminRow), assessThroughput(summaryRow));

    // 只有状态码、只有原因码，各自就够判失败。
    assert.equal(reason({ ...timings, error_status: 429 }), 'request_failed');
    assert.equal(reason({ ...timings, error_code: 'rate_limited' }), 'request_failed');

    // warn（重试/故障转移后成功）不是失败：不拒算。
    assert.deepEqual(assessThroughput({ ...timings, total_attempts: 3 }), { available: true, text: '125', tps: 125 });
    assert.deepEqual(assessThroughput({ ...timings, error_code: '  ', error_status: 0 }), { available: true, text: '125', tps: 125 });
});

test('每个原因码都有给人看文案键，机器码不外露', () => {
    const codes = [
        'request_failed', 'output_missing', 'output_too_short', 'duration_invalid',
        'first_token_missing', 'generation_window_short', 'generation_share_low', 'implausible_speed',
    ];
    const keys = codes.map((code) => throughputReasonLabelKey(code));
    for (const key of keys) assert.match(key, /^tpsReason[A-Z]/, 'reason code must map to a log.card.tpsReason* label');
    assert.equal(new Set(keys).size, codes.length, 'one label per reason, no shared fallback');
});

test('估算用量要标明分子是估算值（不能把估算说成实测）', () => {
    assert.equal(throughputOutputIsEstimated({ ...GOOD, usage_source: 'local_estimate' }), true);
    assert.equal(throughputOutputIsEstimated({ ...GOOD, usage_source: '  LOCAL_ESTIMATE ' }), true);
    assert.equal(throughputOutputIsEstimated({ ...GOOD, usage_source: 'upstream_usage' }), false);
    assert.equal(throughputOutputIsEstimated(GOOD), false);
});

// 图片/视频/音频/向量/审核/重排按张、按秒、按条计费，token 数不是生成量 → 整块不出。
// 端点名都用后端实际落库的 request_endpoint（raw_protocol.go:92-197、images.go:506、
// relay.go:4843-4861），不用归一化显示名，也不靠模型名/URL 猜。
// 认不出的端点（空值、旧行、systemone 之类）沿用旧行为：照常给徽标 + 原因。
test('非文本生成端点不出 TPS，文本/未知端点照常出', () => {
    for (const endpoint of ['images', 'images_generations', 'images_edits', 'images_async', 'images_midjourney', 'edits', 'videos', 'videos_poll', 'audio_speech', 'audio_tts', 'audio_transcriptions', 'audio_realtime', 'embeddings', 'moderations', 'rerank']) {
        assert.equal(throughputEndpointEligible(endpoint), false, `${endpoint} is not a text generation endpoint`);
    }
    for (const endpoint of ['chat', 'responses', 'messages', 'gemini_generate_content', 'gemini_stream_generate_content', 'completions', 'responses_compact', 'systemone', 'avideos']) {
        assert.equal(throughputEndpointEligible(endpoint), true, `${endpoint} keeps the badge`);
    }
    assert.equal(throughputEndpointEligible(''), true, 'legacy row without an endpoint keeps the old eligibility');
    assert.equal(throughputEndpointEligible(undefined), true, 'legacy row without an endpoint keeps the old eligibility');
    assert.equal(throughputEndpointEligible('  Images  '), false, 'matching is case/space tolerant');
});
