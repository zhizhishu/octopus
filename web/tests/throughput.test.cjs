const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { describeThroughput } = load('src/components/modules/log/throughput.ts');

test('首字有效：只算首字之后的生成窗口', () => {
    // 1000 个输出 token，总耗时 10s，其中首字占 2s → 生成 8s → 125 t/s
    const r = describeThroughput({ output_tokens: 1000, use_time: 10000, ftut: 2000 });
    assert.deepEqual(r, { text: '125', window: 'gen' });
});

test('口径与后端一致：不看 is_stream，只认 ftut > 0 且 < use_time', () => {
    // 后端 internal/op/model_health.go 就是这么判的。多带个 is_stream:false 也不能改变结果——
    // 否则日志卡会和首页「平均吞吐」给出两个数（ftut 本来就只在流式路径写）。
    const withFlag = describeThroughput({
        output_tokens: 1000,
        use_time: 10000,
        ftut: 2000,
        is_stream: false,
    });
    assert.deepEqual(withFlag, { text: '125', window: 'gen' });
});

test('首字缺失（上游没报）退回全程平均，而不是不显示', () => {
    const missing = describeThroughput({ output_tokens: 500, use_time: 5000, ftut: 0 });
    assert.deepEqual(missing, { text: '100', window: 'total' });

    // 老日志干脆没这个字段也一样。
    const absent = describeThroughput({ output_tokens: 500, use_time: 5000 });
    assert.deepEqual(absent, { text: '100', window: 'total' });
});

test('首字 ≥ 总耗时这种脏数据不许算出 NaN / 负数 / Infinity', () => {
    const equal = describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 4000 });
    assert.deepEqual(equal, { text: '75.0', window: 'total' });

    const bigger = describeThroughput({ output_tokens: 300, use_time: 4000, ftut: 9000 });
    assert.deepEqual(bigger, { text: '75.0', window: 'total' });
});

test('算不出来就返回 null，让界面整块不渲染——不拿 0 占位', () => {
    assert.equal(describeThroughput({ output_tokens: 0, use_time: 5000 }), null);
    assert.equal(describeThroughput({ output_tokens: 100, use_time: 0 }), null);
    assert.equal(describeThroughput({}), null);
    assert.equal(describeThroughput({ output_tokens: -5, use_time: 5000 }), null);
});

test('小数值不许被舍成 0.0（看着像没数据），极小的给下限写法', () => {
    // 1 个 token / 25s = 0.04 t/s，一位小数会变成 "0.0"
    assert.equal(describeThroughput({ output_tokens: 1, use_time: 25000 }).text, '0.04');
    // 1 个 token / 100000s ≈ 0.00001 → 低于可显示精度
    assert.equal(describeThroughput({ output_tokens: 1, use_time: 100000000 }).text, '<0.01');
    // 三位数以上不要小数位，免得撑宽
    assert.equal(describeThroughput({ output_tokens: 200000, use_time: 1000 }).text, '200000');
});
