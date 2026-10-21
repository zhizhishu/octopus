const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { neutralizeFormula, toCellValue, formatBytes } = load('src/lib/log-export.ts');

// 公式注入是真洞，不是洁癖：导出的 JSON 里那些字段（模型名、错误信息、用户备注）
// 全是上游或用户可控的。以 = + - @ 开头的单元格被 Excel/WPS/Numbers 打开时会当
// 公式执行，`=cmd|'/c calc'!A1` 在旧版 Excel 上真能起进程。下面几条锁死这件事。

test('以等号开头的值会被加引号，不能再被当成公式', () => {
    assert.equal(neutralizeFormula('=SUM(A1:A9)'), "'=SUM(A1:A9)");
    assert.equal(neutralizeFormula('=cmd|\'/c calc\'!A1'), "'=cmd|'/c calc'!A1");
});

test('加号、减号、@、制表符、回车开头同样中和', () => {
    assert.equal(neutralizeFormula('+1+1'), "'+1+1");
    assert.equal(neutralizeFormula('@SUM(1)'), "'@SUM(1)");
    assert.equal(neutralizeFormula('\t=1+1'), "'\t=1+1");
    assert.equal(neutralizeFormula('\r=1+1'), "'\r=1+1");
    // 减号开头但是数字的情况在下面单独测——那是数字不是公式。
    assert.equal(neutralizeFormula('-1+1'), "'-1+1");
});

test('负数不加引号——它是数字，加了反而变文本', () => {
    assert.equal(neutralizeFormula('-12'), '-12');
    assert.equal(neutralizeFormula('-12.5'), '-12.5');
    assert.equal(neutralizeFormula('-0.001'), '-0.001');
});

test('普通文本原样返回，不会被乱加引号', () => {
    for (const value of ['claude-sonnet-4-5', 'gpt-5.5', '错误：超时', 'a=b', '1+1']) {
        assert.equal(neutralizeFormula(value), value, `${value} 不该被改`);
    }
});

test('空值、null、undefined 都不会崩，也不产出引号', () => {
    assert.equal(neutralizeFormula(''), '');
    assert.equal(toCellValue(null), '');
    assert.equal(toCellValue(undefined), '');
});

test('对象和数组摊成 JSON 字符串，而不是 [object Object]', () => {
    assert.equal(toCellValue({ a: 1 }), '{"a":1}');
    assert.equal(toCellValue([1, 2]), '[1,2]');
});

test('对象序列化后若以危险字符开头，同样走中和', () => {
    // JSON.stringify 对字符串会加双引号，所以这里实际是 '["=cmd"]' 的形式；
    // 真正危险的是**值本身**被当单元格用的情况，用字符串直接测更贴近现实。
    assert.equal(toCellValue('=cmd|\'/c calc\'!A1'), "'=cmd|'/c calc'!A1");
});

test('数字与布尔保持字面量，不加引号', () => {
    assert.equal(toCellValue(0), '0');
    assert.equal(toCellValue(-12), '-12');
    assert.equal(toCellValue(true), 'true');
    assert.equal(toCellValue(false), 'false');
});

test('字节数按 1024 进制显示，0 和非法值不出现 NaN', () => {
    assert.equal(formatBytes(0), '0 B');
    assert.equal(formatBytes(512), '512 B');
    assert.equal(formatBytes(1024), '1.0 KB');
    assert.equal(formatBytes(1536), '1.5 KB');
    assert.equal(formatBytes(1024 * 1024), '1.0 MB');
    assert.equal(formatBytes(NaN), '0 B');
    assert.equal(formatBytes(-5), '0 B');
});
