'use strict';

// Run from web: node --test tests/hlwy-card.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');
const {
    comparisonFixture,
    createRuntime,
    findAll,
    findElement,
    baselineEntryFixture,
    countsFixture,
    matchFixture,
    rawNumbers,
    reportFixture,
    snapshotFixture,
    squash,
    text,
    widgets,
} = require('./hlwy-harness.cjs');

/**
 * 报告页的「基准对照」块（BaselineComparisonCard）。
 *
 * 这里守的是三件事：不给分的时候真的一个字百分比都不给；来源/日期/样本量
 * 一直在页面上（360px 也不靠 hover）；换基准重算只是预览、能一键改回原报告。
 */

const hlwy = load('src/components/modules/model-audit/hlwy.ts');

const SNAPSHOTS = [
    snapshotFixture({ id: 'hlwy:base@aaa111', label: '社区基准 A' }),
    snapshotFixture({ id: 'hlwy:b@bbb222', label: '社区基准 B' }),
    snapshotFixture({ id: 'hlwy:c@ccc333', label: '社区基准 C' }),
    snapshotFixture({ id: 'hlwy:d@ddd444', label: '本地自采 D', source: { kind: 'local_capture' } }),
];

/** 换基准用的目录项（catalog Entry）：扁平 source_kind/valid_count，不带原始数组。 */
const CATALOG = [
    baselineEntryFixture({ id: 'hlwy:base@aaa111', label: '社区基准 A' }),
    baselineEntryFixture({ id: 'hlwy:b@bbb222', label: '社区基准 B' }),
    baselineEntryFixture({ id: 'hlwy:c@ccc333', label: '社区基准 C', source_kind: 'local_capture' }),
    baselineEntryFixture({ id: 'hlwy:e@eee555', label: '坏掉的本地件', source_kind: 'local_capture', unavailable: true, reason_code: 'hash_mismatch' }),
];

function matchesFixture() {
    return [
        matchFixture({ baseline_id: 'hlwy:base@aaa111', rank: 1, overall: 0.865 }),
        matchFixture({ baseline_id: 'hlwy:b@bbb222', rank: 2, overall: 0.7, gap_to_previous: 0.032 }),
        matchFixture({ baseline_id: 'hlwy:c@ccc333', rank: 3, overall: 0.5, gap_to_previous: 0.2 }),
        matchFixture({ baseline_id: 'hlwy:d@ddd444', rank: 4, overall: 0.4, gap_to_previous: 0.1 }),
    ];
}

function viewFixture(over = {}) {
    const comparison = comparisonFixture({
        baseline_snapshots: SNAPSHOTS,
        matches: matchesFixture(),
        chosen_baseline_id: SNAPSHOTS[0].id,
        ...over,
    });
    return hlwy.readHlwyComparison(reportFixture({ baseline_comparison: comparison }));
}

function mount(overrides = {}) {
    const runtime = createRuntime();
    const calls = { recompare: [], restore: 0, save: 0, rerun: 0, full: 0, legacy: 0 };
    const mocks = {
        react: runtime.react,
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/lib/utils': { cn: (...args) => args.filter(Boolean).join(' ') },
        '@/api/endpoints/model-audit': {},
    };
    const { BaselineComparisonCard } = load('src/components/modules/model-audit/BaselineComparisonCard.tsx', mocks);
    const props = {
        view: viewFixture(),
        recompareActive: false,
        catalog: CATALOG,
        onRecompare: (id) => calls.recompare.push(id),
        recomparePending: false,
        recompareError: '',
        onRestoreOriginal: () => {
            calls.restore += 1;
        },
        saveEligibility: { eligible: true, reason: '' },
        onSave: () => {
            calls.save += 1;
        },
        saving: false,
        onRerun: () => {
            calls.rerun += 1;
        },
        onExportFull: () => {
            calls.full += 1;
        },
        onExportLegacy: () => {
            calls.legacy += 1;
        },
        canExportLegacy: true,
        ...overrides,
    };
    return { runtime, props, calls, render: () => runtime.render(BaselineComparisonCard, props) };
}

test('a scorable comparison shows the chosen similarity and its provenance side by side', () => {
    const { render } = mount();
    const tree = render();
    const body = squash(tree);

    assert.match(body, /86\.50%/);
    assert.match(body, /相似度/);
    // 元数据与分数同一屏：不看 hover 也能判断这份参考够不够格。
    assert.match(body, /社区基准 A/);
    assert.match(body, /来源：hlwy 社区基准/);
    assert.match(body, /采集日期：2026-01-02/);
    assert.match(body, /参考样本：200 个/);
    assert.match(body, /收到100个有效回答/);
});

test('the sample ledger keeps every count separate', () => {
    const { render } = mount();
    const tree = render();
    const details = findElement(tree, (node) => node.type === 'details' && text(node).includes('计算详情'));
    const body = squash(details);
    assert.match(squash(tree), /收到100个有效回答/);
    // 调用数是上限，不是「一定会拿到 100 个有效样本」的承诺。
    assert.match(body, /最多 100 次调用/);
    assert.match(body, /已发起 100/);
    assert.match(body, /有效 100/);
    assert.match(body, /无效 0/);
    assert.match(body, /失败 0/);
    assert.match(body, /未确认 0/);
    assert.equal(body.includes('开槽'), false);
});

test('top three with gaps, then expand-all to the full candidate list', () => {
    const { render } = mount();
    let tree = render();
    assert.equal(findAll(tree, (node) => node.type === 'li').length, 3, 'default view is the top 3');
    assert.match(squash(tree), /与上一名差 3\.20 个百分点/);
    assert.match(squash(tree), /主对照/);

    findElement(tree, (node) => node.type === 'button' && text(node).includes('展开全部 4 份')).props.onClick();
    tree = render();
    assert.equal(findAll(tree, (node) => node.type === 'li').length, 4);
    assert.match(squash(tree), /全部候选（4 份）/);

    findElement(tree, (node) => node.type === 'button' && text(node).includes('只看前三')).props.onClick();
    assert.equal(findAll(render(), (node) => node.type === 'li').length, 3);
});

test('incompatible comparisons show neither a score nor a ranking', () => {
    const { render } = mount({
        view: viewFixture({ comparability: 'incompatible', reason_codes: ['protocol_mismatch'] }),
    });
    const tree = render();
    const body = squash(tree);

    assert.match(body, /两边协议或题目不兼容，这里不给相似度/);
    assert.equal(body.includes('与主对照的数字分布相似度'), false);
    assert.equal(body.includes('86.50%'), false);
    // 名次也是结论：没分的时候连名次一起拿掉，否则名次会被当成分数读。
    assert.equal(
        findAll(tree, (node) => node.type === 'li' && /^#\d/.test(squash(node))).length,
        0,
        'no candidate list at all'
    );
    assert.equal(body.includes('#1'), false);
    assert.equal(body.includes('与上一名差'), false);
    assert.equal(body.includes('最接近的 3 份参考'), false);
    assert.equal(body.includes('不给分'), false);
    // 基准本身的来源/日期/样本量照旧可见。
    assert.match(body, /社区基准 A/);
    assert.match(body, /参考样本：200 个/);
    // 为什么不可比，用人话说，不摆枚举串。
    assert.match(body, /两边题目协议不同，分布不能放在一起比/);
    assert.equal(body.includes('protocol_mismatch'), false);
});

test('a contradictory payload never shows a score or a ranking', () => {
    // 报告自称 scorable，计数却只有 39 个有效样本，而且还附带了一份排名。
    // 这种自相矛盾的输入必须落到「不给分不给名次」，不能挑好看的那一半信。
    const { render } = mount({
        view: viewFixture({
            sufficiency: 'insufficient',
            scorable: true,
            counts: countsFixture({ valid: 39, started_slots: 39 }),
            reason_codes: ['conditions_incomplete'],
        }),
    });
    const tree = render();
    const body = squash(tree);

    assert.match(body, /有效样本不足，不给相似度分数/);
    assert.equal(body.includes('86.50%'), false);
    assert.equal(body.includes('#1'), false);
    assert.equal(body.includes('与上一名差'), false);
    assert.equal(findAll(tree, (node) => node.type === 'li' && /^#\d/.test(squash(node))).length, 0);
    // 分布本身仍可描述：那是「采到了什么」，不是排名。
    assert.match(body, /看数字表（36 段，共 355 个数）/);
});

test('insufficient samples and scorable=false each have their own explanation', () => {
    const insufficient = mount({ view: viewFixture({ sufficiency: 'insufficient' }) });
    assert.match(squash(insufficient.render()), /有效样本不足，不给相似度分数/);
    assert.equal(squash(insufficient.render()).includes('86.50%'), false);
    assert.equal(squash(insufficient.render()).includes('#1'), false);

    const notScorable = mount({ view: viewFixture({ scorable: false }) });
    assert.match(squash(notScorable.render()), /报告判定这一轮不可评分/);
});

test('the score is never offered as a probability of being genuine', () => {
    const body = squash(mount().render());
    assert.match(body, /这是回答习惯的相似度，不是模型真伪概率/);
});

test('an unknown condition set still shows the score', () => {
    // 「采样条件不知道」是一条警告，不是不给分的理由。
    const body = squash(mount({ view: viewFixture({ comparability: 'unknown' }) }).render());
    assert.match(body, /86\.50%/);
    assert.match(body, /采样条件没有完整记录/);
    assert.match(body, /参考资料的采样设置不完整，结果仅供参考/);
});

test('a self-compare is labelled as not independent evidence', () => {
    const body = squash(mount({ view: viewFixture({ self_compare: true }) }).render());
    assert.match(body, /同一批样本自比，不构成独立证据/);
    assert.equal(body.includes('只说明采样稳定度'), false);
});

test('a partial run says so and keeps the counts', () => {
    const { render } = mount({ view: viewFixture({ execution: 'timed_out' }) });
    const body = squash(render());
    assert.match(body, /超时结束，保留已有结果/);
    assert.match(body, /这一轮提前结束了/);
    assert.match(body, /最多 100 次调用/);
});

test('a report with a missing ledger says the sample size is unknown', () => {
    const { render } = mount({ view: viewFixture({ counts: undefined }) });
    const body = squash(render());
    assert.match(body, /有效回答数量未知/);
    assert.match(body, /报告只能部分读取/);
});

test('the distribution shows 36 bins and a 36-row table', () => {
    const tree = mount().render();
    assert.match(squash(tree), /看数字表（36 段，共 355 个数）/);
    // 采样条件对照也有表格，所以只数「区间 + 本渠道 + 基准」那种行。
    const rows = findAll(tree, (node) => node.type === 'tr' && /^\d+–\d+/.test(squash(node)));
    assert.equal(rows.length, 36, 'one row per bin');
    assert.match(squash(rows[0]), /1–10/);
    assert.match(squash(rows[35]), /351–355/);
});

test('a zero bin draws no bar at all, a missing baseline series draws nothing', () => {
    // 数字只到 200：后 16 段两边的份额都是 0，不能画成 1px 的「有」。
    // 基准里连原始数字都没有：基准这一列干脆不画，不拿 0 冒充。
    const withZeroBins = mount({
        view: viewFixture({ distribution: undefined, raw_numbers: rawNumbers(200) }),
    });
    const tree = withZeroBins.render();
    const runBars = findAll(tree, (node) => node.props && node.props['data-series'] === 'run');
    const baseBars = findAll(tree, (node) => node.props && node.props['data-series'] === 'baseline');
    assert.equal(runBars.length, 36);
    assert.equal(baseBars.length, 36);
    assert.equal(runBars[0].props.style.height, '80px', 'the tallest bin fills the chart');
    assert.equal(runBars[25].props.style.height, '0px', 'an empty bin is zero pixels, not a fake 1px');
    assert.equal(baseBars[25].props.style.height, '0px');

    const noBaselineSeries = mount({
        view: viewFixture({
            distribution: undefined,
            raw_numbers: rawNumbers(200),
            baseline_snapshots: [snapshotFixture({ raw_numbers: [], counts: [] })],
        }),
    }).render();
    assert.equal(findAll(noBaselineSeries, (node) => node.props && node.props['data-series'] === 'baseline').length, 0);
    assert.match(squash(noBaselineSeries), /本渠道/);
    assert.equal(squash(noBaselineSeries).includes('主对照基准'), false);
});

test('tiny but nonzero bins stay visible at one pixel', () => {
    // 200 个样本里 199 个落在第 1 段、1 个落在第 2 段：第 2 段是真的有数据。
    const numbers = new Array(199).fill(1).concat([11]);
    const view = viewFixture({
        distribution: undefined,
        raw_numbers: numbers,
        baseline_snapshots: [snapshotFixture({ raw_numbers: numbers })],
    });
    const bars = findAll(mount({ view }).render(), (node) => node.props && node.props['data-series'] === 'run');
    assert.equal(bars[1].props.style.height, '1px');
});

test('the chart axis names the actual digit positions', () => {
    const tree = mount().render();
    const ticks = findAll(
        tree,
        (node) => node.type === 'span' && node.props && node.props.style && typeof node.props.style.left === 'string'
    ).map((node) => text(node));
    assert.deepEqual(ticks, ['1', '100', '200', '355']);
    assert.match(squash(tree), /横轴是数字 1–355 的位置/);
});

test('the score components, baseline fingerprint and reasons are inspectable', () => {
    const body = squash(mount().render());
    assert.match(body, /计算详情/);
    assert.match(body, /余弦相似0\.9000/);
    assert.match(body, /JS 距离（越小越像）0\.1200/);
    assert.match(body, /众数一致度0\.5000/);
    assert.ok(
        body.includes('a'.repeat(64)),
        'the baseline snapshot hash is printed in full so the numbers can be traced'
    );
});

test('reason codes are translated into human sentences', () => {
    const body = squash(
        mount({ view: viewFixture({ reason_codes: ['temperature_differs', 'transport_differs', 'made_up_code'] }) }).render()
    );
    assert.match(body, /两边 temperature 不同/);
    assert.match(body, /两边传输形态不同（裸客户端 \/ 中转链）/);
    // 认不出的码不猜意思，也不把枚举串当说明摆出来。
    assert.match(body, /本地还没有这条原因的说明，以服务端记录为准/);
    assert.equal(body.includes('made_up_code'), false);
    assert.equal(body.includes('temperature_differs'), false);
});

test('sampling conditions are compared against the chosen baseline row by row', () => {
    const tree = mount().render();
    const body = squash(tree);
    assert.match(body, /采样设置/);
    // 表头 + 每行三层：项目 / 本渠道 / 基准 / 判定。
    assert.match(body, /项目本渠道基准对照/);
    assert.match(body, /接口协议hlwy-number355-v1hlwy-number355-v1一致/);
    assert.match(body, /题目协议未知未知未知/);
});

test('a condition that differs is marked, and unknown stays unknown', () => {
    const body = squash(
        mount({
            view: viewFixture({
                conditions: {
                    ...comparisonFixture().conditions,
                    temperature_state: 'value',
                    temperature: 0.7,
                },
            }),
        }).render()
    );
    assert.match(body, /temperature0\.70不同/);
    assert.match(body, /传输形态未知未知未知/);
});

test('a baseline without conditions says so instead of faking a comparison', () => {
    const body = squash(
        mount({ view: viewFixture({ baseline_snapshots: [snapshotFixture({ conditions: null })] }) }).render()
    );
    assert.match(body, /基准快照里没有记采样条件，没法逐项对照/);
    // 本轮的条件照旧列出来。
    assert.match(body, /题目指纹/);
});

test('recompare is a preview with a way back', () => {
    const { render, calls } = mount({ recompareActive: true });
    const tree = render();
    const body = squash(tree);
    assert.match(body, /本地重算的预览，存下来的原报告没有改动/);

    findElement(tree, (node) => node.type === 'button' && text(node) === '改回原报告').props.onClick();
    assert.equal(calls.restore, 1);
});

test('the recompare action needs a pick before it fires', () => {
    const { render, calls } = mount();
    let tree = render();
    const button = () => findElement(tree, (node) => node.type === 'Button' && text(node) === '比较');
    assert.equal(button().props.disabled, true, 'no baseline picked yet');

    findElement(tree, (node) => node.type === 'SearchableSelect').props.onValueChange('hlwy:b@bbb222');
    tree = render();
    assert.equal(button().props.disabled, false);
    button().props.onClick();
    assert.deepEqual(calls.recompare, ['hlwy:b@bbb222']);
});

test('a corrupt catalog entry cannot be picked for recompare', () => {
    const { render } = mount();
    const select = findElement(render(), (node) => node.type === 'SearchableSelect');
    const options = select.props.options;
    assert.equal(
        options.find((option) => option.value === 'hlwy:e@eee555').disabled,
        true,
        'a corrupt entry would only fail on the server, so it is greyed out up front'
    );
    assert.equal(options.find((option) => option.value === 'hlwy:e@eee555').label, '坏掉的本地件');
    assert.equal(select.props.className, 'w-full min-w-0 max-w-full');
    assert.equal(options.find((option) => option.value === 'hlwy:b@bbb222').disabled, false);
});

test('a pick that turned out unusable blocks recompare with a reason', () => {
    // 选的时候还好好的，目录刷新后这份件被标成校验没过：这时重算也要拦住。
    const { render, calls } = mount();
    let tree = render();
    findElement(tree, (node) => node.type === 'SearchableSelect').props.onValueChange('hlwy:e@eee555');
    tree = render();
    const button = findElement(tree, (node) => node.type === 'Button' && text(node) === '比较');
    assert.equal(button.props.disabled, true);
    assert.match(
        squash(tree),
        /选中的「坏掉的本地件」现在不能用：内容和哈希对不上，可能被改过。换一份再重算。/
    );
    assert.deepEqual(calls.recompare, []);
});

test('recompare failures are printed verbatim', () => {
    const { render } = mount({ recompareError: '基准已被删除（404）' });
    assert.match(squash(render()), /重算失败：基准已被删除（404）/);
});

test('buttons that cannot work explain themselves instead of looking clickable', () => {
    const { render } = mount({
        canExportLegacy: false,
        saveEligibility: { eligible: false, reason: '还有 2 个采样槽位没确认结果' },
    });
    const tree = render();
    const body = squash(tree);

    assert.equal(findElement(tree, (node) => node.type === 'Button' && text(node) === '导出旧 hlwy 格式').props.disabled, true);
    assert.match(body, /没有原始数字，导不出旧格式/);
    assert.equal(findElement(tree, (node) => node.type === 'Button' && text(node) === '保存这次结果作参考').props.disabled, true);
    assert.match(body, /还有 2 个采样槽位没确认结果/);
});

test('export and rerun callbacks stay hand-offs, never inline work', () => {
    const { render, calls } = mount();
    const tree = render();
    findElement(tree, (node) => node.type === 'Button' && text(node) === '导出数据').props.onClick();
    findElement(tree, (node) => node.type === 'Button' && text(node) === '导出旧 hlwy 格式').props.onClick();
    findElement(tree, (node) => node.type === 'Button' && text(node) === '保存这次结果作参考').props.onClick();
    findElement(tree, (node) => node.type === 'Button' && text(node) === '重新采样').props.onClick();
    assert.deepEqual(calls, { recompare: [], restore: 0, save: 1, rerun: 1, full: 1, legacy: 1 });
});

test('a recompare pick does not follow the card to another report', () => {
    const { props, runtime, render } = mount({ reportKey: 41 });
    let tree = render();
    runtime.runEffects();

    findElement(tree, (node) => node.type === 'SearchableSelect').props.onValueChange('hlwy:b@bbb222');
    tree = render();
    const recompare = (node) => node.type === 'Button' && text(node) === '比较';
    assert.equal(findElement(tree, recompare).props.disabled, false, 'picked once, 重算 is armed');

    // 打开另一份报告：A 选的基准不是 B 的意图，不能预先填好。
    props.reportKey = 42;
    tree = render();
    runtime.runEffects();
    tree = render();
    assert.equal(findElement(tree, (node) => node.type === 'SearchableSelect').props.value, '', 'the pick is cleared');
    assert.equal(findElement(tree, recompare).props.disabled, true, 'nothing to recompute for the new report');
});
