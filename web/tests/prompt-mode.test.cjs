const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');

const { load } = require('./load-typescript.cjs');

const {
    PROMPT_POSITION_OPTIONS,
    normalizePromptMode,
    promptPositionOption,
    promptEnabledFromSaved,
    promptLayerActive,
    promptStatus,
} = load('src/components/modules/prompt/prompt-mode.ts');

test('位置只有两个：放在前面（默认）/ 放在后面，都不删请求自带的', () => {
    assert.deepEqual(PROMPT_POSITION_OPTIONS.map((o) => o.value), ['before', 'after']);
    assert.deepEqual(PROMPT_POSITION_OPTIONS.map((o) => o.label), ['放在前面', '放在后面']);
    for (const o of PROMPT_POSITION_OPTIONS) {
        assert.ok(o.hint.includes('请求没带就直接用这段'), `${o.value} 说明要讲没带时的行为`);
        assert.ok(!/替换|删掉|没写才补/.test(o.label + o.hint), '不再有替换 / 补缺的说法');
    }
});

test('旧值、空值都按「放在前面」，after 保持', () => {
    for (const v of [undefined, null, '', 'before', 'append_system', 'replace_system', 'fill_missing', 'bogus']) {
        assert.equal(normalizePromptMode(v), 'before', String(v));
    }
    assert.equal(normalizePromptMode('after'), 'after');
    assert.equal(promptPositionOption('after').label, '放在后面');
});

test('开关：显式开就开；旧数据（没有开关字段）有文字算开', () => {
    assert.equal(promptEnabledFromSaved(true, 'after', ''), true);
    assert.equal(promptEnabledFromSaved(false, 'before', '有字'), false);
    assert.equal(promptEnabledFromSaved(undefined, 'append_system', '有字'), true);
    assert.equal(promptEnabledFromSaved(undefined, 'replace_system', '有字'), true);
    assert.equal(promptEnabledFromSaved(undefined, 'append_system', '  '), false);
    assert.equal(promptLayerActive(true, 'before', '  '), false, '开着但没字 = 不加');
    assert.equal(promptLayerActive(true, 'after', 'x'), true);
});

test('状态标签：已启用 · 位置 / 已关闭 / 未填写', () => {
    assert.equal(promptStatus(true, 'after', 'x').label, '已启用 · 放在后面');
    assert.equal(promptStatus(true, 'before', 'x').tone, 'on');
    assert.equal(promptStatus(false, 'after', 'x').label, '已关闭');
    assert.equal(promptStatus(true, 'after', ' ').label, '未填写');
});

test('三处都用开关 + 前面/后面，不再有下拉框和旧模式', () => {
    const root = path.join(__dirname, '..');
    for (const file of [
        'src/components/modules/prompt/index.tsx',
        'src/components/modules/access-plan/index.tsx',
        'src/components/modules/channel/Form.tsx',
    ]) {
        const src = readFileSync(path.join(root, file), 'utf8');
        assert.ok(src.includes('<PromptLayerControls'), `${file} 要用 PromptLayerControls`);
        assert.ok(src.includes('prompt_override_enabled'), `${file} 要读写开关`);
        assert.ok(!/fill_missing|value="append_system"|value="replace_system"/.test(src), `${file} 还留着旧模式`);
        assert.ok(/disabled=\{!/.test(src), `${file} 关掉时文本框要变灰`);
    }
    for (const loc of ['zh_hans', 'zh_hant', 'en']) {
        const flat = JSON.stringify(JSON.parse(readFileSync(path.join(root, 'public/locale', loc + '.json'), 'utf8')));
        for (const gone of ['"promptReplace"', '"promptFillMissing"', '"promptAppend"', '"promptFillMissingHint"', '"promptAppendHint"']) {
            assert.ok(!flat.includes(gone), `${loc} 还有 ${gone}`);
        }
        for (const key of ['promptEnabled', 'promptEnabledHintOff', 'promptBefore', 'promptAfter', 'promptBeforeHint', 'promptAfterHint', 'promptOffTextHint']) {
            assert.ok(flat.includes(`"${key}"`), `${loc} 缺 ${key}`);
        }
    }
});

test('方案和路由保存是整行覆盖，所有保存路径都要带上开关', () => {
    const src = readFileSync(path.join(__dirname, '..', 'src/components/modules/access-plan/index.tsx'), 'utf8');
    // 方案基本信息保存、路由整表保存、JSON 导出、JSON 导入
    const n = (src.match(/prompt_override_enabled: promptEnabledFromSaved\(/g) || []).length;
    assert.ok(n >= 4, `只有 ${n} 处带开关`);
});
