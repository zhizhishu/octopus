const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

function walk(node, match, out = []) {
    if (Array.isArray(node)) { node.forEach(child => walk(child, match, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (match(node)) out.push(node);
    walk(node.props?.children, match, out);
    return out;
}
function text(node) {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    return text(node.props?.children);
}
function job(id, status = 'succeeded') {
    return { id, status, stage: status === 'succeeded' ? 'complete' : 'scan_users', percent: status === 'succeeded' ? 100 : 30,
        message: `backend progress ${id}`, apply: false, can_apply: true,
        request: { source_dsn: 'secret-server-only-source', include_logs: false, include_api_keys: false },
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:01:00Z' };
}
function harness(jobs = [job('first'), job('second','running')]) {
    let selection;
    let reads = [];
    let detailIDs = [];
    const list = { data: jobs, isFetching: false, isLoading: false, isError: false, refetch() { reads.push('list'); return Promise.resolve({ data: this.data }); } };
    const details = new Map();
    const jsx = (type, props) => typeof type === 'function' ? type(props) : ({ type, props });
    const { Migration } = load('src/components/modules/migration/index.tsx', {
        react: { useState: () => [selection, id => { selection = id; }] },
        'react/jsx-runtime': { jsx, jsxs: jsx },
        'lucide-react': new Proxy({}, { get: (_, key) => `Icon:${String(key)}` }),
        '@/api/endpoints/migration': {
            useMigrationJobs: () => list,
            useMigrationJob: (id, polling) => {
                detailIDs.push({ id, polling });
                return { data: details.get(id), isFetching: false, isError: false,
                    refetch: () => { reads.push(`detail:${id}`); return Promise.resolve({data:details.get(id)}); } };
            },
        },
        '@/components/common/PageWrapper': { PageWrapper: 'main' },
        '@/components/ui/badge': { Badge: 'Badge' },
        '@/components/ui/button': { Button: 'button' },
        '@/components/ui/progress': { Progress: 'Progress' },
        '@/lib/utils': { cn: (...values) => values.filter(Boolean).join(' ') },
    });
    return { list, details, reads, detailIDs, render: () => Migration() };
}

test('migration view contains only results and read actions, never source/orchestration inputs', () => {
    const h = harness();
    const tree = h.render();
    assert.equal(walk(tree, n => ['input','select','textarea','form'].includes(n.type)).length,0);
    assert.equal(walk(tree, n => n.type === 'button' && /取消|开始|执行|正式导入|试运行|重试/.test(text(n)) && n.props['aria-pressed'] === undefined).length,0);
    assert.doesNotMatch(text(tree),/secret-server-only-source/);
    assert.match(text(tree),/迁移结果/);
    assert.match(text(tree),/backend progress first/);
    assert.match(text(tree),/100%/);
});

test('refresh explicitly rereads list and selected backend result without creating jobs', () => {
    const h=harness();const tree=h.render();
    const button=walk(tree,n=>n.type==='button'&&text(n)==='Icon:RefreshCw刷新结果')[0]
        ?? walk(tree,n=>n.type==='button'&&text(n).includes('刷新结果'))[0];
    assert.ok(button);button.props.onClick();
    assert.deepEqual(h.reads,['list','detail:first']);
    assert.match(button.props.className,/h-11/);
});

test('history selection displays the chosen backend snapshot and polls only active jobs', () => {
    const h=harness();let tree=h.render();
    const second=walk(tree,n=>n.type==='button'&&text(n).includes('second'))[0];
    second.props.onClick();tree=h.render();
    assert.deepEqual(h.detailIDs.at(-1),{id:'second',polling:true});
    assert.match(text(tree),/backend progress second/);
    const selected=walk(tree,n=>n.type==='button'&&n.props['aria-pressed']===true);
    assert.equal(selected.length,1);assert.match(text(selected[0]),/second/);
    h.details.set('second',{...job('second','failed'),error:'backend-only terminal error'});
    assert.match(text(h.render()),/backend-only terminal error/);
});

test('empty result history refreshes only the list and does not fabricate a task', () => {
    const h=harness([]);const tree=h.render();
    assert.match(text(tree),/暂无迁移任务/);
    assert.equal(h.detailIDs.at(-1).id,undefined);
    walk(tree,n=>n.type==='button'&&text(n).includes('刷新结果'))[0].props.onClick();
    assert.deepEqual(h.reads,['list']);
});

test('read failure is disclosed without erasing previously loaded results', () => {
    const h=harness();h.list.isError=true;
    const tree=h.render();
    assert.equal(walk(tree,n=>n.props?.role==='alert').length,1);
    assert.match(text(tree),/backend progress first/);
    assert.match(text(tree),/不会重新执行任务/);
});

test('pending reads disable the explicit refresh control', () => {
    const h=harness();h.list.isFetching=true;
    const control=walk(h.render(),n=>n.type==='button'&&text(n).includes('刷新结果'))[0];
    assert.equal(control.props.disabled,true);assert.equal(control.props['aria-busy'],true);
});

test('migration hooks expose GET-only results with unchanged server routes', async () => {
    const requests=[];
    const hooks=load('src/api/endpoints/migration.ts',{
        '@tanstack/react-query':{useQuery:options=>options},
        '../client':{apiClient:{get:async path=>{requests.push(['GET',path]);return [];},post(){assert.fail('frontend must not mutate migrations');}}},
    });
    assert.deepEqual(Object.keys(hooks).sort(),['useMigrationJob','useMigrationJobs']);
    const list=hooks.useMigrationJobs(),detail=hooks.useMigrationJob('job/a',true);
    await list.queryFn();await detail.queryFn();
    assert.deepEqual(requests,[['GET','/api/v1/migration/newapi/jobs'],['GET','/api/v1/migration/newapi/jobs/job%2Fa']]);
    assert.equal(detail.refetchInterval,1500);
    assert.equal(hooks.useMigrationJob(undefined).enabled,false);
    assert.equal(hooks.useMigrationJob('completed',false).refetchInterval,false);
    assert.equal(list.refetchIntervalInBackground,false);
});
