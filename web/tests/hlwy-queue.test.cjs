'use strict';

// Run from web: node --test tests/hlwy-queue.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');
const {
    comparisonFixture,
    createRuntime,
    countsFixture,
    findAll,
    findElement,
    makeJob,
    makeResult,
    reportFixture,
    squash,
    text,
    translate,
    widgets,
} = require('./hlwy-harness.cjs');

/**
 * 检测记录页（Queue）对 hlwy 对照任务的 kind 感知。
 *
 * 这一页原本只认识多探针：结论是风险档位、失败判据是 verdict=unknown。对照
 * 任务两样都不成立，直接套用会把一次跑满 100 个样本的对照显示成「没测成」。
 */

function hlwyJob(over = {}) {
    return makeJob({
        id: 1,
        channel_name: '渠道A',
        kind: 'hlwy_compare',
        status: 'done',
        total: 1,
        finished: 1,
        single_model: 'gpt-test',
        // 后端给对照结果写的结论就是 unknown（它没有风险档位）。
        single_verdict: 'unknown',
        single_status: 'done',
        hlwy_summary: {
            execution: 'complete',
            sufficiency: 'scorable',
            comparability: 'matched',
            counts: countsFixture(),
        },
        ...over,
    });
}

function probeJob(over = {}) {
    return makeJob({
        id: 4,
        channel_name: '渠道D',
        kind: 'audit',
        single_model: 'm-d',
        single_verdict: 'unknown',
        single_status: 'failed',
        ...over,
    });
}

function mount(jobs, detail = { results: [] }) {
    const runtime = createRuntime();
    const opened = [];
    const mocks = {
        react: runtime.react,
        'next-intl': { useTranslations: () => translate },
        'lucide-react': widgets(
            'CheckCircle2 ChevronDown ChevronRight CircleDashed HelpCircle Hash Loader2 Square XCircle'
        ),
        '@/components/ui/button': widgets('Button'),
        '@/lib/utils': { cn: (...args) => args.filter(Boolean).join(' ') },
        '@/lib/model-audit-verdict': {
            historyBucket: () => 'none',
            verdictHeadline: () => '旧结论',
            formatReportTime: () => '',
        },
        '@/api/endpoints/model-audit': {
            useAuditJobs: () => ({ data: jobs, isLoading: false }),
            useAuditJobDetail: () => ({ data: { job: jobs[0], ...detail }, isLoading: false }),
            useCancelAuditJob: () => ({ mutate: () => {}, isPending: false, variables: undefined }),
        },
    };
    const { ModelAuditQueue } = load('src/components/modules/model-audit/Queue.tsx', mocks);
    const props = { onOpenReport: (result) => opened.push(result.id) };
    return { runtime, props, opened, render: () => runtime.render(ModelAuditQueue, props) };
}

/** 点开某一行的结果列表。 */
function openRow(tree, channelName) {
    findElement(tree, (node) => node.type === 'button' && text(node).includes(channelName)).props.onClick();
}

test('a finished comparison is neither 可疑 nor 没测成', () => {
    const { render } = mount([hlwyJob(), probeJob()]);
    const body = squash(render());
    assert.match(body, /可疑 0/, 'a comparison makes no verdict, so it cannot be suspect');
    assert.match(body, /没测成 1/, 'only the genuinely failed multi-probe run counts');
});

test('a canceled or timed-out comparison is not counted as failed either', () => {
    const { render } = mount([
        hlwyJob({ hlwy_summary: { execution: 'canceled', sufficiency: 'insufficient', comparability: 'matched' } }),
        hlwyJob({ id: 2, hlwy_summary: { execution: 'timed_out', sufficiency: 'insufficient', comparability: 'matched' } }),
        hlwyJob({ id: 3, hlwy_summary: { execution: 'failed', sufficiency: 'insufficient', comparability: 'unknown' } }),
    ]);
    const body = squash(render());
    assert.match(body, /没测成 1/, 'only execution=failed is 没测成');
});

test('the row is labelled as a comparison, with the sample ledger instead of a verdict', () => {
    const { render } = mount([hlwyJob()]);
    const body = squash(render());
    assert.match(body, /基准对照/);
    // 调用数是上限，不是「一定有 100 个有效样本」。
    assert.match(body, /gpt-test · 对照完成 · 最多 100 次调用 · 已发起 100 · 有效 100/);
    assert.equal(body.includes('开槽'), false);
});

test('the progress bar counts settled slots, and says 正在收尾 while one is in flight', () => {
    const job = hlwyJob({
        status: 'running',
        finished: 0,
        hlwy_summary: {
            execution: 'sampling',
            sufficiency: 'insufficient',
            counts: countsFixture({ started_slots: 100, valid: 99, unresolved: 1 }),
        },
    });
    const { render } = mount([job]);
    const tree = render();
    // 99 个落定、1 个还没回：进度是 99%，不是已经把 100 个都发起过的 100%。
    const bars = findAll(tree, (node) => node.props && node.props.style && typeof node.props.style.width === 'string');
    assert.deepEqual(bars.map((node) => node.props.style.width), ['99%']);
    assert.match(squash(tree), /正在收尾（已发起 100，1 个样本还没回）/);
});

test('a canceled run whose last sample is still in flight says so', () => {
    const { render } = mount([hlwyJob({ status: 'canceled' })], {
        results: [makeResult({ status: 'running', report: '' })],
    });
    let tree = render();
    openRow(tree, '渠道A');
    tree = render();
    assert.match(squash(tree), /任务已停，这一次在收尾/);
});

test('skipped and failed comparison rows use their own wording', () => {
    const { render, opened } = mount([hlwyJob({ status: 'canceled' })], {
        results: [
            makeResult({ id: 21, status: 'skipped' }),
            makeResult({ id: 22, status: 'failed', report: '{"kind":"hlwy_compare"}', error_msg: '上游 502' }),
        ],
    });
    let tree = render();
    openRow(tree, '渠道A');
    tree = render();
    const body = squash(tree);
    assert.match(body, /任务已停，未开始/);
    assert.match(body, /失败/);
    assert.equal(body.includes('没跑成'), false, 'a failed comparison is called what it is');
    assert.match(body, /上游 502/, 'the server message is repeated verbatim');
    assert.match(body, /看已采到的部分/, 'a failed capture still keeps its partial report reachable');

    findElement(tree, (node) => node.type === 'button' && text(node) === '看已采到的部分').props.onClick();
    assert.deepEqual(opened, [22]);
});

test('a skipped row that already spent calls says it kept them', () => {
    // 取消时已经发出去的那一次也标成 skipped：它花过钱、报告里有部分结果，
    // 笼统写「未开始」等于把那笔钱和那份结果都抹掉。
    const kept = JSON.stringify(
        reportFixture({
            baseline_comparison: comparisonFixture({
                counts: countsFixture({ started_slots: 40, valid: 39, unresolved: 1 }),
            }),
        })
    );
    const { render } = mount([hlwyJob({ status: 'canceled' })], {
        results: [
            makeResult({ id: 31, status: 'skipped', report: kept }),
            makeResult({ id: 32, status: 'skipped' }),
        ],
    });
    let tree = render();
    openRow(tree, '渠道A');
    tree = render();
    const body = squash(tree);
    assert.match(body, /已取消，保留已采结果/);
    assert.match(body, /任务已停，未开始/, 'the row that truly never started keeps the old wording');
});

test('a completed comparison row summarises the report it stored', () => {
    const report = reportFixture();
    const { render } = mount([hlwyJob()], {
        results: [makeResult({ status: 'done', verdict: 'unknown', report: JSON.stringify(report) })],
    });
    let tree = render();
    openRow(tree, '渠道A');
    tree = render();
    assert.match(squash(tree), /对照完成/);

    // 行按钮里也有模型名，所以排除掉带渠道名的那个，否则点的是「收起」。
    const expand = findElement(
        tree,
        (node) => node.type === 'button' && text(node).includes('gpt-test') && !text(node).includes('渠道A')
    );
    expand.props.onClick();
    tree = render();
    const body = squash(tree);
    assert.match(body, /与主对照的数字分布相似度 86\.50%/);
    assert.match(body, /采样账目：最多 100 次调用 · 有效 100 · 未确认 0/);
    assert.match(body, /两边采样条件一致/);
    assert.match(body, /queue\.openReport/);
});

test('an unreadable comparison report is reported, not dressed up', () => {
    const { render } = mount([hlwyJob()], {
        results: [makeResult({ status: 'done', verdict: 'unknown', report: '{not json' })],
    });
    let tree = render();
    openRow(tree, '渠道A');
    tree = render();
    assert.match(squash(tree), /对照完成（细节读不出）/);

    findElement(
        tree,
        (node) => node.type === 'button' && text(node).includes('gpt-test') && !text(node).includes('渠道A')
    ).props.onClick();
    assert.match(squash(render()), /这份报告解析失败/);
});

test('legacy multi-probe rows keep their old rendering', () => {
    const report = JSON.stringify({
        plain: { headline: '上游是别家', meaning: '说明了什么', caveat: '不能证明什么', next_step: '下一步' },
    });
    const { render } = mount([probeJob()], { results: [makeResult({ status: 'done', verdict: 'high', report })] });
    let tree = render();
    openRow(tree, '渠道D');
    tree = render();
    assert.match(squash(tree), /上游是别家/);
    assert.equal(squash(tree).includes('基准对照'), false, 'a plain audit job is not badged as a comparison');
});
