const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const {
    normalizeAudience, promptValuesEqual, previewPromptLayer, editPromptDraft, acceptPromptSave,
    channelPlanNames, channelIDsForPlan, groupChannelsByType, CHANNEL_TYPE_LABELS,
} = load('src/components/modules/prompt/prompt-ui-state.ts');

function val(text, enabled = true, audience = 'all') {
    return { text, enabled, mode: 'before', audience };
}

test('作用域默认沿用旧版全部用户，仅管理员需显式选择', () => {
    assert.equal(normalizeAudience(undefined), 'all');
    assert.equal(normalizeAudience('wrong'), 'all');
    assert.equal(normalizeAudience('admin_only'), 'admin_only');
    assert.equal(promptValuesEqual(val(' a '), val('a')), true);
    assert.equal(promptValuesEqual(val('x'), val('x', true, 'admin_only')), false);
});

test('离线合并预览与实际本层前后插入语义相符，关闭不影响客户端原文', () => {
    const client = '客户端原有system';
    assert.equal(previewPromptLayer({ ...val('品牌A'), mode: 'before' }, client), '品牌A\n客户端原有system');
    assert.equal(previewPromptLayer({ ...val('品牌A'), mode: 'after' }, client), '客户端原有system\n品牌A');
    assert.equal(previewPromptLayer(val('保留的关闭草稿', false), client), client);
    assert.equal(previewPromptLayer(val('   '), client), client);
});

test('折叠/取消筛选后仍保留草稿，async server refetch 不覆盖用户文字', () => {
    const oldValue = val('旧规则');
    let drafts = editPromptDraft({}, 'plan:12', oldValue, (current) => ({ ...current, text: '新的未保存草稿' }));
    assert.equal(drafts['plan:12'].value.text, '新的未保存草稿');
    assert.equal(drafts['plan:12'].baseline.text, '旧规则');
    drafts = editPromptDraft(drafts, 'plan:12', val('后台更新'), (current) => ({ ...current, enabled: false }));
    assert.equal(drafts['plan:12'].value.text, '新的未保存草稿');
    assert.equal(drafts['plan:12'].baseline.text, '旧规则');
});

test('较旧保存结果回到浏览器时不能覆盖继续输入的新草稿', () => {
    const saved = val('旧');
    let drafts = editPromptDraft({}, 'channel:42', saved, (current) => ({ ...current, text: '提交时' }));
    const submitted = drafts['channel:42'].value;
    drafts = editPromptDraft(drafts, 'channel:42', saved, (current) => ({ ...current, text: '继续输入' }));
    drafts = acceptPromptSave(drafts, 'channel:42', submitted);
    assert.equal(drafts['channel:42'].value.text, '继续输入');
    assert.equal(drafts['channel:42'].baseline.text, '提交时');
    assert.equal(promptValuesEqual(drafts['channel:42'].value, drafts['channel:42'].baseline), false);
});

test('同一渠道关联VIP/SVIP必须列出两套方案，不能被筛选当成授权范围', () => {
    const plans = [
        { id: 1, display_name: 'VIP', slug: 'vip', route_targets: [
            { channel_id: 41 }, { channel_id: 41 }, { channel_id: 42 },
        ] },
        { id: 2, display_name: 'SVIP', slug: 'svip', route_targets: [
            { channel_id: 41 },
        ] },
    ];
    assert.deepEqual(channelPlanNames(plans).get(41), ['VIP', 'SVIP']);
    assert.deepEqual([...channelIDsForPlan(plans[0])].sort(), [41, 42]);
    assert.equal(channelIDsForPlan(plans[1]).has(42), false);
});

test('200渠道依类型分组且稳定ID排序，保留退役type=4未知标识', () => {
    const list = Array.from({ length: 200 }, (_, i) => ({ id: 200 - i, type: i % 2 ? 2 : 1 }));
    list.push({ id: 1000, type: 4 });
    const groups = groupChannelsByType(list);
    assert.equal(groups.reduce((n, group) => n + group.channels.length, 0), 201);
    assert.deepEqual(groups.map((group) => group.type), [1, 2, 4]);
    assert.equal(groups.at(-1).label, '其他类型 (4)');
    for (const group of groups) {
        const ids = group.channels.map((c) => c.id);
        assert.deepEqual(ids, [...ids].sort((a, b) => a - b));
    }
    assert.equal(CHANNEL_TYPE_LABELS[4], undefined, '4号渠道类型不能被复用');
});

test('管理UI只改普通提示词配置、草稿不丢且含管理者专用范围，无自行改模型证据', () => {
    const read = (f) => readFileSync(path.join(__dirname, '../src/components/modules/prompt/' + f), 'utf8');
    const source = read('index.tsx');
    const editor = read('rule-editor.tsx');
    const model = read('rule-model.ts');
    const list = read('rule-list.tsx');
    const preview = read('merge-preview.tsx');

    // 草稿仍由页面统一保管（新结构是 rule-model 的草稿函数），切条目/切页签不丢。
    assert.match(source, /useState<PromptRuleDraftMap>\(\{\}\)/);
    for (const fn of ['editRuleDraft', 'acceptRuleDraftSave', 'dirtyRuleDraftKeys']) {
        assert.ok(source.includes(`${fn}(`), `页面要用草稿函数：${fn}`);
        assert.match(model, new RegExp(`export function ${fn}`), `rule-model 要提供：${fn}`);
    }
    assert.match(model, /export function promptRuleValuesEqual/);
    // 删除要二次确认，不能一点就删。
    assert.match(editor, /confirmDelete \? \(\) => \{ setConfirmDelete\(false\); onDelete\(\); \} : \(\) => setConfirmDelete\(true\)/);
    // 还没保存过的新规则不能删/复制（否则点了没反应），保存被拦时要把原因露在页面上。
    assert.match(editor, /disabled=\{deleting \|\| !saved\}/);
    assert.match(editor, /disabled=\{saving \|\| copying \|\| !saved\}/);
    assert.match(editor, /role="alert"[^>]*>\{saveProblem\}/);
    // 窄屏删除初态就是「删除」，不能一上来就写「确认？」。
    assert.match(editor, /confirmDelete \? '确认删除' : '删除'/);
    // 模型输入框显示用户原文（尾逗号/换行不被吞）、不再拦 Enter，草稿存解析后的值。
    assert.match(editor, /value=\{patternsText\}/);
    assert.match(editor, /setPatternsText\(raw\)/);
    assert.match(editor, /models: parseModelPatterns\(raw\)/);
    assert.match(editor, /promptRuleModelsTextSync\(patternsText, patternSignature\)/);
    assert.doesNotMatch(editor, /event\.key === 'Enter'/, 'Enter 不能再被 preventDefault，要能换行');
    // 受众含「仅管理员 Key」，写的是规则表的 audience（不是旧四层的 prompt_audience）。
    assert.match(editor, /\{ value: 'admin_only', label: '仅管理员 Key' \}/);
    assert.match(model, /audience: rule\.audience/);
    // 一条规则能同时勾多个方案（多方案 chip 多选，不是单选下拉）。
    assert.match(source, /plans=\{planOptions\}/);
    assert.match(editor, /value\.plan_mode === 'selected' && value\.plan_ids\.includes\(plan\.id\)/);
    // 渠道作用域三态仍在（所有 / 只在这些 / 除了这些），逐渠道勾选写 channel_ids。
    assert.match(editor, /\{ value: 'include', label: '只在这些' \}/);
    assert.match(editor, /\{ value: 'exclude', label: '除了这些' \}/);
    assert.match(editor, /channel_ids: current\.channel_ids\.includes\(channel\.id\)/);
    // 渠道分组/折叠能力仍可用（数据源还在 prompt-ui-state），列表也能按渠道筛规则。
    assert.deepEqual(groupChannelsByType([{ id: 2, type: 2 }, { id: 1, type: 2 }]).map((group) => group.type), [2]);
    assert.match(list, /promptRuleChannelAllows\(rule, channelFilter\)/);
    // 合并预览走独立面板，命中与合并文本都由后端 /preview 算（前端不自拼、不重排）。
    assert.match(source, /<MergePreview/);
    assert.match(preview, /usePreviewPromptRules\(/);
    assert.match(preview, /promptRulePreviewView\(normalizePromptRulePreview\(response\)\)/);
    assert.doesNotMatch(preview, /previewPromptRules\(|evaluatePromptRules\(/, '前端不再自己算命中/拼接');
    assert.doesNotMatch(model, /export function previewPromptRules/, '本地拼接已删，避免与后端的模型名规范化对不上');
    assert.match(preview, /不会调用模型/, '预览页要说明只在本站算、不调模型');
    // 左栏是应用先后、越具体的越靠外：不能再说反。
    assert.match(preview, /越具体的规则在最终提示词里越靠外/);
    assert.match(preview, /左栏从上到下是加进去的先后/);
    assert.doesNotMatch(preview, /范围小的靠里/);
    // 排序入口：列表可上移/下移，页面调 /reorder 提交全量顺序。
    assert.match(list, /promptRuleCanMove\(rules, rule\.id, 'up'\)/);
    assert.match(source, /useReorderPromptRules\(/);
    assert.match(model, /export function promptRuleReorderIDs/);
    // 开关成功后同步草稿里的 enabled（脏草稿只改 enabled，不能把文字弄丢）。
    assert.match(source, /baseline: \{ \.\.\.entry\.baseline, enabled \}/);
    // 复制命名按码点截断，不在页面里自己拼 " 副本"。
    assert.match(source, /promptRuleCopyName\(source\.name\)/);
    assert.match(model, /export function promptRuleCopyName/);
    // 离开提示：有脏草稿时拦刷新/关页，卸掉页面时也提醒一句。
    assert.match(source, /addEventListener\('beforeunload'/);
    // 界面不讲表结构/实现细节。
    assert.doesNotMatch(source, /规则只存在一张表里/);
    assert.doesNotMatch(editor, /disabled=\{!value\.enabled\}|disabled=\{!rule\.enabled\}/, '关闭状态也要允许编辑文本草稿');
    assert.doesNotMatch(source + editor, /applyPromptOverrides\(/, 'UI不能本地自造relay规则');
});
