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

test('提示词只在提示词页改：方案页和渠道表单里没有提示词编辑', () => {
    const root = path.join(__dirname, '..');
    const read = (f) => readFileSync(path.join(root, f), 'utf8');
    const promptPage = read('src/components/modules/prompt/index.tsx');
    assert.ok(promptPage.includes('<PromptLayerControls') || promptPage.includes('<PromptEditor'), '提示词页要有编辑区');
    for (const hook of ['useUpdateAccessPlanPrompt', 'useUpdateAccessPlanRoutePrompt', 'useUpdateChannel', 'SettingKey.PromptOverrideEnabled']) {
        assert.ok(promptPage.includes(hook), `提示词页要能改：${hook}`);
    }
    assert.ok(/disabled=\{!value\.enabled\}/.test(promptPage), '关掉时文本框要变灰');
    assert.ok(!/fill_missing|append_system|replace_system/.test(promptPage), '提示词页不再有旧模式');

    const planRaw = read('src/components/modules/access-plan/index.tsx');
    assert.ok(planRaw.includes('withoutPromptFields(target)'), '路由整表保存要剥掉列表回显的提示词字段');
    // 去掉注释和 withoutPromptFields 本身，剩下的代码里不该再有任何提示词相关的东西。
    const plan = planRaw
        .replace(/\/\*[\s\S]*?\*\//g, '')
        .replace(/\/\/.*$/gm, '')
        .replace(/function withoutPromptFields[\s\S]*?\n}\n/, '');
    assert.ok(!plan.includes('提示词'), '方案页不该再出现提示词文案');
    assert.ok(!/prompt/i.test(plan.replace(/withoutPromptFields/g, '')), '方案页不该再引用提示词字段或组件');

    for (const f of ['src/components/modules/channel/Form.tsx', 'src/components/modules/channel/CardContent.tsx', 'src/components/modules/channel/Create.tsx']) {
        const src = read(f);
        assert.ok(!src.includes('PromptLayerControls') && !src.includes('system_prompt_override') && !src.includes('prompt_override_'), `${f} 还在编辑提示词`);
    }
    assert.ok(read('src/components/modules/channel/Form.tsx').includes("t('promptMovedHint')"), '渠道表单留一句去哪改的说明');

    for (const loc of ['zh_hans', 'zh_hant', 'en']) {
        const flat = JSON.stringify(JSON.parse(read('public/locale/' + loc + '.json')));
        for (const gone of ['"promptReplace"', '"promptFillMissing"', '"promptAppend"', '"promptOverridePlaceholder"', '"promptBefore"']) {
            assert.ok(!flat.includes(gone), `${loc} 还有 ${gone}`);
        }
        assert.ok(flat.includes('"promptMovedHint"'), `${loc} 缺 promptMovedHint`);
    }
});

test('方案 API 类型：方案保存不带提示词，提示词走单独的接口', () => {
    const src = readFileSync(path.join(__dirname, '..', 'src/api/endpoints/access-plan.ts'), 'utf8');
    const update = src.slice(src.indexOf('export type UpdateAccessPlanRequest'), src.indexOf('export type PromptOverridePatch'));
    assert.ok(!update.includes('prompt'), 'UpdateAccessPlanRequest 不该有提示词字段');
    assert.ok(src.includes("'/api/v1/access-plan/prompt/update'") && src.includes("'/api/v1/access-plan/route-prompt/update'"));
});
