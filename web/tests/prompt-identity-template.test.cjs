const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const { buildIdentityPreset, appendIdentityPreset } = load('src/components/modules/prompt/prompt-identity.ts');

test('品牌模板只是助手显示名称，不冒充底层模型', () => {
    const text = buildIdentityPreset({ mode: 'brand', displayName: 'Octopus 助手 · 专业版', style: '准确、简洁' });
    assert.match(text, /Octopus 助手 · 专业版/);
    assert.match(text, /不代表底层模型/);
    assert.match(text, /准确、简洁/);
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

test('UI插入仍走已有每层保存按钮且只在管理员视图', () => {
    const source = fs.readFileSync(path.join(__dirname, '../src/components/modules/prompt/index.tsx'), 'utf8');
    const builder = fs.readFileSync(path.join(__dirname, '../src/components/modules/prompt/PromptIdentityBuilder.tsx'), 'utf8');
    assert.match(source, /role === 'admin' \? <AdminPromptWorkspace \/> : <UserPromptView \/>/);
    assert.match(source, /<PromptIdentityBuilder/);
    assert.match(source, /appendIdentityPreset\(current\.text, identityPreset\)/);
    assert.match(source, /onSave=\{save\}|onClick=\{save\}/);
    assert.match(builder, /中性模式不会删除客户端原有 system\/developer/);
});
