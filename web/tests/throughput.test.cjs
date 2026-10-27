const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { describeThroughput } = load('src/components/modules/log/throughput.ts');

// 2026-10-27 起的新口径（astra 裁决，生产爆表实证驱动）：
// 生成窗口不可采信 → 不显示（null），绝不退回全程平均冒充生成速度。
// 阈值与 internal/op/throughput.go 一一对应，改一边必须同步另一边。

test('正常流式窗口：只算首字之后的生成窗口', () => {
    // 1000 个输出 token，总耗时 10s，其中首字占 2s → 生成 8s → 125 t/s
    const r = describeThroughput({ output_tokens: 1000, use_time: 10000, ftut: 2000 });
    assert.deepEqual(r, { text: '125' });
});

test('爆表原型：ftut 落在 use_time-1ms（非流式合成流）→ 不显示', () => {
    // 生产实证：5874 token 曾显示成 5,874,000 tok/s。
    assert.equal(describeThroughput({ output_tokens: 5874, use_time: 24133, ftut: 24132 }), null);
});

test('生成窗口占比不足 25% → 不显示', () => {
    // 窗口 1s 达标但只占总耗时 10%：首字落在 90% 处，窗口不可采信。
    assert.equal(describeThroughput({ output_tokens: 500, use_time: 10000, ftut: 9000 }), null);
});

test('输出不足 100 token → 不显示（窗口量的是网络冲刷不是生成）', () => {
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

test('超出现实上限（测量坏了不是上游快）→ 不显示', () => {
    assert.equal(describeThroughput({ output_tokens: 5000, use_time: 3000, ftut: 1000 }), null);
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
