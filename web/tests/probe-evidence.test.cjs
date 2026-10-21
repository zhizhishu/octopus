const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { describeProbeEvidence } = load('src/components/modules/log/probe-evidence.ts');

test('把 token 加减法的证据翻成中文名，并给出可复算的式子', () => {
    const rows = describeProbeEvidence('token_arithmetic', {
        input: 1200,
        output: 300,
        sum: 1500,
        total: 1700,
        diff: 200,
    });

    assert.deepEqual(
        rows.map((r) => r.label),
        ['输入 token', '输出 token', '输入 + 输出', '日志记的总数', '差额']
    );

    // 数字必须原样露出，不加工、不加千分位——用户是来核对的。
    assert.equal(rows.find((r) => r.label === '日志记的总数').value, '1700');

    // 关键的两行要能自己算出来。
    assert.equal(rows.find((r) => r.label === '输入 + 输出').formula, '1200 + 300 = 1500');
    assert.equal(rows.find((r) => r.label === '差额').formula, '1700 - 1500 = 200');
});

test('缓存超输入的证据同样给得出加法式子', () => {
    const rows = describeProbeEvidence('cache_exceeds_prompt', {
        cache_hit: 1500,
        cache_write: 400,
        cache_total: 1900,
        input_tokens: 1200,
    });

    assert.equal(rows.find((r) => r.label === '缓存合计').formula, '1500 + 400 = 1900');
    assert.equal(rows.find((r) => r.label === '输入 token').value, '1200');
});

test('字符/token 比值给的是除法式子，不是把 ratio 原样抄一遍', () => {
    const rows = describeProbeEvidence('char_token_ratio', {
        chars: 900,
        output_tokens: 100,
        ratio: 9,
        min: 1.2,
        max: 10,
    });

    assert.equal(rows.find((r) => r.label === '字符 / token').formula, '900 / 100 = 9.00');
});

test('分母是 0 时不编式子，宁可只给数字', () => {
    const rows = describeProbeEvidence('char_token_ratio', {
        chars: 900,
        output_tokens: 0,
        ratio: 0,
    });

    const ratioRow = rows.find((r) => r.label === '字符 / token');
    assert.equal(ratioRow.formula, undefined);
    assert.equal(ratioRow.value, '0');
});

test('认不出的键必须原样保留——丢掉它等于让证据面板假装完整', () => {
    const rows = describeProbeEvidence('some_future_probe', {
        brand_new_field: 42,
        another_one: 'hello',
    });

    assert.deepEqual(
        rows.map((r) => [r.label, r.value]),
        [
            ['brand_new_field', '42'],
            ['another_one', 'hello'],
        ]
    );
});

test('已知探针里夹了未知键时，未知键排在已知键之后而不是被吞掉', () => {
    const rows = describeProbeEvidence('empty_output_with_tokens', {
        output: 128,
        unexpected_extra: true,
    });

    assert.equal(rows[0].label, '输出 token');
    assert.equal(rows[1].label, 'unexpected_extra');
    assert.equal(rows[1].value, '是');
});

test('缺 evidence 或空对象时返回空数组，不抛异常', () => {
    assert.deepEqual(describeProbeEvidence('token_arithmetic', undefined), []);
    assert.deepEqual(describeProbeEvidence('token_arithmetic', {}), []);
});

test('null / 未定义的值显示成破折号而不是 "null" 字样', () => {
    const rows = describeProbeEvidence('token_arithmetic', {
        input: 1,
        output: null,
    });

    assert.equal(rows.find((r) => r.label === '输出 token').value, '—');
});

test('数字字符串也能参与算式子（后端 JSON 反序列化可能给字符串）', () => {
    const rows = describeProbeEvidence('token_arithmetic', {
        input: '1200',
        output: '300',
        sum: 1500,
        total: 1700,
    });

    assert.equal(rows.find((r) => r.label === '输入 + 输出').formula, '1200 + 300 = 1500');
});

test('展示顺序按探针码固定，跟对象里键的物理顺序无关', () => {
    // 故意把 diff 写在最前面，输出仍应该是 input 在前。
    const rows = describeProbeEvidence('token_arithmetic', {
        diff: 200,
        total: 1700,
        input: 1200,
        output: 300,
        sum: 1500,
    });

    assert.equal(rows[0].label, '输入 token');
    assert.equal(rows[rows.length - 1].label, '差额');
});
