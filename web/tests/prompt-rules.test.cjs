/* eslint-disable @typescript-eslint/no-require-imports */
// Run from web/: node --test tests/prompt-rules.test.cjs
// 提示词规则页的回归测试：列表顺序/上移下移、筛选、保存前校验、草稿、以及
// 合并预览对后端 /preview 结果的展示（不重排、不补字）。全部离线执行，不发请求。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const m = load('src/components/modules/prompt/rule-model.ts');
const api = load('src/api/endpoints/prompt-rule.ts', {
    '@/lib/logger': { logger: { error() {}, log() {} } },
});

function rule(overrides = {}, id = 1) {
    return {
        id,
        name: `规则 ${id}`,
        enabled: true,
        text: '示例提示词',
        position: 'before',
        plan_mode: 'all',
        plan_ids: [],
        include_no_plan: false,
        channel_mode: 'all',
        channel_ids: [],
        model_mode: 'all',
        models: [],
        audience: 'all',
        sort: 0,
        ...overrides,
    };
}

const scopeOf = (planId = 0, model = '', channelId = 10, isAdmin = false) => ({ plan_id: planId, request_model: model, channel_id: channelId, is_admin: isAdmin });

// ── 列表顺序 / 上移下移 ──

test('列表顺序 = 叠加顺序：全局 → 方案 → 模型 → 渠道，同级按 sort、再按 id', () => {
    const rules = [
        rule({ id: 5, sort: 0, channel_mode: 'include', channel_ids: [10] }), // 渠道
        rule({ id: 2, sort: 5 }),                                             // 全局
        rule({ id: 3, sort: 0, model_mode: 'selected', models: ['gpt-*'] }),  // 模型
        rule({ id: 1, sort: 5, plan_mode: 'selected', plan_ids: [7] }),       // 方案
        rule({ id: 4, sort: 1 }),                                             // 全局，sort 更小
    ];
    assert.deepEqual(m.promptRuleOrdered(rules).map((row) => row.id), [4, 2, 1, 3, 5]);
    assert.deepEqual(rules.map((row) => row.id), [5, 2, 3, 1, 4], '排序不能改入参');
});

test('同层里上下移动：返回全量 id，跨层和到边界不动', () => {
    const rules = [
        rule({ id: 10, sort: 0 }),
        rule({ id: 11, sort: 1 }),
        rule({ id: 12, sort: 2, plan_mode: 'selected', plan_ids: [7] }),
    ];
    // 同层相邻：11 上移和 10 换位置，12 仍排最后。
    assert.deepEqual(m.promptRuleReorderIDs(rules, 11, 'up'), [11, 10, 12]);
    assert.equal(m.promptRuleCanMove(rules, 11, 'up'), true);
    // 最上面那条不能再上移。
    assert.equal(m.promptRuleReorderIDs(rules, 10, 'up'), null);
    assert.equal(m.promptRuleCanMove(rules, 10, 'up'), false);
    // 跨层不动：12 上面是全局层，不给换。
    assert.equal(m.promptRuleReorderIDs(rules, 12, 'up'), null);
    // 不存在的 id 不动。
    assert.equal(m.promptRuleReorderIDs(rules, 404, 'down'), null);
    assert.equal(m.promptRuleCanMove(rules, 10, 'down'), true);
});

// ── 筛选（只看范围，不做命中判定） ──

test('方案筛选：selected 只看 plan_ids，没选方案的 key 由 include_no_plan 兜底', () => {
    const selected = rule({ id: 1, plan_mode: 'selected', plan_ids: [7] });
    assert.equal(m.promptRulePlanAllows(selected, 7), true);
    assert.equal(m.promptRulePlanAllows(selected, 8), false);
    assert.equal(m.promptRulePlanAllows(selected, 0), false);
    const withKeyless = rule({ id: 2, plan_mode: 'selected', plan_ids: [], include_no_plan: true });
    assert.equal(m.promptRulePlanAllows(withKeyless, 0), true);
    assert.equal(m.promptRulePlanAllows(withKeyless, 7), false);
    const all = rule({ id: 3 });
    assert.equal(m.promptRulePlanAllows(all, 0), true);
});

test('渠道筛选：all / 只在这些 / 除了这些', () => {
    const all = rule({ id: 1 });
    const include = rule({ id: 2, channel_mode: 'include', channel_ids: [10, 11] });
    const exclude = rule({ id: 3, channel_mode: 'exclude', channel_ids: [10] });
    assert.equal(m.promptRuleChannelAllows(all, 10), true);
    assert.equal(m.promptRuleChannelAllows(include, 10), true);
    assert.equal(m.promptRuleChannelAllows(include, 12), false);
    assert.equal(m.promptRuleChannelAllows(exclude, 10), false);
    assert.equal(m.promptRuleChannelAllows(exclude, 12), true);
});

test('模型筛选：精确名 / 尾星通配 / 不分大小写；pattern 去空后比对', () => {
    const rules = rule({ id: 1, model_mode: 'selected', models: ['claude-*', ' gemini-2.5-pro '] });
    assert.equal(m.promptRuleModelAllows(rules, 'CLAUDE-SONNET-4'), true);
    assert.equal(m.promptRuleModelAllows(rules, 'gemini-2.5-pro'), true);
    assert.equal(m.promptRuleModelAllows(rules, 'gemini-2.5-flash'), false);
    assert.equal(m.promptRuleModelAllows(rules, ''), false);
    // 筛选不是命中判定：后端匹配前会做 1M 别名清洗（claude-sonnet-4-1m → claude-sonnet-4），
    // 这里没有那份规范化，所以精确名对不上就返回 false，命中一律以 /preview 为准。
    const exact = rule({ id: 2, model_mode: 'selected', models: ['claude-sonnet-4'] });
    assert.equal(m.promptRuleModelAllows(exact, 'claude-sonnet-4-1m'), false);
});

// ── 合并预览：只展示后端 /preview 的结果 ──

function serverRule(overrides = {}) {
    return {
        id: 1,
        name: '品牌身份',
        enabled: true,
        text: '你是 Oct 智能助手',
        position: 'before',
        plan_mode: 'all',
        plan_ids: null,
        include_no_plan: false,
        channel_mode: 'all',
        channel_ids: null,
        model_mode: 'all',
        models: null,
        audience: 'all',
        sort: 0,
        created_at: null,
        updated_at: null,
        ...overrides,
    };
}

test('后端响应规范化：null 的列表列变空数组、未知枚举收敛，命中顺序照后端不动', () => {
    const response = {
        matches: [
            { rule: serverRule({ id: 9, channel_mode: 'include', channel_ids: null }), matched: true, reason: 'matched' },
            { rule: serverRule({ id: 2, plan_mode: 'selected', plan_ids: [7], models: null }), matched: false, reason: 'channel_scope' },
        ],
        segments: [{ rule_id: 9, name: '品牌身份', position: 'before', text: '你是 Oct 智能助手' }],
        text: '你是 Oct 智能助手',
    };
    const view = m.promptRulePreviewView(api.normalizePromptRulePreview(response));
    assert.deepEqual(view.matches.map((row) => row.rule.id), [9, 2], '后端给的顺序不能被前端按 id/sort 重排');
    assert.deepEqual(view.matches[1].rule.plan_ids, [7]);
    assert.deepEqual(view.matches[0].rule.channel_ids, [], 'null → []');
    assert.deepEqual(view.matches[0].rule.models, []);
    assert.equal(view.matches[1].rule.position, 'before');
    assert.equal(view.matches[1].rule.audience, 'all');
    assert.equal(view.matchedCount, 1);
});

test('展示顺序照后端分段顺序：before → 客户端 → after，客户端段在中间', () => {
    const view = m.promptRulePreviewView({
        matches: [],
        segments: [
            { rule_id: 3, name: '渠道', position: 'before', text: '渠道前' },
            { rule_id: 1, name: '全局', position: 'before', text: '全局前' },
            { rule_id: 0, name: 'client', position: 'client', text: '客户端自带' },
            { rule_id: 1, name: '全局', position: 'after', text: '全局后' },
        ],
        text: '渠道前\n全局前\n客户端自带\n全局后',
    });
    assert.deepEqual(view.segments.map((seg) => seg.rule_id), [3, 1, 0, 1]);
    assert.equal(view.segments[2].position, 'client');
    assert.equal(view.text, '渠道前\n全局前\n客户端自带\n全局后');
});

test('没填客户端示例就不往正文塞占位文字：正文只用后端 text，提示只挂在客户端段上', () => {
    const view = m.promptRulePreviewView({
        matches: [],
        segments: [
            { rule_id: 1, name: '品牌身份', position: 'before', text: '品牌前' },
            { rule_id: 0, name: 'client', position: 'client', text: '' },
        ],
        text: '品牌前',
    });
    assert.equal(view.text.includes(m.PROMPT_RULE_CLIENT_EMPTY_HINT), false, '提示语不能进正文');
    assert.equal(view.text, '品牌前');
    assert.equal(m.promptRuleClientSegmentEmpty(view.segments[1]), true);
    assert.equal(m.promptRuleClientSegmentEmpty(view.segments[0]), false);
});

test('未命中原因文案：按当前模拟条件说大白话', () => {
    assert.equal(m.promptRuleReasonText('disabled', scopeOf()), '已关闭，什么都不加');
    assert.equal(m.promptRuleReasonText('audience', scopeOf(0, '', 10, false)), '只对管理员 Key 生效');
    assert.equal(m.promptRuleReasonText('audience', scopeOf(0, '', 10, true)), '只对普通用户生效');
    assert.equal(m.promptRuleReasonText('plan_scope', scopeOf(0)), '只管有方案的 Key');
    assert.equal(m.promptRuleReasonText('channel_scope', scopeOf(0, '', 0)), '没有候选渠道可用');
    assert.equal(m.promptRuleReasonText('model_scope', scopeOf(0, 'gpt-4o')), '模型不在它的范围里');
});

// ── 一句话总结 ──

test('一句话：按作用域换成大白话', () => {
    const names = new Map([[7, 'VIP']]);
    const channelNames = new Map([[10, 'gemini_claude']]);
    assert.match(
        m.promptRuleOneLineSummary(rule({ plan_mode: 'selected', plan_ids: [7], include_no_plan: true, position: 'before' }), names, channelNames),
        /VIP 方案、没选方案的 Key 的请求，不管走哪个渠道，都在系统提示词的前面加这段/,
    );
    assert.match(
        m.promptRuleOneLineSummary(rule({ channel_mode: 'include', channel_ids: [10], model_mode: 'selected', models: ['claude-*'], audience: 'admin_only', position: 'after' }), names, channelNames),
        /管理员 Key 的请求.*claude-\*.*渠道.*后面/,
    );
});

// ── 草稿 ──

test('草稿：改动合进 map、保存受理保留更新的字、删除清掉条目', () => {
    const saved = { ...rule({}, 5), created_at: 'x', updated_at: 'y' };
    let map = m.editRuleDraft({}, '5', saved, (value) => ({ ...value, name: '改名' }));
    assert.equal(map['5'].value.name, '改名');
    assert.equal(m.dirtyRuleDraftKeys(map).length, 1);
    // 保存之外又打了字：草稿保留
    map = m.editRuleDraft(map, '5', saved, (value) => ({ ...value, text: '新内容' }));
    map = m.acceptRuleDraftSave(map, '5', { ...saved, name: '改名', text: '新内容' });
    assert.equal(map['5'].value.text, '新内容');
    assert.deepEqual(m.dirtyRuleDraftKeys(map), []);
    map = m.dropRuleDraft(map, '5');
    assert.ok(!map['5']);
});

test('sort 不算用户改动：顺序被别人调过后，草稿不会被误判成未保存', () => {
    const saved = m.promptRuleFromSaved(rule({}, 5));
    const map = m.editRuleDraft({}, '5', saved, (value) => ({ ...value, sort: 99 }));
    assert.deepEqual(m.dirtyRuleDraftKeys(map), []);
    assert.equal(m.promptRuleValuesEqual({ ...saved, sort: 3 }, { ...saved, sort: 42 }), true);
});

test('payload：名字与文案 trim、models 按小写去空去重、sort 用服务端最新值', () => {
    const payload = m.promptRuleDraftPayload({
        ...m.promptRuleFromSaved(rule({}, 3)),
        id: undefined,
        name: ' 名字 ',
        text: ' 文案\n\n',
        models: [' a ', 'A', 'b', ''],
        sort: 7,
    }, 0);
    assert.equal(payload.id, undefined);
    assert.equal(payload.name, '名字');
    assert.equal(payload.text, '文案');
    assert.deepEqual(payload.models, ['a', 'b']);
    assert.equal(payload.sort, 0, 'sortOverride 优先，避免把调顺序前的旧值写回去');
    assert.equal(m.promptRuleDraftPayload(m.promptRuleFromSaved(rule({}, 3))).sort, 0);
});

// ── preview 请求体契约：字段名与后端 json tag 一致（防止再错位） ──
test('preview 请求体字段名与后端 json tag 一致：request_model 改名发 model', () => {
    // internal/server/handlers/prompt_rule.go 的 previewPromptRules 结构体 tag：
    // plan_id / model / channel_id / is_admin / client_system。
    const req = api.promptRulePreviewRequest({ plan_id: 7, request_model: 'gpt', channel_id: 10, is_admin: false }, '客户端示例');
    assert.ok(typeof api.promptRulePreviewRequest === 'function', 'prompt-rule.ts 要提供 promptRulePreviewRequest');
    assert.deepEqual(Object.keys(req).sort(), ['channel_id', 'client_system', 'is_admin', 'model', 'plan_id']);
    assert.equal(req.model, 'gpt');
    assert.equal(req.plan_id, 7);
    assert.equal(req.channel_id, 10);
    assert.equal(req.is_admin, false);
    assert.equal(req.client_system, '客户端示例');
    // 没填客户端示例就发空串：后端不会把任何占位文字当用户原文。
    assert.equal(api.promptRulePreviewRequest({ plan_id: 0, request_model: '', channel_id: 0, is_admin: false }).client_system, '');
});

// ── 校验 ──

test('保存阻断：没名字/方案范围空/渠道范围空/模型没填', () => {
    assert.equal(m.promptRuleSaveProblem({ ...m.promptRuleFromSaved(rule()), name: ' ' }), '规则要有名字');
    assert.match(
        m.promptRuleSaveProblem(rule({ plan_mode: 'selected', plan_ids: [], include_no_plan: false })),
        /方案范围还没选/,
    );
    assert.match(
        m.promptRuleSaveProblem(rule({ channel_mode: 'include', channel_ids: [] })),
        /还没勾渠道/,
    );
    assert.match(
        m.promptRuleSaveProblem(rule({ model_mode: 'selected', models: [' '] })),
        /指定模型还没填/,
    );
    assert.equal(m.promptRuleSaveProblem(m.promptRuleFromSaved(rule({ plan_mode: 'selected', plan_ids: [7] }))), null);
});

test('保存阻断与后端同款：pattern 重复/星号位置/内容字节上限', () => {
    const withModels = (models) => rule({ model_mode: 'selected', models });
    assert.match(m.promptRuleSaveProblem(withModels(['Claude-*', 'claude-*'])), /写重复了/);
    assert.match(m.promptRuleSaveProblem(withModels(['claude-sonnet-4', 'CLAUDE-SONNET-4'])), /写重复了/);
    assert.match(m.promptRuleSaveProblem(withModels(['claude-*suffix'])), /星.*放在最后/);
    assert.match(m.promptRuleSaveProblem(withModels(['*claude'])), /星.*放在最后/);
    assert.equal(m.promptRuleSaveProblem(withModels(['*'])), null, '裸星允许，代表全部模型');
    assert.equal(m.promptRuleSaveProblem(withModels(['claude-*', 'claude-sonnet-4'])), null, '尾星和精确名是两个空间');
    assert.match(m.promptRuleSaveProblem(rule({ text: 'x'.repeat(m.PROMPT_RULE_TEXT_MAX_BYTES + 1) })), /内容太长/);
    assert.equal(m.promptRuleSaveProblem(rule({ text: 'x'.repeat(m.PROMPT_RULE_TEXT_MAX_BYTES) })), null);
});

// ── 模型输入框原文 / 复制命名 ──

test('模型输入框保留正在敲的尾逗号/尾换行，外部值真变了才覆盖', () => {
    // 尾分隔符不改变解析结果 → 保留用户原文，不吞逗号/换行
    assert.equal(m.promptRuleModelsTextSync('a,', 'a'), null);
    assert.equal(m.promptRuleModelsTextSync('a\n', 'a'), null);
    assert.equal(m.promptRuleModelsTextSync('a, b\n', 'a\nb'), null);
    assert.equal(m.promptRuleModelsTextSync('', ''), null);
    // 外部值真的多/少了 pattern → 输入框跟上外部值
    assert.equal(m.promptRuleModelsTextSync('a,', 'a\nb'), 'a\nb');
    assert.equal(m.promptRuleModelsTextSync('a\nb', 'a'), 'a');
});

test('复制命名按码点截到 191-3 再拼「 副本」，不劈开 emoji', () => {
    assert.equal(m.promptRuleCopyName('品牌身份'), '品牌身份 副本');
    assert.equal(m.promptRuleCopyName('  品牌身份  '), '品牌身份 副本');
    const long = m.promptRuleCopyName('规'.repeat(300));
    assert.equal([...long].length, 191);
    assert.ok(long.endsWith(' 副本'));
    assert.equal([...long].slice(0, 188).join(''), '规'.repeat(188));
    // emoji 是代理对：按码点截不能留下半个字符
    const emoji = m.promptRuleCopyName('😀'.repeat(200));
    assert.equal([...emoji].length, 191);
    assert.equal([...emoji].slice(0, 188).join(''), '😀'.repeat(188));
    assert.ok(!/[\uD800-\uDBFF]$/.test([...emoji].slice(0, 188).join('')), '不能以孤立高代理结尾');
});
