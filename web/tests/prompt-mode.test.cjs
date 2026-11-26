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
    // 规则页（新结构：左列表 + 右编辑器）。写入面是规则表，不再是旧四层的逐层字段。
    const promptPage = read('src/components/modules/prompt/index.tsx');
    const ruleEditor = read('src/components/modules/prompt/rule-editor.tsx');
    const ruleList = read('src/components/modules/prompt/rule-list.tsx');
    assert.ok(promptPage.includes('<RuleList') && promptPage.includes('<RuleEditor'), '提示词页要有规则列表和编辑区');
    for (const hook of ['usePromptRuleList', 'useCreatePromptRule', 'useUpdatePromptRule', 'useDeletePromptRule', 'useTogglePromptRule']) {
        assert.ok(promptPage.includes(hook), `规则页要能改：${hook}`);
    }
    for (const legacy of ['useUpdateAccessPlanPrompt', 'useUpdateAccessPlanRoutePrompt', 'useUpdateChannel', 'SettingKey.PromptOverrideEnabled']) {
        assert.ok(!promptPage.includes(legacy), `提示词页不该再逐层写旧字段：${legacy}`);
    }
    // 关闭只影响生效，仍可编辑与保存草稿：编辑器不许因为规则关着就禁用输入。
    assert.ok(!/disabled=\{!value\.enabled\}/.test(ruleEditor), '关闭只影响生效，仍可编辑与保存草稿');
    assert.ok(!/disabled=\{!rule\.enabled\}/.test(ruleEditor), '规则关着也要能改文字');
    // 关闭的规则在列表里用删除线标出来，而不是藏掉。
    assert.ok(ruleList.includes('line-through decoration-muted-foreground/40'), '关闭的规则用删除线区分草稿状态');
    assert.ok(!/fill_missing|append_system|replace_system/.test(promptPage + ruleEditor), '提示词页不再有旧模式');

    const planRaw = read('src/components/modules/access-plan/index.tsx');
    assert.ok(planRaw.includes('withoutPromptFields(target)'), '路由整表保存要剥掉列表回显的提示词字段');
    // 去掉注释和 withoutPromptFields 本身，剩下的代码里不该再有任何提示词相关的东西。
    const plan = planRaw
        .replace(/\/\*[\s\S]*?\*\//g, '')
        .replace(/\/\/.*$/gm, '')
        .replace(/function withoutPromptFields[\s\S]*?\r?\n}\r?\n/, '');
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

test('提示词页主屏只留短句，细节放进 ? 里', () => {
    const root = path.join(__dirname, '..');
    const read = (f) => readFileSync(path.join(root, f), 'utf8');
    const src = read('src/components/modules/prompt/index.tsx');
    const editor = read('src/components/modules/prompt/rule-editor.tsx');
    const list = read('src/components/modules/prompt/rule-list.tsx');
    assert.ok(!/<h2[^>]*>\s*提示词\s*<\/h2>/.test(src), '页头已经写着「提示词」，卡片里不再重复大标题');
    assert.ok(!src.includes('ORDER_CHIPS') && !src.includes('最终顺序'), '主屏不堆顺序小标签');
    assert.ok(!src.includes('拼接顺序') && !src.includes('<OrderPopover'), '顺序说明搬去合并预览，规则页不再有弹层');
    assert.ok(!/`\$\{planOn\}\+\$\{routeOn\}`/.test(src), '筛选数量只显示一个数');
    assert.ok(!src.includes('方案层 · ') && !src.includes('路由层 · '), '不再有「方案层 · …」「路由层 · …」这种长小标题');
    // 新主屏：规则卡片列表 + 右侧编辑区；筛选是一排短 chip（「全部 N」+ 范围 chip + 「只看开着的」）。
    assert.ok(src.includes('<RuleList') && src.includes('<RuleEditor'), '规则页是左列表右编辑');
    // 宽屏布局：左边列表固定 360px、右边编辑区自适应（a9789da3 排版重做后的形态）。
    assert.ok(/md:grid-cols-\[360px_minmax\(0,1fr\)\]/.test(src), '宽屏列表 360px 在左、编辑区在右');
    assert.ok(list.includes('全部 {rules.length}') && list.includes('只看开着的'), '筛选是一排短 chip');
    // 叠加顺序只在合并预览里讲，并逐段标注「前面 / 后面 / 客户端」。
    const preview = read('src/components/modules/prompt/merge-preview.tsx');
    assert.ok(preview.includes('usePreviewPromptRules('), '合并预览的命中与合并文本走后端 /preview');
    assert.ok(preview.includes('不会调用模型'), '预览页要说明只在本站算、不调模型');
    assert.ok(!preview.includes('previewPromptRules('), '前端不再自己拼一遍');
    assert.ok(preview.includes("'放在后面'") && preview.includes("'放在前面'"), '每段标注放在前面/放在后面');
    assert.ok(preview.includes('客户端'), '客户端自带的系统提示词固定标注');
    assert.ok(editor.includes("variant={dirty && !saveProblem ? 'default' : 'outline'}"), '没改动时保存按钮是灰的，改了才是主按钮');
    const form = readFileSync(path.join(root, 'src/components/modules/channel/Form.tsx'), 'utf8');
    assert.ok(form.includes("setActiveNavItem('prompt')"), '渠道表单的说明可以点过去提示词页');
});
