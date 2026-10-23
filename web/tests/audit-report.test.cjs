const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');

const { load } = require('./load-typescript.cjs');

const { parseAuditReport } = load('src/components/modules/log/audit-report.ts');

// `model_audit_report` 是 Go 侧 behavior.ViewReport 的 JSON，前端照着读。
// 两侧字段名对不上时前端**不会报错**：JSON.parse 成功，每个字段都是 undefined，
// 面板静默变空白。没有编译期信号，也没有运行期信号。
//
// 所以这里做两件事：
//   1. 对着 Go 源码的 struct tag 逐字核对前端类型声明（防"改了 Go 忘了前端"）
//   2. 钉住解析的边界行为（空/坏/数组 都不能炸整个弹窗）

const GO_VIEW = path.resolve(__dirname, '../../internal/modelverify/behavior/view.go');
const GO_SCORE = path.resolve(__dirname, '../../internal/modelverify/behavior/score.go');

/** 从 Go 源码里抠某个 struct 的全部 json tag。 */
function goJSONTags(source, structName) {
    const start = source.indexOf(`type ${structName} struct {`);
    assert.ok(start >= 0, `Go 源码里找不到 struct ${structName}`);
    const body = source.slice(start, source.indexOf('\n}', start));
    return [...body.matchAll(/json:"([a-zA-Z0-9_]+)[,"]/g)].map((m) => m[1]);
}

test('前端报告的字段名与 Go 侧 ViewReport 的 json tag 逐字一致', () => {
    const src = readFileSync(GO_VIEW, 'utf8');
    const goTags = goJSONTags(src, 'ViewReport');

    // 前端真正消费的字段（渲染时会读的）。results/findings 的元素结构另有断言。
    const consumed = [
        'requested_model',
        'score',
        'verdict',
        'findings',
        'errors',
        'disclaimer',
        'plain',
        'family',
    ];

    for (const field of consumed) {
        assert.ok(
            goTags.includes(field),
            `Go ViewReport 没有 json tag "${field}"，但前端在读它 —— ` +
                `字段一旦改名，前端不会报错，面板会静默变空。Go 侧现有 tag: ${goTags.join(', ')}`
        );
    }
});

test('plain 的四段人话文案字段名前后端一致', () => {
    const src = readFileSync(path.resolve(__dirname, '../../internal/modelverify/behavior/plain.go'), 'utf8');
    const goTags = goJSONTags(src, 'PlainVerdict');

    for (const field of ['level', 'headline', 'meaning', 'caveat', 'next_step']) {
        assert.ok(
            goTags.includes(field),
            `Go PlainVerdict 没有 json tag "${field}"。` +
                `其中 caveat 尤其不能丢 —— 它是"这不能说明什么"的诚实边界，` +
                `前端藏掉它就会把检测结论读成验真证书。现有 tag: ${goTags.join(', ')}`
        );
    }
});

test('findings 的元素字段名与 Go ViewFinding 一致', () => {
    const src = readFileSync(GO_VIEW, 'utf8');
    const goTags = goJSONTags(src, 'ViewFinding');
    for (const field of ['probe', 'severity', 'score', 'title', 'evidence', 'recommendation']) {
        assert.ok(goTags.includes(field), `Go ViewFinding 没有 json tag "${field}"`);
    }
});

test('errors 的元素是 {probe,error}，不是 probe_id/message', () => {
    const src = readFileSync(GO_SCORE, 'utf8');
    const goTags = goJSONTags(src, 'ProbeError');
    assert.deepEqual(goTags, ['probe', 'error']);
});

test('results 的元素是 {probe_id,ok,data,error}，不是 probe/title', () => {
    // 注意：results 用的是 probe_id（ViewResult），而 findings/errors 用的是 probe。
    // 两边混用不会报错，只会让「原始观测」整块静默变成空白。
    const src = readFileSync(GO_VIEW, 'utf8');
    const goTags = goJSONTags(src, 'ViewResult');
    assert.deepEqual(goTags, ['probe_id', 'ok', 'data', 'error']);

    // 前端类型声明必须跟着这几个 tag 走，不能自己改名字。
    const ts = readFileSync(path.resolve(__dirname, '../src/components/modules/log/audit-report.ts'), 'utf8');
    const start = ts.indexOf('interface AuditReportResult {');
    assert.ok(start >= 0, '前端缺少 AuditReportResult 接口 —— results 会退回 unknown[]');
    const body = ts.slice(start, ts.indexOf('\n}', start));
    for (const field of goTags) {
        assert.match(body, new RegExp(`\\b${field}\\?:`), `前端 AuditReportResult 缺少字段 ${field}`);
    }
});

// ── 解析边界 ────────────────────────────────────────────────────────────────

test('空列与空串都视为"没检测过"，返回 null 而不是空壳报告', () => {
    assert.equal(parseAuditReport(undefined), null);
    assert.equal(parseAuditReport(null), null);
    assert.equal(parseAuditReport(''), null);
    assert.equal(parseAuditReport('   \n\t '), null);
});

test('坏 JSON 返回 null，绝不抛异常', () => {
    // 库里可能存着旧版本写下的或人工改坏的历史记录。为一条读不懂的旧记录
    // 让整个详情弹窗白屏，等于用脏数据惩罚正在查当前问题的用户。
    assert.equal(parseAuditReport('{'), null);
    assert.equal(parseAuditReport('not json at all'), null);
    assert.equal(parseAuditReport('{"a":'), null);
});

test('合法 JSON 但不是报告（数组 / null / 标量）也返回 null', () => {
    // 这些都能被 JSON.parse 吞下，放过去会在渲染时按对象取属性而炸。
    assert.equal(parseAuditReport('[]'), null);
    assert.equal(parseAuditReport('[1,2,3]'), null);
    assert.equal(parseAuditReport('null'), null);
    assert.equal(parseAuditReport('42'), null);
    assert.equal(parseAuditReport('"a string"'), null);
});

test('真实形态的报告能被解析出来，关键字段可读', () => {
    const raw = JSON.stringify({
        requested_model: 'gpt-6-astra',
        score: 42,
        verdict: 'high',
        findings: [
            {
                probe: 'token_arithmetic',
                severity: 'high',
                score: 30,
                title: 'token 对不上',
                evidence: { input: 10, output: 5, sum: 15, total: 99 },
                recommendation: '换渠道复核',
            },
        ],
        errors: [{ probe: 'ttft_histogram', error: '上游没返回首字时间' }],
        disclaimer: '检查结果提供参考，不等同模型真实性证明。',
        plain: {
            level: 'high',
            headline: '看着像换过模型',
            meaning: '这次回复的 token 总量对不上。',
            caveat: '这不能证明模型是假的 —— 上游可能只是做了截断。',
            next_step: '换一个渠道再测一次。',
        },
        family: 'openai',
    });

    const parsed = parseAuditReport(raw);
    assert.ok(parsed);
    assert.equal(parsed.verdict, 'high');
    assert.equal(parsed.score, 42);
    assert.equal(parsed.findings.length, 1);
    assert.equal(parsed.findings[0].title, 'token 对不上');
    assert.equal(parsed.errors[0].error, '上游没返回首字时间');
    // caveat 必须在 —— 它是最容易被"优化"掉、又最不能丢的一段。
    assert.match(parsed.plain.caveat, /不能证明/);
});

test('带 results 的报告能解析出来，data 里的键值可读', () => {
    const raw = JSON.stringify({
        requested_model: 'gpt-6-astra',
        verdict: 'low',
        findings: [],
        results: [
            { probe_id: 'token_arithmetic', ok: true, data: { input: 10, output: 5, total: 99 } },
            { probe_id: 'ttft_histogram', ok: false, error: '上游没返回首字时间' },
        ],
        errors: [],
    });

    const parsed = parseAuditReport(raw);
    assert.ok(parsed);
    assert.equal(parsed.results.length, 2);
    // probe_id（不是 probe）—— 与 findings 的元素不同名。
    assert.equal(parsed.results[0].probe_id, 'token_arithmetic');
    assert.equal(parsed.results[0].ok, true);
    // data 的每个键都要能读出来：用户查的就是「你到底测出了什么数」。
    assert.deepEqual(parsed.results[0].data, { input: 10, output: 5, total: 99 });
    assert.equal(parsed.results[1].ok, false);
    assert.equal(parsed.results[1].data, undefined);
    assert.equal(parsed.results[1].error, '上游没返回首字时间');
});

test('字段缺失的报告不抛异常（老版本数据）', () => {
    const parsed = parseAuditReport('{"verdict":"low"}');
    assert.ok(parsed);
    assert.equal(parsed.verdict, 'low');
    assert.equal(parsed.findings, undefined);
    assert.equal(parsed.plain, undefined);
});
