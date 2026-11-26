const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const { buildIdentityPreset, appendIdentityPreset, upsertIdentityPreset } = load('src/components/modules/prompt/prompt-identity.ts');

test('品牌模板只是助手显示名称，不冒充底层模型', () => {
    const text = buildIdentityPreset({ mode: 'brand', displayName: 'Octopus 助手 · 专业版', style: '准确、简洁' });
    assert.match(text, /Octopus 助手 · 专业版/);
    assert.match(text, /不代表底层模型/);
    assert.match(text, /准确、简洁/);
    assert.match(text, /用户询问“你是谁”/);
    assert.match(text, /不主动将底层模型名称当作品牌名称/);
    assert.match(text, /明确询问底层模型或提供商.*如实说明/);
});

test('中性模板既不带品牌名，也不删除原提示词', () => {
    const neutral = buildIdentityPreset({ mode: 'neutral', displayName: 'Octopus 助手', style: '清晰' });
    assert.doesNotMatch(neutral, /Octopus/);
    assert.match(neutral, /通用助手/);
    assert.equal(appendIdentityPreset('原有 system 指令', neutral), `原有 system 指令\n\n${neutral}`);
});

test('品牌名空白时禁止生成；禁止换行混入额外指令', () => {
    assert.equal(buildIdentityPreset({ mode: 'brand', displayName: '   ', style: '' }), '');
    const text = buildIdentityPreset({ mode: 'brand', displayName: '  Octopus\n  助手 ', style: '专业\r\n稳健' });
    assert.match(text, /Octopus 助手/);
    assert.match(text, /专业 稳健/);
});

test('插入纯文本：不覆盖旧规则、不重复、不自动开启任何开关', () => {
    const preset = '展示名称为 Octopus 助手';
    assert.equal(appendIdentityPreset('旧规则\n', preset), '旧规则\n\n展示名称为 Octopus 助手');
    assert.equal(appendIdentityPreset('旧规则\n\n展示名称为 Octopus 助手', preset), '旧规则\n\n展示名称为 Octopus 助手');
    assert.equal(appendIdentityPreset('旧规则', ''), '旧规则');
});

test('快捷品牌草稿可重复替换自己标记的身份段，不动原有工具规则', () => {
    const first = upsertIdentityPreset('原有工具规则', buildIdentityPreset({ mode: 'brand', displayName: 'Brand A', style: '专业' }));
    const again = upsertIdentityPreset(first, buildIdentityPreset({ mode: 'brand', displayName: 'Brand B', style: '简洁' }));
    assert.match(again, /原有工具规则/);
    assert.doesNotMatch(again, /Brand A/);
    assert.match(again, /Brand B/);
    assert.equal((again.match(/【Octopus 助手身份预设开始】/g) ?? []).length, 1);
    assert.equal(upsertIdentityPreset(again, ''), again);
});

test('UI插入仍走已有每层保存按钮且只在管理员视图', () => {
    const source = fs.readFileSync(path.join(__dirname, '../src/components/modules/prompt/index.tsx'), 'utf8');
    const editor = fs.readFileSync(path.join(__dirname, '../src/components/modules/prompt/rule-editor.tsx'), 'utf8');
    const builder = fs.readFileSync(path.join(__dirname, '../src/components/modules/prompt/PromptIdentityBuilder.tsx'), 'utf8');
    // 只在管理员视图：普通用户看不到插入入口。
    assert.match(source, /role === 'admin' \? <AdminPromptWorkspace \/> : <UserPromptView \/>/);
    assert.match(source, /<PromptIdentityBuilder/);
    // 模板只填进当前规则草稿；入口在规则编辑器里，插完必须手动点「保存」。
    assert.match(editor, /appendIdentityPreset\(current\.text, identityPreset\)/);
    assert.match(source, /identityPreset=\{identityPreset\}/);
    // 保存走规则接口：新建规则 / 更新已有规则各一条真实接线（不再有旧四层的 onSave）。
    assert.match(source, /create\.mutateAsync\(promptRuleDraftPayload\(draft\.value\)\)|create\.mutateAsync\(payload\)/);
    assert.match(source, /update\.mutateAsync\(\{ \.\.\.payload, id: payload\.id \}\)/);
    assert.match(editor, /onClick=\{onSave\}/);
    // builder 自己的两条承诺不变：中性模式不删原有 system/developer，且要显式「填入草稿」。
    assert.match(builder, /中性模式不会删除原有 system\/developer/);
    assert.match(builder, /onApplyGlobal/);
    // 一键模板落到规则草稿时替换自己标记的身份段，不重复累加。
    assert.match(source, /upsertIdentityPreset\(/);
});
