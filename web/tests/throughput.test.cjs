const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const {
    describeThroughput,
    assessThroughput,
    assessOverallThroughput,
    throughputReasonLabelKey,
    throughputEndpointEligible,
    throughputOutputIsEstimated,
} = load('src/components/modules/log/throughput.ts');

// 2026-10-27 起的新口径（astra 裁决，库内实测驱动）：
// 生成窗口不可采信 → 不当生成速度展示；后来另给一条明确标注为约值的
// 「全程平均」fallback（kind 'overall'，assessOverallThroughput），带 ≈ 前缀，
// 不冒充生成速度。阈值与 internal/op/throughput.go 一一对应，改一边必须同步另一边。
// 闸门只声明「这组数字不足以支撑展示」，不断言原因（缓冲/排队/重试/思考都只是可能）。

test('正常流式窗口：只算首字之后的生成窗口', () => {
    // 1000 个输出 token，总耗时 10s，其中首字占 2s → 生成 8s → 125 t/s
    const r = describeThroughput({ output_tokens: 1000, use_time: 10000, ftut: 2000 });
    assert.deepEqual(r, { text: '125' });
});

test('爆表原型：ftut 落在 use_time-1ms（窗口只剩 1ms）→ 退回全程平均约值', () => {
    // 实测数据：5874 token / 24133ms / ftut 24132ms，1ms 窗口会算出 5,874,000 tok/s。
    // 精确路径拒算后退全程平均：5874 / 24.133s ≈ 243（kind 'overall'，界面带 ≈）。
    assert.deepEqual(describeThroughput({ output_tokens: 5874, use_time: 24133, ftut: 24132 }), { text: '243' });
});

test('生成窗口占比不足 25% → 退回全程平均约值', () => {
    // 窗口 1s 达标但只占总耗时 10%：首字落在 90% 处，窗口不可采信 → 500/10s = 50。
    assert.deepEqual(describeThroughput({ output_tokens: 500, use_time: 10000, ftut: 9000 }), { text: '50' });
});

test('输出不足 100 token → 退回全程平均约值（样本太短只影响生成速度）', () => {
    assert.deepEqual(describeThroughput({ output_tokens: 73, use_time: 46271, ftut: 3995 }), { text: '1.58' });
    assert.deepEqual(describeThroughput({ output_tokens: 99, use_time: 5000, ftut: 1000 }), { text: '19.80' });
});

test('首字缺失 / 脏数据 → 退回全程平均约值（约值不冒充生成速度）', () => {
    // 约值用总耗时（含首字等待）算：500/5s = 100，300/4s = 75。
    assert.deepEqual(describeThroughput({ output_tokens: 500, use_time: 5000, ftut: 0 }), { text: '100' });
    assert.deepEqual(describeThroughput({ output_tokens: 500, use_time: 5000 }), { text: '100' });
    assert.deepEqual(describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 4000 }), { text: '75' });
    assert.deepEqual(describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 9000 }), { text: '75' });
});

test('生成速度超展示上限 → 退全程平均约值（约值上限更宽，5000）', () => {
    // 精确窗口 2s 算出 2500 超 1000；全程 3s 给 5000/3 ≈ 1667，在 5000 上限内。
    assert.deepEqual(describeThroughput({ output_tokens: 5000, use_time: 3000, ftut: 1000 }), { text: '1667' });
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
    assert.deepEqual(assessThroughput(GOOD), { available: true, text: '125', tps: 125, kind: 'generation' });
});

test('原因码真值表（fallback 仍不可用的情况）：失败 / 无输出不退，退不出保留原原因', () => {
    // request_failed / output_missing 永不退 fallback：失败行与无输出行没有速度概念。
    assert.equal(reason({ ...GOOD, error: 'upstream 500' }), 'request_failed');
    assert.equal(reason({ output_tokens: 0, use_time: 10000, ftut: 2000 }), 'output_missing');
    assert.equal(reason({ use_time: 10000, ftut: 2000 }), 'output_missing');
    // 总耗时缺失（=0）：fallback 也判 duration_invalid → 保留精确路径的原因码。
    assert.equal(reason({ output_tokens: 1000, ftut: 200 }), 'duration_invalid');
    // use_time=5ms：fallback 被 10ms 兜底算出 50000 tok/s，超 5000 上限 → 退不出来。
    assert.equal(reason({ output_tokens: 500, use_time: 5 }), 'duration_invalid');
});

test('精确路径拒算但行本身有效 → 退回全程平均约值，并带上精确原因码', () => {
    // 输出过短：99 token / 10s ≈ 9.9 → '9.90'
    const tooShort = assessThroughput({ ...GOOD, output_tokens: 99 });
    assert.equal(tooShort.available, true);
    assert.equal(tooShort.kind, 'overall');
    assert.equal(tooShort.text, '9.90');
    assert.equal(tooShort.preciseReason, 'output_too_short');

    // use_time=999ms：总耗时够 fallback 的门槛（>0），1000/0.999s ≈ 1001 → '1001'
    const shortTotal = assessThroughput({ ...GOOD, use_time: 999 });
    assert.equal(shortTotal.available, true);
    assert.equal(shortTotal.kind, 'overall');
    assert.equal(shortTotal.text, '1001');
    assert.equal(shortTotal.preciseReason, 'duration_invalid');

    // 首字缺失 / 非法（非流式行最常见）：1000 token / 10s = 100
    for (const log of [{ output_tokens: 1000, use_time: 10000 }, { ...GOOD, ftut: 10000 }, { ...GOOD, ftut: 12000 }]) {
        const a = assessThroughput(log);
        assert.equal(a.available, true);
        assert.equal(a.kind, 'overall');
        assert.equal(a.text, '100');
        assert.equal(a.preciseReason, 'first_token_missing');
    }

    // 窗口只剩 1ms / 占比不足 25%：各自带精确原因码退约值。
    const windowShort = assessThroughput({ output_tokens: 5874, use_time: 24133, ftut: 24132 });
    assert.equal(windowShort.available, true);
    assert.equal(windowShort.kind, 'overall');
    assert.equal(windowShort.text, '243');
    assert.equal(windowShort.preciseReason, 'generation_window_short');

    const shareLow = assessThroughput({ output_tokens: 500, use_time: 10000, ftut: 9000 });
    assert.equal(shareLow.available, true);
    assert.equal(shareLow.kind, 'overall');
    assert.equal(shareLow.text, '50');
    assert.equal(shareLow.preciseReason, 'generation_share_low');

    // 超 1000 展示上限：全程平均 5000/3 ≈ 1667 在 5000 上限内。
    const implausible = assessThroughput({ output_tokens: 5000, use_time: 3000, ftut: 1000 });
    assert.equal(implausible.available, true);
    assert.equal(implausible.kind, 'overall');
    assert.equal(implausible.text, '1667');
    assert.equal(implausible.preciseReason, 'implausible_speed');
});

// --- 全程平均 fallback 本体（assessOverallThroughput 与 assessThroughput 的退路）---

test('非流式行（ftut=0）：500 token / 3.559s ≈ 140.5，全程平均', () => {
    const a = assessThroughput({ output_tokens: 500, use_time: 3559, ftut: 0 });
    assert.equal(a.available, true);
    assert.equal(a.kind, 'overall');
    assert.equal(a.preciseReason, 'first_token_missing');
    assert.equal(a.text, '140');
    assert.ok(Math.abs(a.tps - 140.5) < 0.1);
});

test('快行（102ms）：10ms 兜底不生效，500 / 0.102s ≈ 4902 在 5000 上限内', () => {
    const a = assessThroughput({ output_tokens: 500, use_time: 102, ftut: 0 });
    assert.equal(a.available, true);
    assert.equal(a.kind, 'overall');
    assert.equal(a.text, '4902');
    assert.ok(Math.abs(a.tps - 4902) < 1);
});

test('输出 50 token 的流式行：精确路径拒算，退全程平均 5.00', () => {
    const a = assessThroughput({ output_tokens: 50, use_time: 10000, ftut: 2000 });
    assert.equal(a.available, true);
    assert.equal(a.kind, 'overall');
    assert.equal(a.preciseReason, 'output_too_short');
    assert.equal(a.text, '5.00');
});

test('assessOverallThroughput：5ms 被兜到 10ms → 50000 超上限 → implausible_speed', () => {
    assert.deepEqual(
        assessOverallThroughput({ output_tokens: 500, use_time: 5 }),
        { available: false, reason: 'implausible_speed' },
    );
    // 失败与无输出行照旧不退。
    assert.equal(assessOverallThroughput({ output_tokens: 500, use_time: 5000, error: 'boom' }).reason, 'request_failed');
    assert.equal(assessOverallThroughput({ output_tokens: 0, use_time: 5000 }).reason, 'output_missing');
});

test('精确可采信的行仍给 kind generation，不带 preciseReason', () => {
    // 500 token / 3.022s 生成窗口（ftut 503，窗口占比 86%）→ ≈165 t/s
    const a = assessThroughput({ output_tokens: 500, use_time: 3525, ftut: 503 });
    assert.equal(a.available, true);
    assert.equal(a.kind, 'generation');
    assert.equal(a.preciseReason, undefined);
    assert.equal(a.text, '165');
    assert.ok(Math.abs(a.tps - 500 / 3.022) < 0.001);
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
    assert.deepEqual(assessThroughput({ ...timings, total_attempts: 3 }), { available: true, text: '125', tps: 125, kind: 'generation' });
    assert.deepEqual(assessThroughput({ ...timings, error_code: '  ', error_status: 0 }), { available: true, text: '125', tps: 125, kind: 'generation' });
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
