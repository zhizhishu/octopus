'use strict';

// Run from web/: node --test tests/hlwy-model.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');
const {
    comparisonFixture,
    countsFixture,
    dist355,
    makeJob,
    reportFixture,
    rawNumbers,
    baselineEntryFixture,
    snapshotFixture,
} = require('./hlwy-harness.cjs');

/**
 * hlwy 纯读模型（model-audit/hlwy.ts）的契约测试。
 *
 * 这一层是「报告 → 界面」之间唯一的翻译层，所有「能不能显示分数」「缺字段时
 * 说什么」的判断都在这里，所以盒子在外面测：界面只负责把这里算出来的东西画出来。
 */

const hlwy = load('src/components/modules/model-audit/hlwy.ts');

/* ------------------------------------------------------------------ *
 * 解析：旧报告不受影响，坏报告不编数字
 * ------------------------------------------------------------------ */

test('legacy probe report stays on the old path', () => {
    // 多探针报告没有 baseline_comparison：必须返回 null，让页面继续走旧的展示，
    // 不能因为读不出对照就把一份正常报告显示成空的。
    assert.equal(hlwy.readHlwyComparison({ verdict: 'none', results: [] }), null);
    assert.equal(hlwy.readHlwyComparison(null), null);
    assert.equal(hlwy.readHlwyComparison(undefined), null);
    assert.equal(hlwy.readHlwyComparison({}), null);
});

test('a hlwy report without a comparison block is openable but says so', () => {
    // 只有 kind、没有 baseline_comparison：能打开（它不是坏文件），但什么都不许编。
    const view = hlwy.readHlwyComparison({ kind: 'hlwy_compare' });
    assert.ok(view, 'kind alone identifies the report');
    assert.equal(view.counts, null, 'no counts object is invented');
    assert.equal(view.hasScore, false);
    assert.match(view.problem, /没有对照细节/);
});

test('missing counts never turn into zeros', () => {
    const view = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ counts: undefined }) })
    );
    assert.equal(view.counts, null);
    assert.equal(view.hasScore, false, 'cannot score without a sample ledger');
    assert.match(view.problem, /采样计数缺失/);
});

test('a damaged count field drops the whole ledger instead of half of it', () => {
    const view = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ counts: countsFixture({ valid: '100' }) }) })
    );
    assert.equal(view.counts, null, 'one wrong type invalidates the ledger, it is not silently patched');
});

/* ------------------------------------------------------------------ *
 * 分数：只在报告自己说可评分时出现
 * ------------------------------------------------------------------ */

test('scorable report shows a score', () => {
    const view = hlwy.readHlwyComparison(reportFixture());
    assert.equal(view.hasScore, true);
    assert.equal(view.chosen.percent, '86.50%', '0..1 raw value is rendered as a percent');
});

test('unknown comparability does not suppress the score', () => {
    // 「采样条件没记录全」是一条警告，不是「不能比」。把它当成不能比，
    // 会把一份算得出来的分布对比直接藏掉。
    const view = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ comparability: 'unknown' }) })
    );
    assert.equal(view.hasScore, true);
    assert.match(hlwy.hlwyComparabilityLabel(view.comparability), /没有完整记录/);
});

test('incompatible and insufficient never show a percent', () => {
    const incompatible = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ comparability: 'incompatible' }) })
    );
    assert.equal(incompatible.hasScore, false);
    assert.equal(incompatible.sufficiency, 'scorable', 'sufficiency is separate from comparability');

    const insufficient = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ sufficiency: 'insufficient' }) })
    );
    assert.equal(insufficient.hasScore, false);
});

test('an explicit scorable=false wins over the enum', () => {
    const view = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ scorable: false }) })
    );
    assert.equal(view.scorable, false);
    assert.equal(view.hasScore, false, 'the report said it is not scorable; we do not overrule it');
});

test('canceled / timed_out / interrupted are marked partial', () => {
    for (const execution of ['canceled', 'timed_out', 'interrupted']) {
        const view = hlwy.readHlwyComparison(
            reportFixture({ baseline_comparison: comparisonFixture({ execution }) })
        );
        assert.equal(view.partial, true, execution);
    }
    assert.equal(hlwy.readHlwyComparison(reportFixture()).partial, false);
});

/* ------------------------------------------------------------------ *
 * 分箱
 * ------------------------------------------------------------------ */

test('36 bins cover 1..355 and add up to the whole distribution', () => {
    const shares = hlwy.hlwyBinShares(dist355());
    assert.equal(shares.length, hlwy.HLWY_BIN_COUNT);
    const total = shares.reduce((sum, v) => sum + v, 0);
    assert.ok(Math.abs(total - 1) < 1e-9, `bins must sum to 1, got ${total}`);
    // 第一段吃下 dist[0..9]（数字 1..10），最后一段吃下 dist[350..354]（数字 351..355）。
    assert.ok(Math.abs(shares[0] - 0.6) < 1e-12, `first bin is digits 1..10, got ${shares[0]}`);
    assert.ok(Math.abs(shares[35] - 0.4) < 1e-12, `last bin is digits 351..355, got ${shares[35]}`);
    assert.deepEqual(hlwy.hlwyBinRange(0), { from: 1, to: 10, label: '1–10' });
    assert.deepEqual(hlwy.hlwyBinRange(35), { from: 351, to: 355, label: '351–355' });
});

test('a damaged distribution falls back to the raw numbers instead of drawing garbage', () => {
    // 数组长度对但内容坏了：整条分布丟掉，改用原始数字重算（原始数字本身仍然有效），
    // 而不是拿着一个带字符串的数组去画图。
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({ distribution: new Array(355).fill('x') }),
        })
    );
    assert.equal(view.distribution.length, 355);
    const sum = view.distribution.reduce((a, b) => a + b, 0);
    assert.ok(Math.abs(sum - 1) < 1e-9, `distribution is rebuilt from raw numbers, got ${sum}`);
    const shares = hlwy.hlwyBinShares(view.distribution);
    assert.equal(shares.length, hlwy.HLWY_BIN_COUNT);
    assert.equal(shares[35], 0, 'the fixture only sampled digits 1..100');
});

test('a wrong-length distribution yields no bins at all', () => {
    assert.deepEqual(hlwy.hlwyBinShares(null), []);
    assert.deepEqual(hlwy.hlwyBinShares([1, 2, 3]), [], 'length is 355 or nothing — no truncation, no padding');
});

/* ------------------------------------------------------------------ *
 * 基准快照 = numberdist.Baseline 的完整形状
 * ------------------------------------------------------------------ */

test('a baseline snapshot is read as a full Baseline: source.kind, raw numbers, counts', () => {
    const base = snapshotFixture();
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({
                baseline_snapshots: [base],
                matches: [{ baseline_id: base.id, rank: 1, overall: 0.8 }],
                chosen_baseline_id: base.id,
            }),
        })
    );
    const chosen = view.chosen;
    assert.equal(chosen.label, '社区基准 A');
    assert.equal(chosen.sourceKind, 'builtin_hlwy', 'the source lives at source.kind');
    assert.match(chosen.sourceText, /hlwy 社区基准/);
    assert.equal(chosen.validCount, 200, 'valid count is len(raw_numbers), not a stored derived copy');
    assert.equal(chosen.collectedAt, '2026-01-02T03:04:05Z');
    assert.equal(view.snapshots[0].payloadSha256.length, 64);
});

test('the baseline series comes from the counts when raw numbers are gone', () => {
    const base = snapshotFixture();
    delete base.raw_numbers;
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({
                baseline_snapshots: [base],
                matches: [{ baseline_id: base.id, rank: 1, overall: 0.8 }],
                chosen_baseline_id: base.id,
            }),
        })
    );
    const baseline = view.bins.map((bin) => bin.baseline);
    assert.equal(baseline.every((share) => typeof share === 'number'), true, 'counts still give a distribution');
    const total = baseline.reduce((sum, share) => sum + share, 0);
    assert.ok(Math.abs(total - 1) < 1e-9, `baseline shares must sum to 1, got ${total}`);
});

test('an abbreviated snapshot does not get silently reshaped into a Baseline', () => {
    // 后端曾回过一份「只有 id/label/valid_count」的缩水快照：那不是能离线重算的证据。
    const abbreviated = {
        id: 'hlwy:base@aaa111',
        label: '社区基准 A',
        source_kind: 'builtin_hlwy',
        valid_count: 200,
        distribution: dist355(),
        mode: 43,
    };
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({
                baseline_snapshots: [abbreviated],
                matches: [{ baseline_id: abbreviated.id, rank: 1, overall: 0.8 }],
                chosen_baseline_id: abbreviated.id,
            }),
        })
    );
    const chosen = view.chosen;
    assert.equal(chosen.sourceKind, '', 'no flat source_kind fallback');
    assert.match(chosen.sourceText, /来源未标注/);
    assert.equal(chosen.validCount, null, 'no fabricated sample count');
    assert.equal(
        view.bins.every((bin) => bin.baseline === null),
        true,
        'no distribution means no baseline series, and the UI shows a dash instead'
    );
});

test('an empty counts array is not turned into a zero distribution', () => {
    const base = snapshotFixture({ raw_numbers: [], counts: new Array(355).fill(0) });
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({
                baseline_snapshots: [base],
                matches: [{ baseline_id: base.id, rank: 1, overall: 0.8 }],
                chosen_baseline_id: base.id,
            }),
        })
    );
    assert.equal(view.snapshots[0].distribution, null);
    assert.equal(view.snapshots[0].validCount, 0, 'an empty baseline is 0 samples, which is a fact, not a guess');
});

/* ------------------------------------------------------------------ *
 * 打开历史结果
 * ------------------------------------------------------------------ */

test('stored report reader accepts hlwy reports that have no results array', () => {
    const raw = reportFixture();
    delete raw.results;
    const read = hlwy.readStoredReport(JSON.stringify(raw));
    assert.equal(read.ok, true);
    assert.equal(read.hlwy, true);
    assert.deepEqual(read.view.results, [], 'results is filled in so the old page shell still works');
});

test('stored report reader keeps rejecting broken files, with a reason', () => {
    assert.deepEqual(hlwy.readStoredReport(''), { ok: false, error: '这份报告是空的' });
    assert.deepEqual(hlwy.readStoredReport('   '), { ok: false, error: '这份报告是空的' });
    assert.equal(hlwy.readStoredReport('{oops').ok, false);
    assert.match(hlwy.readStoredReport('{oops').error, /解析失败/);
    // 旧报告仍然要求 results：这是「能不能展示多探针结论」的门槛。
    const legacy = hlwy.readStoredReport(JSON.stringify({ verdict: 'none' }));
    assert.equal(legacy.ok, false);
    assert.match(legacy.error, /不完整/);
    const legacyOk = hlwy.readStoredReport(JSON.stringify({ verdict: 'none', results: [] }));
    assert.equal(legacyOk.ok, true);
    assert.equal(legacyOk.hlwy, false);
});

/* ------------------------------------------------------------------ *
 * 「开始」按钮的资格：没有计划就没有价格承诺
 * ------------------------------------------------------------------ */

function startState(over = {}) {
    return hlwy.hlwyStartState({
        hasTarget: true,
        baselineId: 'hlwy:base@aaa111',
        flagEnabled: true,
        plan: {
            enabled: true,
            max_calls: 200,
            protocol_id: 'hlwy-number355-v1',
            sample_timeout_seconds: 60,
            timeout_seconds: 1800,
        },
        planError: '',
        planLoading: false,
        pending: false,
        ...over,
    });
}

test('start is enabled only when every condition holds', () => {
    assert.deepEqual(startState(), { disabled: false, reason: '' });
});

test('a failed plan disables start and says why, in order', () => {
    assert.match(startState({ planError: 'boom', plan: null }).reason, /^计划没读出来：boom/);
    assert.equal(startState({ planError: 'boom', plan: null }).disabled, true);
    // 还在读（且没有旧计划）时说「正在读」，不是「读不到」。
    assert.match(startState({ plan: null, planLoading: true }).reason, /正在读取/);
    // 读完了但是没有计划：不给提交。
    const none = startState({ plan: null, planLoading: false });
    assert.equal(none.disabled, true);
    assert.match(none.reason, /计划没读出来/);
    // 计划在、但服务端关了这个能力。
    assert.match(startState({ plan: { enabled: false, max_calls: 200 } }).reason, /没有启用/);
    assert.match(startState({ plan: { enabled: true, max_calls: 0 } }).reason, /调用额度/);
});

test('a selected baseline that turned unusable blocks start with a reason', () => {
    const blocked = startState({ baselineUnavailable: true });
    assert.equal(blocked.disabled, true);
    assert.match(blocked.reason, /选中的基准现在不能用/);
    // 其它条件都满足时，未选到不可用件的状况照旧可提交。
    assert.equal(startState({ baselineUnavailable: false }).disabled, false);
});

test('a rerun whose channel disappeared is blocked with a reason, not swapped quietly', () => {
    // 渠道被删掉/停用以后，选择器会退到别的渠道上：按钮必须拦住，并说清是哪一条。
    const blocked = startState({ channelMissing: '上一轮用的渠道（#9）已经不在渠道列表里了，请重新选一个渠道再提交' });
    assert.equal(blocked.disabled, true);
    assert.match(blocked.reason, /上一轮用的渠道（#9）已经不在渠道列表里了/);
    // 这条比笼统的「先选渠道和模型」具体，所以排在它前面说。
    assert.equal(blocked.reason, '上一轮用的渠道（#9）已经不在渠道列表里了，请重新选一个渠道再提交');
    // 渠道还在的时候不受影响。
    assert.equal(startState({ channelMissing: '' }).disabled, false);
});

test('missing baseline and closed flag are both explicit reasons', () => {
    assert.match(startState({ baselineId: '' }).reason, /先选一份基准/);
    const off = startState({ flagEnabled: false });
    assert.equal(off.disabled, true);
    assert.match(off.reason, /设置里关闭/);
});

/* ------------------------------------------------------------------ *
 * 「存成我的基准」的资格
 * ------------------------------------------------------------------ */

const doneOpts = { resultId: 11, resultStatus: 'done' };

test('a complete, fully accounted capture may be saved as a baseline', () => {
    const view = hlwy.readHlwyComparison(reportFixture());
    assert.deepEqual(hlwy.hlwySaveEligibility(view, doneOpts), { eligible: true, reason: '' });
});

test('saving never asks for complete condition metadata', () => {
    // 采样条件很多时候就是「不知道」：不知道也要能存，存下来的仍是「不知道」，
    // 不能为了让 completeness 好看就编一个 temperature=0。
    const view = hlwy.readHlwyComparison(
        reportFixture({
            baseline_comparison: comparisonFixture({
                conditions: {
                    protocol: 'hlwy-number355-v1',
                    temperature_state: 'unknown',
                    top_p_state: 'unknown',
                    completeness: 'partial',
                    unknown_fields: ['temperature', 'top_p'],
                },
            }),
        })
    );
    assert.deepEqual(hlwy.hlwySaveEligibility(view, doneOpts), { eligible: true, reason: '' });
    // 条件里的「未知」原样留着，不被补成默认值。
    assert.equal(view.conditions.temperature_state, 'unknown');
    const rows = hlwy.hlwyConditionRows(view.conditions);
    assert.equal(rows.find((row) => row.label === 'temperature').value, '未知');
});

test('save eligibility refuses every half-finished case', () => {
    const view = (over) => hlwy.readHlwyComparison(reportFixture({ baseline_comparison: comparisonFixture(over) }));

    assert.match(hlwy.hlwySaveEligibility(null, doneOpts).reason, /没有可保存/);
    assert.match(hlwy.hlwySaveEligibility(view({}), { resultStatus: 'done' }).reason, /检测记录/);
    assert.match(hlwy.hlwySaveEligibility(view({}), { resultId: 11, resultStatus: 'failed' }).reason, /正常跑完/);
    assert.match(hlwy.hlwySaveEligibility(view({ execution: 'canceled' }), doneOpts).reason, /没有完整跑完/);
    assert.match(
        hlwy.hlwySaveEligibility(view({ counts: countsFixture({ unresolved: 3 }) }), doneOpts).reason,
        /3 个采样槽位没确认/
    );
    assert.match(
        hlwy.hlwySaveEligibility(view({ counts: countsFixture({ started_slots: 90 }) }), doneOpts).reason,
        /没有全部走完/
    );
    assert.match(
        hlwy.hlwySaveEligibility(view({ counts: countsFixture({ valid: 40 }) }), doneOpts).reason,
        /少于 100 个/
    );
    assert.match(hlwy.hlwySaveEligibility(view({ raw_numbers: undefined }), doneOpts).reason, /没有原始数字/);
    assert.match(
        hlwy.hlwySaveEligibility(view({ raw_numbers: rawNumbers(90) }), doneOpts).reason,
        /对不上/
    );
});

/* ------------------------------------------------------------------ *
 * 导出
 * ------------------------------------------------------------------ */

test('full export is a whitelist: channel plumbing never leaves the browser', () => {
    // 报告每一层都可能被塞进渠道信息。这里在三个层级都埋一遍，导出的文本里
    // 一个都不许出现——「先 spread 再删键」漏一个就泄一次渠道地址。
    const report = reportFixture({
        endpoint: 'https://secret-upstream.example/v1/chat',
        url: 'https://secret-upstream.example/v1/chat',
        headers: { authorization: 'Bearer sk-secret' },
        note: '内部备注：走的是人肉代充',
        api_key: 'sk-secret',
    });
    report.baseline_comparison.conditions.endpoint = 'https://secret-upstream.example';
    report.baseline_comparison.note = '内部备注';
    report.baseline_comparison.baseline_snapshots[0].endpoint = 'https://secret-upstream.example';
    report.baseline_comparison.matches[0].note = '内部备注';

    const payload = hlwy.hlwyExportFull(report);
    const dumped = JSON.stringify(payload);
    for (const leak of ['secret-upstream', 'sk-secret', '内部备注', 'authorization']) {
        assert.equal(dumped.includes(leak), false, `export must not contain ${leak}`);
    }

    // 该在的还在：计数、原始数字、分布、快照、匹配。
    assert.equal(payload.kind, 'hlwy_compare');
    assert.deepEqual(payload.results, [], 'empty skeleton arrays are kept');
    assert.equal(payload.baseline_comparison.counts.valid, 100);
    assert.equal(payload.baseline_comparison.raw_numbers.length, 100);
    assert.equal(payload.baseline_comparison.distribution.length, 355);
    assert.equal(payload.baseline_comparison.baseline_snapshots.length, 1);
    assert.equal(payload.baseline_comparison.matches[0].baseline_id, 'hlwy:base@aaa111');

    // 快照按完整 Baseline 导出：离线重算要用的数字都在，来源只留 kind。
    const snap = payload.baseline_comparison.baseline_snapshots[0];
    assert.deepEqual(snap.source, { kind: 'builtin_hlwy' }, 'only the source kind travels, not import prose');
    assert.equal(snap.raw_numbers.length, 200);
    assert.equal(snap.counts.length, 355);
    assert.equal(snap.schema_version, 1);
    assert.equal(Object.hasOwn(snap, 'source_kind'), false, 'the abbreviated flat copy must not reappear');
    assert.equal(Object.hasOwn(snap, 'valid_count'), false);
    assert.equal(payload.score, 0);
    assert.equal(payload.verdict, 'unknown');
});

test('full export refuses a report without a comparison block', () => {
    assert.equal(hlwy.hlwyExportFull({ verdict: 'none', results: [] }), null);
    assert.equal(hlwy.hlwyExportFull(null), null);
});

test('legacy export keeps the 8 fields and the original statistics semantics', () => {
    const view = hlwy.readHlwyComparison(reportFixture());
    const legacy = hlwy.hlwyExportLegacy(view, { model: 'gpt-test', timestamp: '2026-01-02T03:04:05Z' });
    assert.deepEqual(Object.keys(legacy).sort(), [
        'apiType',
        'distribution',
        'iterations',
        'model',
        'name',
        'rawData',
        'stats',
        'timestamp',
    ]);
    assert.equal(legacy.iterations, legacy.rawData.length, 'iterations is the raw sample count');
    assert.equal(legacy.rawData.length, 100);
    assert.equal(legacy.distribution.length, 355);
    const sum = legacy.distribution.reduce((a, b) => a + b, 0);
    assert.ok(Math.abs(sum - 1) < 1e-9, 'distribution is recomputed from raw numbers');
    // stats.mode 是数字本身，不是出现次数；次数在 modeCount。
    assert.equal(legacy.stats.mode, 43);
    assert.equal(legacy.stats.modeCount, 2);
    assert.equal(legacy.stats.median, 43, 'integer median from the report, not re-interpolated');
    assert.deepEqual(legacy.rawData, view.rawNumbers, 'raw numbers are passed through untouched');
});

test('legacy export refuses when there are no raw numbers', () => {
    const view = hlwy.readHlwyComparison(
        reportFixture({ baseline_comparison: comparisonFixture({ raw_numbers: undefined }) })
    );
    assert.equal(hlwy.hlwyExportLegacy(view, { model: 'gpt-test' }), null);
    assert.equal(hlwy.hlwyExportLegacy(null, {}), null);
});

/* ------------------------------------------------------------------ *
 * 队列 / 历史记录
 * ------------------------------------------------------------------ */

test('a finished comparison is complete, not failed, even though its verdict is unknown', () => {
    // 后端给对照结果写的是 verdict=unknown（它没有风险档位）。旧的「没测成」
    // 判据正是 unknown —— 直接套过来，一次跑满 100 个样本的对照会显示成没测成。
    const job = makeJob({
        kind: 'hlwy_compare',
        status: 'done',
        single_verdict: 'unknown',
        single_status: 'done',
        hlwy_summary: {
            execution: 'complete',
            sufficiency: 'scorable',
            comparability: 'matched',
            counts: countsFixture(),
        },
    });
    assert.equal(hlwy.isHlwyJob(job), true);
    assert.equal(hlwy.hlwyJobBucket(job), 'complete');
    assert.notEqual(hlwy.hlwyJobBucket(job), 'failed');
    assert.equal(hlwy.hlwyJobHeadline(job), '对照完成');
    // 调用数是上限：写「最多 N 次调用」，不把它说成「一定有 100 个有效样本」。
    assert.equal(hlwy.hlwyJobLedger(job), '最多 100 次调用 · 已发起 100 · 有效 100');
});

test('every terminal state has its own bucket', () => {
    const bucket = (execution, sufficiency = 'scorable', status = 'done') =>
        hlwy.hlwyJobBucket(
            makeJob({
                kind: 'hlwy_compare',
                status,
                hlwy_summary: { execution, sufficiency, comparability: 'matched', counts: countsFixture() },
            })
        );
    assert.equal(bucket('complete'), 'complete');
    assert.equal(bucket('complete', 'insufficient'), 'insufficient');
    assert.equal(bucket('canceled'), 'stopped');
    assert.equal(bucket('timed_out'), 'stopped');
    assert.equal(bucket('interrupted'), 'stopped');
    assert.equal(bucket('failed'), 'failed');
    assert.equal(bucket('sampling', 'scorable', 'running'), 'running');
    assert.match(hlwy.hlwyJobHeadline(makeJob({ kind: 'hlwy_compare', hlwy_summary: { execution: 'canceled' } })), /已停止/);
});

test('missing summary fabricates nothing', () => {
    const job = makeJob({ kind: 'hlwy_compare', status: 'running' });
    assert.equal(hlwy.hlwyJobLedger(job), '', 'no counts, no numbers');
    assert.equal(hlwy.hlwyJobBucket(job), 'running');
    // 摘要缺失但任务表说 done：仍旧算完成，不猜成失败。
    assert.equal(hlwy.hlwyJobBucket(makeJob({ kind: 'hlwy_compare', status: 'done' })), 'complete');
});

test('non-hlwy jobs are not treated as comparisons', () => {
    assert.equal(hlwy.isHlwyJob(makeJob({ kind: 'audit' })), false);
    assert.equal(hlwy.isHlwyJob(makeJob({})), false);
    assert.equal(hlwy.isHlwyJob(null), false);
});

/* ------------------------------------------------------------------ *
 * 文案：中性，不判真假
 * ------------------------------------------------------------------ */

test('labels stay neutral and honest about unknown values', () => {
    assert.equal(hlwy.hlwyExecutionLabel('canceled'), '已停止，保留已有结果');
    assert.equal(hlwy.hlwyExecutionLabel('nonsense'), '状态未知');
    assert.equal(hlwy.hlwyComparabilityLabel('incompatible'), '协议或题目不兼容，不能放在一起比');
    const rows = hlwy.hlwyConditionRows(null);
    assert.equal(rows.length, 1);
    assert.match(rows[0].value, /未知/);
    const typed = hlwy.hlwyConditionRows({ temperature_state: 'absent', top_p_state: 'value', stream: true });
    const byLabel = Object.fromEntries(typed.map((row) => [row.label, row.value]));
    assert.equal(byLabel.temperature, '未下发（观测到省略）');
    assert.equal(byLabel.top_p, '未知', 'state=value without a value is unknown, not 0');
    assert.equal(byLabel.stream, '是');
    assert.equal(byLabel['接口协议'], '未知');
    assert.equal(byLabel['题目协议'], '未知');
    assert.equal(hlwy.hlwySourceLabel('local_capture'), '本地自采');
    assert.match(hlwy.hlwySourceLabel('weird'), /来源未识别/);
});

test('baseline option labels contain only the human name', () => {
    assert.equal(hlwy.hlwyBaselineOptionLabel(baselineEntryFixture()), '社区基准 A');
    assert.equal(hlwy.hlwyBaselineOptionLabel(baselineEntryFixture({ collected_at: null })), '社区基准 A');
    assert.equal(hlwy.hlwyBaselineOptionLabel(baselineEntryFixture({ unavailable: true, reason_code: 'hash_mismatch' })), '社区基准 A');
});

test('report display ignores historic backend summaries and internal identifiers', () => {
    const view = hlwy.readHlwyComparison(reportFixture({
        plain: { headline: 'old internal headline', summary: 'hlwy-builtin:id@hash' },
    }));
    assert.match(view.headline, /数字习惯对照/);
    assert.match(view.summary, /收到100个有效回答/);
    assert.match(view.summary, /社区基准 A/);
    assert.doesNotMatch(view.headline + view.summary, /internal|hlwy-builtin|@hash/);
    const missing = hlwy.readHlwyComparison(reportFixture({
        baseline_comparison: comparisonFixture({ baseline_snapshots: [], counts: undefined }),
    }));
    assert.match(missing.summary, /有效回答数量未知/);
    assert.doesNotMatch(missing.summary, /hlwy:|@/);
});

test('stop reasons show readable explanations without leaking internal codes', () => {
    assert.equal(hlwy.hlwyStopReasonLabel(''), '');
    assert.equal(hlwy.hlwyStopReasonLabel('run_timeout'), '采样时间已到，提前结束');
    assert.match(hlwy.hlwyStopReasonLabel('http_401'), /认证失败/);
    assert.match(hlwy.hlwyStopReasonLabel('http_418'), /请求未被接受/);
    assert.equal(hlwy.hlwyStopReasonLabel('new_internal_code'), '采样未能继续，具体原因未记录');
});

test('an unusable catalog entry explains itself in human words', () => {
    assert.equal(hlwy.hlwyEntryUnavailableText(baselineEntryFixture({ unavailable: true, reason_code: 'hash_mismatch' })), '内容和哈希对不上，可能被改过');
    assert.equal(hlwy.hlwyEntryUnavailableText(baselineEntryFixture({ unavailable: true, reason_code: 'counts_mismatch' })), '计数和原始数字对不上');
    // 不认识的原因码带上原码，方便对照后端日志，但不把机器串当成整句解释。
    assert.match(
        hlwy.hlwyEntryUnavailableText(baselineEntryFixture({ unavailable: true, reason_code: 'weird_code' })),
        /服务端校验没通过（weird_code）/
    );
});

/* ------------------------------------------------------------------ *
 * 原因码、条件对照、柱子高度：都是纯函数，单独钉住
 * ------------------------------------------------------------------ */

test('reason codes become human sentences, unrecognised ones stay honest', () => {
    const known = hlwy.hlwyReasonMessages(['protocol_mismatch', 'temperature_differs', 'conditions_incomplete']);
    assert.deepEqual(
        known.map((row) => row.severity),
        ['incompatible', 'differs', 'unknown']
    );
    assert.equal(known[0].text, '两边题目协议不同，分布不能放在一起比');
    assert.equal(known[1].text, '两边 temperature 不同');

    const unknown = hlwy.hlwyReasonMessages(['brand_new_code']);
    assert.equal(unknown.length, 1);
    assert.doesNotMatch(unknown[0].text, /brand_new_code/, 'a raw enum is not an explanation');
    assert.match(unknown[0].text, /本地还没有这条原因的说明/);
    // 码本身留着，用来对照服务端记录。
    assert.equal(unknown[0].code, 'brand_new_code');
    assert.deepEqual(hlwy.hlwyReasonMessages([]), []);
});

test('conditions are compared one by one against the baseline', () => {
    const rows = hlwy.hlwyConditionCompareRows(
        { protocol: 'anthropic-messages', parser_version: 'v1', temperature_state: 'value', temperature: 1 },
        { protocol: 'anthropic-messages', parser_version: 'v2', temperature_state: 'value', temperature: 0 }
    );
    const byLabel = Object.fromEntries(rows.map((row) => [row.label, row]));
    assert.equal(byLabel['接口协议'].state, 'same');
    assert.equal(byLabel['解析版本'].state, 'differs');
    assert.equal(byLabel.temperature.state, 'differs');
    assert.equal(byLabel.temperature.run, '1');
    assert.equal(byLabel.temperature.baseline, '0');
    // 两边都没记录 → 未知；一边没记录 → 也不能写成「一致」。
    assert.equal(byLabel['题目协议'].state, 'unknown');
    const oneSided = Object.fromEntries(
        hlwy.hlwyConditionCompareRows(
            { transport_profile: 'direct' },
            { transport_profile: '' }
        ).map((row) => [row.label, row])
    );
    assert.equal(oneSided['传输形态'].state, 'unknown');
    // 基准没有条件记录时不做对照，由界面直说。
    assert.deepEqual(hlwy.hlwyConditionCompareRows({ protocol: 'x' }, null), []);
    assert.deepEqual(hlwy.hlwyConditionCompareRows(null, { protocol: 'x' }), []);
});

test('bar heights never invent a positive bar for a zero or unknown share', () => {
    assert.equal(hlwy.hlwyBarHeightPx(0, 0.5), 0, 'a zero share is zero pixels');
    assert.equal(hlwy.hlwyBarHeightPx(0.25, 0.5), 40);
    assert.equal(hlwy.hlwyBarHeightPx(0.0001, 0.5), 1, 'a tiny but real share stays visible');
    assert.equal(hlwy.hlwyBarHeightPx(0.5, 0), 0, 'no scale, no bars');
    // 刻度按数字 1–355 均分给位置。
    assert.deepEqual(
        hlwy.HLWY_AXIS_TICKS.map((tick) => tick.digit),
        [1, 100, 200, 355]
    );
    assert.equal(hlwy.HLWY_AXIS_TICKS[0].pct, 0);
    assert.equal(hlwy.HLWY_AXIS_TICKS[3].pct, 100);
});

test('a failed comparison is called 失败, not 没跑成', () => {
    const job = makeJob({
        kind: 'hlwy_compare',
        status: 'done',
        hlwy_summary: { execution: 'failed', sufficiency: 'insufficient', counts: countsFixture({ valid: 0 }) },
    });
    assert.equal(hlwy.hlwyJobHeadline(job), '失败');
    assert.equal(hlwy.hlwyExecutionLabel('failed'), '失败');
});

test('progress counts settled slots, never 100% while a sample is still in flight', () => {
    const flushing = makeJob({
        kind: 'hlwy_compare',
        status: 'running',
        hlwy_summary: { execution: 'sampling', counts: countsFixture({ started_slots: 100, unresolved: 1 }) },
    });
    // 分子是落定槽位（100 - 1），不是已发起：否则进度条顶在 100% 却没有结果。
    assert.equal(hlwy.hlwyProgressPercent(flushing), 99);
    assert.equal(hlwy.hlwyInFlightNote(flushing), '正在收尾（已发起 100，1 个样本还没回）');

    const settled = makeJob({
        kind: 'hlwy_compare',
        hlwy_summary: { execution: 'complete', counts: countsFixture() },
    });
    assert.equal(hlwy.hlwyProgressPercent(settled), 100);
    assert.equal(hlwy.hlwyInFlightNote(settled), '', 'nothing in flight, nothing to say');

    // 摘要缺失或计划为 0：没有分母就不给百分数，不编一个 0%。
    assert.equal(hlwy.hlwyProgressPercent(makeJob({ hlwy_summary: {} })), null);
    assert.equal(hlwy.hlwyProgressPercent(makeJob({ hlwy_summary: { counts: countsFixture({ planned_calls: 0 }) } })), null);
    // 已发起 1 个、1 个没回：落定 0，进度就是 0，不是 100。
    assert.equal(
        hlwy.hlwyProgressPercent(makeJob({ hlwy_summary: { counts: countsFixture({ started_slots: 1, unresolved: 1 }) } })),
        0
    );
});

test('a skipped result only claims 未开始 when nothing was ever started', () => {
    const partial = JSON.stringify(
        reportFixture({
            baseline_comparison: comparisonFixture({
                counts: countsFixture({ started_slots: 40, valid: 39, unresolved: 1 }),
            }),
        })
    );
    // 取消时已经发出去的那一次会被标成 skipped，但它花过钱、也有部分结果。
    assert.equal(hlwy.hlwySkippedText(partial), '已取消，保留已采结果');

    const untouched = JSON.stringify(
        reportFixture({
            baseline_comparison: comparisonFixture({
                counts: countsFixture({ started_slots: 0, valid: 0, unresolved: 0 }),
            }),
        })
    );
    assert.equal(hlwy.hlwySkippedText(untouched), '任务已停，未开始');
    // 报告读不出来时不下结论说「跑过了」，按未开始说。
    assert.equal(hlwy.hlwySkippedText(''), '任务已停，未开始');
    assert.equal(hlwy.hlwySkippedText('{not json'), '任务已停，未开始');
});
