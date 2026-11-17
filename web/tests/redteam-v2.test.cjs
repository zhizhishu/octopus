const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, '..', 'src');
const ui = fs.readFileSync(path.join(root, 'components/modules/prompt/RedTeamV2.tsx'), 'utf8');
const host = fs.readFileSync(path.join(root, 'components/modules/prompt/RedTeamLab.tsx'), 'utf8');
const api = fs.readFileSync(path.join(root, 'api/endpoints/redteam-lab-v2.ts'), 'utf8');

test('V2 is a separate tabbed workspace without replacing the legacy single-case lab', () => {
    assert.match(host, /<RedTeamV2 enabled=\{saved\.enabled\}/);
    assert.match(host, /旧版单题实验与兼容配置/);
    assert.match(host, /<details open className/);
    assert.match(host, /快速单题测试/);
    assert.match(host, /高级案例库与多轮评估（可选）/);
    assert.ok(host.indexOf('快速单题测试') < host.indexOf('高级案例库与多轮评估（可选）'), 'quick experiment must appear before advanced V2');
    assert.match(ui, /01 案例库/);
    assert.match(ui, /02 新建评估/);
    assert.match(ui, /03 任务与结果/);
});

test('V2 execution is explicit and billed only after confirmation with an idempotency key', () => {
    assert.match(ui, /costConfirmed/);
    assert.match(ui, /confirm_cost: true/);
    assert.match(ui, /idempotency_key: key/);
    assert.match(ui, /crypto\.randomUUID/);
    assert.match(ui, /setIdempotencyKey\(''\);/);
    assert.match(ui, /setSelectedID\(null\)/);
    assert.match(ui, /discardCheck/);
    assert.match(ui, /!enabled \|\| dirtyConfig/);
    assert.match(ui, /取消后续请求/);
});

test('V2 case contract has immutable revisions and license-confirmed import', () => {
    assert.match(api, /octopus\.redteam\.case\.v1/);
    assert.match(api, /revision: number/);
    assert.match(api, /confirmed_license: true/);
    assert.match(ui, /licenseConfirmed/);
    assert.match(ui, /版本不可变/);
    assert.match(ui, /JSON\.stringify\(\[draft\]/);
    assert.match(api, /useReviewRedTeamV2Run/);
    assert.match(api, /useRedTeamV2Revisions/);
    assert.match(ui, /历史版本对照/);
    assert.match(ui, /人工复核/);
});

test('V2 remains separate from the identity-detection UI and its API', () => {
    assert.doesNotMatch(api, /model-audit|model_audit|modelverify/);
    assert.doesNotMatch(ui, /model-audit|model_audit/);
    assert.match(ui, /评估记录不进入模型真伪报告/);
});
