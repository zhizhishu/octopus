const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const project = path.join(__dirname, '../src');
const lab = fs.readFileSync(path.join(project, 'components/modules/prompt/RedTeamLab.tsx'), 'utf8');
const workspace = fs.readFileSync(path.join(project, 'components/modules/prompt/index.tsx'), 'utf8');
const endpoint = fs.readFileSync(path.join(project, 'api/endpoints/redteam-lab.ts'), 'utf8');

test('红队面板有独立管理页签，老提示词草稿切换时保留挂载', () => {
    assert.match(workspace, /function AdminPromptWorkspace/);
    // 三个页签：规则 / 合并预览 / 破甲实验。
    assert.match(workspace, /\{ value: 'redteam', label: '破甲实验' \}/);
    assert.match(workspace, /<RedTeamLab \/>/);
    // 三个面板常驻挂载，只用 hidden 切换：切走不卸载，破甲实验里的状态和提示词草稿都不丢。
    assert.match(workspace, /hidden=\{tab !== 'rules'\}/);
    assert.match(workspace, /hidden=\{tab !== 'preview'\}/);
    assert.match(workspace, /hidden=\{tab !== 'redteam'\}/);
    assert.doesNotMatch(workspace, /\{tab === 'redteam'\s*\?/, '破甲实验不能被条件渲染成「切走就卸载」');
    assert.doesNotMatch(workspace, /tab !== 'redteam' &&/, '同上：不要用 && 短路卸载');
    assert.doesNotMatch(workspace, /高级实验接口（预留）/);
});

test('存在真正的设置保存、单次运行、结果与历史 API', () => {
    for (const verb of ['useRedTeamConfig', 'useSaveRedTeamConfig', 'useRunRedTeamCase', 'useRedTeamHistory']) {
        assert.match(lab, new RegExp(verb));
    }
    assert.match(endpoint, /apiClient\.put<RedTeamConfig>/);
    assert.match(endpoint, /apiClient\.post<RedTeamRun>/);
    assert.match(endpoint, /apiClient\.get<RedTeamRun\[\]>/);
    assert.match(lab, /保存专用设定/);
    assert.match(lab, /执行这一个案例/);
    assert.match(lab, /最近 25 次测试记录/);
    assert.match(lab, /模型可见回答/);
});

test('发送需要启用、保存、选择目标并勾选付费确认', () => {
    assert.match(lab, /disabled=\{!form\.enabled \|\| dirty/);
    assert.match(lab, /\!confirmed/);
    assert.match(lab, /confirm_cost: true/);
    assert.match(lab, /setConfirmed\(false\)/);
    assert.match(lab, /maxLength=\{3000\}/);
    assert.match(lab, /maxLength=\{160\}/);
    assert.match(lab, /最多保存 8 个测试案例/);
});

test('类型4永久退役，系统决策和嵌入渠道不出现在可运行列表', () => {
    assert.match(lab, /!\[4, 5, 7\]\.includes\(item\.type\)/);
    assert.match(lab, /channel\.selected_models/);
    assert.match(lab, /channel\.model\.split/);
    assert.match(lab, /\.map\(\(item\) => item\.raw\)/);
});

test('明确区分字面匹配与真实安全判定', () => {
    assert.match(lab, /标记出现不等于证明模型被破甲/);
    assert.match(lab, /无法评分/);
    assert.match(lab, /4000|4,000/);
    assert.match(lab, /按渠道 shape/);
    assert.doesNotMatch(endpoint, /model-audit\/run/);
});
