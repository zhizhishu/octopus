const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const {
    normalizeAudience, promptValuesEqual, editPromptDraft, acceptPromptSave,
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

test('管理UI只改普通提示词配置、包含折叠与管理者专用范围，无自行改模型证据', () => {
    const source = readFileSync(path.join(__dirname, '../src/components/modules/prompt/index.tsx'), 'utf8');
    assert.match(source, /PromptDraftsContext\.Provider/);
    assert.match(source, /expandedChannelTypes/);
    assert.match(source, /groupChannelsByType\(visibleChannels\)/);
    assert.match(source, /scopeKey=\{\x60channel:/);
    assert.match(source, /scopeKey=\{\x60plan:/);
    assert.match(source, /option value="admin_only"/);
    assert.match(source, /relatedPlans\.length > 1/);
    assert.match(source, /prompt_audience: next\.audience/);
    assert.doesNotMatch(source, /applyPromptOverrides\(/, 'UI不能本地自造relay规则');
});
