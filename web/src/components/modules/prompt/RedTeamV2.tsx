'use client';

import { useEffect, useMemo, useState } from 'react';
import {
    AlertTriangle, BookOpen, CheckCircle2, ChevronDown, ClipboardCopy,
    FileJson, FlaskConical, History, Loader2, Plus, Save, Search,
    ShieldCheck, StopCircle,
} from 'lucide-react';
import { useChannelList } from '@/api/endpoints/channel';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { channelIDsForPlan } from './prompt-ui-state';
import {
    blankV2CaseSpec, useCancelRedTeamV2Run, useImportRedTeamV2Cases, useRedTeamV2Revisions, useRedTeamV2HistoricalCase,
    useRedTeamV2Cases, useRedTeamV2RunDetail, useRedTeamV2Runs,
    useSaveRedTeamV2Case, useStartRedTeamV2Run, useReviewRedTeamV2Run,
    type RedTeamV2CaseSpec, type RedTeamV2Run,
} from '@/api/endpoints/redteam-lab-v2';
import { toast } from '@/components/common/Toast';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';

const input = 'min-h-11 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring';
const area = 'min-h-28 w-full min-w-0 resize-y rounded-xl border border-input bg-background p-3 text-sm leading-6 text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring';
const card = 'rounded-3xl border border-border bg-card p-4 shadow-sm sm:p-5';

const runStatus: Record<RedTeamV2Run['status'], string> = {
    queued: '排队中', running: '执行中', completed: '已完成',
    cancelled: '已取消', timeout: '超时', transport_error: '请求异常',
    uncertain: '证据不足', budget_exhausted: '预算已用尽',
};
const scoreLabel: Record<RedTeamV2Run['verdict'], string> = {
    matched: '字面标记命中', not_matched: '字面标记未命中', unscored: '无有效评分',
};
const safeTime = (value: string) => {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? '未知时间' : date.toLocaleString('zh-CN');
};
const cloneSpec = (item: RedTeamV2CaseSpec): RedTeamV2CaseSpec => ({
    ...item,
    prompts: [...item.prompts],
    tags: [...(item.tags ?? [])],
});
const messageOf = (error: unknown) =>
    (error as { message?: string } | null)?.message ?? '操作失败，请检查授权和服务端日志';

type Panel = 'cases' | 'run' | 'results';

export function RedTeamV2({ enabled, dirtyConfig, onToggleEnabled, toggling }: {
    enabled: boolean;
    dirtyConfig: boolean;
    onToggleEnabled: () => void;
    toggling: boolean;
}) {
    const casesQuery = useRedTeamV2Cases();
    const runsQuery = useRedTeamV2Runs();
    const channelQuery = useChannelList();
    const plansQuery = useAccessPlanList();
    const saveCase = useSaveRedTeamV2Case();
    const importCases = useImportRedTeamV2Cases();
    const start = useStartRedTeamV2Run();
    const cancel = useCancelRedTeamV2Run();
    const review = useReviewRedTeamV2Run();

    const cases = casesQuery.data ?? [];
    const [panel, setPanel] = useState<Panel>('cases');
    const [search, setSearch] = useState('');
    // '' means awaiting the first fetch; null means an intentional NEW draft.
    // Keeping these distinct prevents the auto-select effect from erasing a new case.
    const [selectedID, setSelectedID] = useState<string | null>('');
    const [historyRevision, setHistoryRevision] = useState(0);
    const [draft, setDraft] = useState<RedTeamV2CaseSpec>(() => blankV2CaseSpec('case_manual'));
    const [importOpen, setImportOpen] = useState(false);
    const [importText, setImportText] = useState('');
    const [licenseConfirmed, setLicenseConfirmed] = useState(false);
    const [channelID, setChannelID] = useState('');
    const [channelSearch, setChannelSearch] = useState('');
    const [planFilter, setPlanFilter] = useState('');
    const [modelName, setModelName] = useState('');
    const [costConfirmed, setCostConfirmed] = useState(false);
    const [idempotencyKey, setIdempotencyKey] = useState('');
    const [selectedRunID, setSelectedRunID] = useState<number | null>(null);
    const [reviewDecision, setReviewDecision] = useState<'confirmed' | 'rejected' | 'uncertain'>('uncertain');
    const [reviewReason, setReviewReason] = useState('');

    const selected = cases.find((item) => item.spec.id === selectedID);
    const revisionsQuery = useRedTeamV2Revisions(selectedID ?? '');
    const historicQuery = useRedTeamV2HistoricalCase(selectedID ?? '', historyRevision);
    const savedMatch = !!selected && JSON.stringify(draft) === JSON.stringify(cloneSpec(selected.spec));
    const matchingCases = useMemo(() => cases.filter((item) => (
        [item.spec.name, item.spec.category, item.spec.id, ...item.spec.tags]
            .join(' ').toLowerCase().includes(search.trim().toLowerCase())
    )), [cases, search]);

    const channels = useMemo(() => (channelQuery.data ?? [])
        .map((item) => item.raw)
        .filter((item) => item.enabled && ![4, 5, 7].includes(item.type)),
    [channelQuery.data]);
    const plans = plansQuery.data ?? [];
    const filteredChannels = useMemo(() => {
        const selectedPlan = plans.find((item) => String(item.id) === planFilter);
        const ids = selectedPlan ? channelIDsForPlan(selectedPlan) : null;
        const term = channelSearch.trim().toLowerCase();
        return channels.filter((item) => (!ids || ids.has(item.id)) &&
            (!term || [String(item.id), item.name, item.model].join(' ').toLowerCase().includes(term)));
    }, [channels, plans, planFilter, channelSearch]);
    const channel = filteredChannels.find((item) => String(item.id) === channelID);
    const models = useMemo(() => {
        if (!channel) return [];
        return [...new Set([...(channel.selected_models ?? []), ...channel.model.split(',')]
            .map((item) => item.trim()).filter(Boolean))];
    }, [channel]);
    const runs = runsQuery.data ?? [];
    const detailQuery = useRedTeamV2RunDetail(selectedRunID);
    const currentRun = detailQuery.data?.run ?? runs.find((item) => item.id === selectedRunID);
    const detailTurns = detailQuery.data?.turns ?? [];
    const detailReviews = detailQuery.data?.reviews ?? [];

    useEffect(() => {
        if (selectedID !== '' || cases.length === 0) return;
        setSelectedID(cases[0].spec.id);
        setDraft(cloneSpec(cases[0].spec));
    }, [selectedID, cases]);
    useEffect(() => {
        if (!channelID && filteredChannels.length) setChannelID(String(filteredChannels[0].id));
    }, [channelID, filteredChannels]);
    useEffect(() => {
        if (!models.includes(modelName)) setModelName(models[0] ?? '');
    }, [models, modelName]);
    useEffect(() => {
        if (selectedRunID === null && runs.length) setSelectedRunID(runs[0].id);
    }, [selectedRunID, runs]);
    useEffect(() => {
        setReviewReason('');
        setReviewDecision('uncertain');
    }, [selectedRunID]);
    useEffect(() => {
        setHistoryRevision(0);
    }, [selectedID]);

    const edit = (patch: Partial<RedTeamV2CaseSpec>) =>
        setDraft((current) => ({ ...current, ...patch }));

    const hasUnsavedCase = selectedID === null
        ? JSON.stringify(draft) !== JSON.stringify(blankV2CaseSpec(draft.id))
        : selectedID !== '' && !savedMatch;
    const discardCheck = () => !hasUnsavedCase || window.confirm('当前案例有未保存的修改，确定放弃吗？');

    const changeCase = (id: string) => {
        if (id === selectedID || !discardCheck()) return;
        const target = cases.find((item) => item.spec.id === id);
        setSelectedID(target?.spec.id ?? '');
        if (target) setDraft(cloneSpec(target.spec));
        setCostConfirmed(false);
        setIdempotencyKey('');
    };
    const newCase = () => {
        if (!discardCheck()) return;
        setSelectedID(null);
        setDraft(blankV2CaseSpec(`case_${Date.now().toString(36)}`));
        setPanel('cases');
        setCostConfirmed(false);
        setIdempotencyKey('');
    };
    const doSave = async () => {
        try {
            const item = await saveCase.mutateAsync(draft);
            setSelectedID(item.spec.id);
            setDraft(cloneSpec(item.spec));
            setCostConfirmed(false);
            setIdempotencyKey('');
            toast.success(`案例已保存至不可变版本 #${item.revision}`);
        } catch (error) {
            toast.error(messageOf(error));
        }
    };
    const doImport = async () => {
        if (!licenseConfirmed) { toast.error('请先确认导入素材的许可证和使用权'); return; }
        try {
            const parsed: unknown = JSON.parse(importText);
            const items = Array.isArray(parsed) ? parsed : (
                parsed !== null && typeof parsed === 'object' && 'cases' in parsed
                    ? (parsed as { cases: unknown }).cases : null
            );
            if (!Array.isArray(items) || items.length < 1 || items.length > 20) {
                toast.error('JSON 应为 1–20 个 CaseSpec 的数组或包含 cases 的对象');
                return;
            }
            const saved = await importCases.mutateAsync(items as RedTeamV2CaseSpec[]);
            toast.success(`已导入 ${saved.length} 个案例版本`);
            setImportOpen(false);
            setImportText('');
            setLicenseConfirmed(false);
        } catch (error) {
            toast.error(error instanceof SyntaxError ? 'JSON 格式不合法' : messageOf(error));
        }
    };
    const startRun = async () => {
        if (!enabled || dirtyConfig || !savedMatch || !selected || !channel || !modelName || !costConfirmed) return;
        const key = idempotencyKey || crypto.randomUUID().replaceAll('-', '');
        setIdempotencyKey(key);
        try {
            const result = await start.mutateAsync({
                case_id: selected.spec.id,
                revision: selected.revision,
                channel_id: channel.id,
                model: modelName,
                confirm_cost: true,
                idempotency_key: key,
            });
            setSelectedRunID(result.id);
            setPanel('results');
            // A confirmed round-trip consumes this click's idempotency key.
            // Keep it only on unknown network errors, when a retry must not
            // accidentally create another billable attempt.
            setIdempotencyKey('');
            setCostConfirmed(false);
            toast.success('评估已登记；执行结果在任务记录中更新');
        } catch (error) {
            toast.error(messageOf(error));
        }
    };
    const cancelRun = async (id: number) => {
        try {
            await cancel.mutateAsync(id);
            toast.success('取消请求已登记；正在执行的请求可能已产生费用');
        } catch (error) {
            toast.error(messageOf(error));
        }
    };
    const saveReview = async () => {
        if (!currentRun || reviewReason.trim().length < 3) return;
        try {
            await review.mutateAsync({ id: currentRun.id, decision: reviewDecision, reason: reviewReason.trim() });
            setReviewReason('');
            toast.success('人工复核已追加到独立审计记录，原始评分保持不变');
        } catch (error) {
            toast.error(messageOf(error));
        }
    };
    const copyCase = async () => {
        try {
            await navigator.clipboard.writeText(JSON.stringify([draft], null, 2));
            toast.success('案例 JSON 已复制');
        } catch {
            toast.error('复制失败；可改用浏览器手动复制');
        }
    };

    return (
        <section className={card} aria-label="授权评估 V2">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="flex min-w-0 items-start gap-3">
                    <span className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary"><FlaskConical className="size-5" /></span>
                    <div>
                        <h3 className="text-base font-semibold">授权对抗评估 · V2</h3>
                        <p className="mt-1 max-w-2xl text-xs leading-5 text-muted-foreground">
                            版本化案例、最多四轮、单目标、幂等运行与数据库防重复执行。评估记录不进入模型真伪报告。
                        </p>
                    </div>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <Badge variant={enabled ? 'secondary' : 'outline'}>{enabled ? '可执行' : '默认关闭'}</Badge>
                    <Button size="sm" variant="outline" disabled={dirtyConfig || toggling} onClick={onToggleEnabled}>
                        {toggling ? <Loader2 className="mr-1 size-4 animate-spin" /> : <ShieldCheck className="mr-1 size-4" />}
                        {enabled ? '关闭实验' : '启用实验'}
                    </Button>
                </div>
            </div>
            {dirtyConfig ? <p className="mt-3 text-xs text-amber-600">旧版配置有未保存的改动，请先保存旧版草稿再切换总开关。</p> : null}
            {casesQuery.isError ? <p className="mt-3 text-sm text-destructive">案例库读取失败，服务端可能尚未升级到 V2。</p> : null}

            <div className="mt-4 flex flex-wrap items-center gap-2 border-b border-border pb-3" role="tablist" aria-label="授权评估 V2 工作区">
                {([
                    ['cases', '01 案例库', BookOpen],
                    ['run', '02 新建评估', FlaskConical],
                    ['results', '03 任务与结果', History],
                ] as const).map(([value, title, Icon]) => (
                    <button type="button" role="tab" aria-selected={panel === value} key={value}
                        className={cn('inline-flex min-h-11 items-center gap-2 rounded-xl px-3 text-sm font-medium transition-colors',
                            panel === value ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted')}
                        onClick={() => setPanel(value)}>
                        <Icon className="size-4" />{title}
                    </button>
                ))}
            </div>

            {panel === 'cases' && (
                <div className="mt-4 grid gap-4 xl:grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)]">
                    <div className="min-w-0 space-y-3">
                        <label className="relative block">
                            <Search className="pointer-events-none absolute left-3 top-3.5 size-4 text-muted-foreground" />
                            <input aria-label="搜索评估案例" className={cn(input, 'pl-9')}
                                value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索名称、分类、标签" />
                        </label>
                        <div className="flex justify-between gap-2">
                            <p className="self-center text-xs text-muted-foreground">{matchingCases.length} / {cases.length} 案例 · 版本不可变</p>
                            <Button size="sm" variant="outline" onClick={newCase}><Plus className="mr-1 size-4" />新建案例</Button>
                        </div>
                        <div className="max-h-80 space-y-2 overflow-y-auto">
                            {matchingCases.map((item) => (
                                <button type="button" key={item.spec.id} onClick={() => changeCase(item.spec.id)}
                                    className={cn('w-full min-w-0 rounded-xl border p-3 text-left transition-colors hover:bg-muted/40',
                                        selectedID === item.spec.id ? 'border-primary/60 bg-primary/5' : 'border-border')}>
                                    <div className="flex items-center justify-between gap-2">
                                        <span className="truncate text-sm font-medium">{item.spec.name}</span>
                                        <Badge variant="outline">v{item.revision}</Badge>
                                    </div>
                                    <p className="mt-1 truncate text-xs text-muted-foreground">{item.spec.category} · {item.spec.prompts.length} 轮</p>
                                </button>
                            ))}
                            {!matchingCases.length && <p className="rounded-xl border border-dashed border-border p-5 text-center text-xs text-muted-foreground">暂无匹配案例。可以新建或导入已授权的 JSON。</p>}
                        </div>
                        <Button variant="outline" className="w-full" onClick={() => setImportOpen((open) => !open)}>
                            <FileJson className="mr-2 size-4" />{importOpen ? '关闭导入区' : '导入 JSON 案例包（最多 20 条）'}
                        </Button>
                        {importOpen && (
                            <div className="space-y-2 rounded-xl border border-border p-3">
                                <textarea aria-label="导入案例 JSON" className={area} value={importText}
                                    onChange={(event) => setImportText(event.target.value)}
                                    placeholder='[{"schema":"octopus.redteam.case.v1", ...}]' />
                                <label className="flex items-start gap-2 text-xs leading-5">
                                    <input type="checkbox" className="mt-1 size-4" checked={licenseConfirmed}
                                        onChange={(event) => setLicenseConfirmed(event.target.checked)} />
                                    我已核对全部案例来源，仅导入 original/MIT/Apache-2.0/CC0-1.0 等获准素材。
                                </label>
                                <Button size="sm" disabled={!licenseConfirmed || !importText.trim() || importCases.isPending}
                                    onClick={() => void doImport()}>{importCases.isPending ? '导入中…' : '确认导入'}</Button>
                            </div>
                        )}
                    </div>
                    <div className="min-w-0 space-y-3 rounded-2xl border border-border p-3 sm:p-4">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                            <h4 className="text-sm font-semibold">案例编辑器 {selected ? <span className="text-xs font-normal text-muted-foreground">· 当前 v{selected.revision}</span> : '· 新案例'}</h4>
                            <Button size="sm" variant="outline" onClick={() => void copyCase()}><ClipboardCopy className="mr-1 size-4" />复制 JSON</Button>
                        </div>
                        {selected && selected.revision > 1 && (
                            <details className="rounded-xl border border-border bg-muted/15 p-3">
                                <summary className="min-h-8 cursor-pointer text-xs font-semibold">历史版本对照 · 当前 v{selected.revision}</summary>
                                <label className="mt-2 grid gap-1 text-xs text-muted-foreground">选择只读历史版本
                                    <select aria-label="V2历史版本对照" className={input} value={historyRevision}
                                        onChange={(e) => setHistoryRevision(Number(e.target.value))}>
                                        <option value={0}>选择一个历史版本</option>
                                        {(revisionsQuery.data ?? []).filter((revision) => revision.revision < selected.revision).map((revision) => (
                                            <option value={revision.revision} key={revision.revision}>版本 {revision.revision} · {safeTime(revision.created_at)}</option>
                                        ))}
                                    </select>
                                </label>
                                {historyRevision > 0 && historicQuery.isLoading && <p className="mt-2 text-xs text-muted-foreground">正在读取历史版本…</p>}
                                {historicQuery.data && historyRevision > 0 && (
                                    <div className="mt-3 grid gap-2 lg:grid-cols-2">
                                        <div className="min-w-0">
                                            <h6 className="mb-1 text-xs font-medium">历史 v{historyRevision} · 只读</h6>
                                            <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-background p-2 text-xs">{JSON.stringify(historicQuery.data.spec, null, 2)}</pre>
                                        </div>
                                        <div className="min-w-0">
                                            <h6 className="mb-1 text-xs font-medium">当前 v{selected.revision} · 草稿单独保存</h6>
                                            <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-background p-2 text-xs">{JSON.stringify(selected.spec, null, 2)}</pre>
                                        </div>
                                    </div>
                                )}
                            </details>
                        )}
                        <div className="grid gap-3 sm:grid-cols-2">
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">案例 ID
                                <input aria-label="案例 ID" className={input} value={draft.id} maxLength={48}
                                    disabled={!!selected} onChange={(e) => edit({ id: e.target.value })} />
                            </label>
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">风险分类
                                <input aria-label="风险分类" className={input} value={draft.category} maxLength={64}
                                    onChange={(e) => edit({ category: e.target.value })} />
                            </label>
                        </div>
                        <label className="grid gap-1 text-xs font-medium text-muted-foreground">案例名称
                            <input aria-label="案例名称" className={input} value={draft.name} maxLength={80}
                                onChange={(e) => edit({ name: e.target.value })} />
                        </label>
                        <div className="flex items-center justify-between gap-2">
                            <p className="text-xs font-medium text-muted-foreground">对话轮次（1–4；每次至多一条目标请求）</p>
                            <Button size="sm" variant="outline" disabled={draft.prompts.length >= 4}
                                onClick={() => edit({ prompts: [...draft.prompts, '请总结上一轮回复。'] })}><Plus className="mr-1 size-4" />增加轮次</Button>
                        </div>
                        {draft.prompts.map((prompt, index) => (
                            <div className="grid gap-1" key={index}>
                                <div className="flex items-center justify-between">
                                    <span className="text-xs text-muted-foreground">第 {index + 1} 轮用户题目</span>
                                    <button type="button" className="text-xs text-muted-foreground hover:text-destructive"
                                        disabled={draft.prompts.length <= 1}
                                        onClick={() => edit({ prompts: draft.prompts.filter((_, i) => i !== index) })}>删除此轮</button>
                                </div>
                                <textarea aria-label={`第 ${index + 1} 轮题目`} className={area} maxLength={3000} value={prompt}
                                    onChange={(e) => edit({ prompts: draft.prompts.map((p, i) => i === index ? e.target.value : p) })} />
                            </div>
                        ))}
                        <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,140px)]">
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">字面期望标记（选填，不代表绕过）
                                <input aria-label="V2期望标记" className={input} maxLength={160}
                                    value={draft.expected} onChange={(e) => edit({ expected: e.target.value })} />
                            </label>
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">每轮 max_tokens
                                <input aria-label="V2输出令牌上限" className={input} type="number" min={16} max={512}
                                    value={draft.max_tokens} onChange={(e) => edit({ max_tokens: Number(e.target.value) })} />
                            </label>
                        </div>
                        <label className="grid gap-1 text-xs font-medium text-muted-foreground">标签（逗号分隔，最多八个）
                            <input aria-label="案例标签" className={input} value={draft.tags.join(', ')}
                                onChange={(e) => edit({ tags: e.target.value.split(',').map((tag) => tag.trim()).filter(Boolean).slice(0, 8) })} />
                        </label>
                        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3">
                            <p className="max-w-sm text-xs leading-5 text-muted-foreground">
                                每次保存生成新不可变版本；既有运行始终引用旧版本及哈希。当前仅支持手写案例，第三方素材请走带许可核查的 JSON 导入。
                            </p>
                            <Button onClick={() => void doSave()} disabled={savedMatch || saveCase.isPending}>
                                {saveCase.isPending ? <Loader2 className="mr-1 size-4 animate-spin" /> : <Save className="mr-1 size-4" />}
                                {savedMatch ? '已保存' : '保存案例版本'}
                            </Button>
                        </div>
                    </div>
                </div>
            )}

            {panel === 'run' && (
                <div className="mt-4 grid gap-4 lg:grid-cols-2">
                    <div className="space-y-3">
                        <h4 className="text-sm font-semibold">选择已保存的案例版本</h4>
                        <select aria-label="选择已保存案例" className={input} value={selectedID ?? ''} onChange={(event) => changeCase(event.target.value)}>
                            <option value="">请选择案例</option>
                            {cases.map((item) => <option key={item.spec.id} value={item.spec.id}>{item.spec.name} · v{item.revision}</option>)}
                        </select>
                        {selected ? (
                            <div className="rounded-xl border border-border p-3 text-xs leading-6 text-muted-foreground">
                                <p className="font-medium text-foreground">{selected.spec.category} · {selected.spec.prompts.length} 轮 · 每轮最多 {selected.spec.max_tokens} 输出 token</p>
                                <p>版本 SHA-256：<code className="break-all">{selected.sha256.slice(0, 20)}…</code></p>
                                <p>请求数上限：{selected.spec.prompts.length}；任务中途拒绝/取消则提前停止，不自动重试。</p>
                            </div>
                        ) : <p className="text-xs text-muted-foreground">先在案例库中保存一个案例。</p>}
                        {!savedMatch && <p className="text-xs text-amber-600">当前案例有尚未保存的改动，禁止拿旧版本偷偷执行。</p>}
                    </div>
                    <div className="space-y-3">
                        <h4 className="text-sm font-semibold">真实目标（复用既有渠道出站）</h4>
                        <div className="grid gap-2 sm:grid-cols-2">
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">按访问方案筛选
                                <select aria-label="V2访问方案筛选" className={input} value={planFilter}
                                    onChange={(e) => { setPlanFilter(e.target.value); setChannelID(''); setModelName(''); setCostConfirmed(false); setIdempotencyKey(''); }}>
                                    <option value="">全部访问方案</option>
                                    {plans.map((item) => <option value={String(item.id)} key={item.id}>{item.display_name || item.slug}</option>)}
                                </select>
                            </label>
                            <label className="grid gap-1 text-xs font-medium text-muted-foreground">搜索渠道或模型
                                <input aria-label="V2搜索目标渠道" className={input} value={channelSearch}
                                    onChange={(e) => { setChannelSearch(e.target.value); setChannelID(''); setModelName(''); setCostConfirmed(false); }}
                                    placeholder="名称 / ID / 模型" />
                            </label>
                        </div>
                        <p className="text-xs text-muted-foreground">筛选只用于导航，不改变渠道在 VIP/SVIP/SSVIP 等方案中的共享范围。</p>
                        <label className="grid gap-1 text-xs font-medium text-muted-foreground">目标渠道（{filteredChannels.length} 条）
                            <select aria-label="V2目标渠道" className={input} value={channelID}
                                onChange={(e) => { setChannelID(e.target.value); setModelName(''); setCostConfirmed(false); setIdempotencyKey(''); }}>
                                {!filteredChannels.length && <option value="">无匹配渠道</option>}
                                {filteredChannels.map((item) => <option value={String(item.id)} key={item.id}>#{item.id} · {item.name}</option>)}
                            </select>
                        </label>
                        <label className="grid gap-1 text-xs font-medium text-muted-foreground">目标模型
                            <select aria-label="V2目标模型" className={input} value={modelName}
                                onChange={(e) => { setModelName(e.target.value); setCostConfirmed(false); setIdempotencyKey(''); }}>
                                {!models.length && <option value="">未配置模型</option>}
                                {models.map((m) => <option key={m} value={m}>{m}</option>)}
                            </select>
                        </label>
                        <div className="rounded-xl border border-border bg-muted/25 p-3 text-xs leading-5 text-muted-foreground">
                            <p className="flex items-center gap-2 font-medium text-foreground"><AlertTriangle className="size-4 text-amber-500" />费用、并发与失败语义</p>
                            <p className="mt-1">该任务可能向实际供应商请求 1–4 次。已在 DB 里记录每次尝试，服务器崩溃或失败不会自动重复扣费。部分 Claude 出站可能忽略自定义 max_tokens，因此这里仅承诺请求次数上限，不能伪称精确美元费用硬上限。一次仅允许运行一个 V2 任务。</p>
                        </div>
                        <label className="flex items-start gap-2 text-xs leading-5">
                            <input aria-label="V2确认可能费用" type="checkbox" className="mt-1 size-4" checked={costConfirmed}
                                onChange={(e) => setCostConfirmed(e.target.checked)} />
                            我确认已授权针对所选渠道和模型执行该版本的评估，并接受最多 {selected?.spec.prompts.length ?? 0} 次可能收费的请求。
                        </label>
                        <Button className="min-h-11 w-full" onClick={() => void startRun()}
                            disabled={!enabled || dirtyConfig || !savedMatch || !selected || !channel || !modelName || !costConfirmed || start.isPending}>
                            {start.isPending ? <Loader2 className="mr-2 size-4 animate-spin" /> : <FlaskConical className="mr-2 size-4" />}
                            {start.isPending ? '正在登记评估…' : '确认并登记评估'}
                        </Button>
                        {!enabled && <p className="text-center text-xs text-amber-600">实验总开关尚未开启，默认不会向任何模型发题。</p>}
                    </div>
                </div>
            )}

            {panel === 'results' && (
                <div className="mt-4 grid gap-4 xl:grid-cols-[minmax(0,0.7fr)_minmax(0,1.3fr)]">
                    <div className="min-w-0">
                        <div className="mb-3 flex items-center justify-between gap-2">
                            <p className="text-xs text-muted-foreground">最近 {runs.length} 条评估记录（只读轮询，不发模型请求）</p>
                            <Button variant="outline" size="sm" onClick={() => void runsQuery.refetch()} disabled={runsQuery.isFetching}>刷新</Button>
                        </div>
                        {runsQuery.isError && <p className="text-sm text-destructive">无法读取运行列表。</p>}
                        <div className="max-h-96 space-y-2 overflow-y-auto">
                            {runs.map((item) => (
                                <button type="button" key={item.id} onClick={() => setSelectedRunID(item.id)}
                                    className={cn('w-full rounded-xl border p-3 text-left hover:bg-muted/30',
                                        selectedRunID === item.id ? 'border-primary/60 bg-primary/5' : 'border-border')}>
                                    <div className="flex items-center justify-between gap-2">
                                        <span className="truncate text-sm font-medium">{item.case_id} · v{item.case_revision}</span>
                                        <Badge variant="outline">{runStatus[item.status] ?? item.status}</Badge>
                                    </div>
                                    <p className="mt-1 truncate text-xs text-muted-foreground">{item.channel_name} · {item.request_model}</p>
                                    <p className="mt-1 text-xs text-muted-foreground">{safeTime(item.created_at)} · 已尝试 {item.target_requests} 次</p>
                                </button>
                            ))}
                            {!runs.length && <p className="rounded-xl border border-dashed border-border p-5 text-center text-xs text-muted-foreground">尚无 V2 运行记录。</p>}
                        </div>
                    </div>
                    <div className="min-w-0 rounded-2xl border border-border p-3 sm:p-4">
                        {!currentRun ? <div className="flex min-h-48 flex-col items-center justify-center gap-2 text-center text-muted-foreground">
                            <History className="size-6" /><p className="text-sm">选择一条运行查看不可变版本、证据和输出</p>
                        </div> : <>
                            <div className="flex flex-wrap items-center justify-between gap-2">
                                <div>
                                    <h4 className="text-sm font-semibold">运行 #{currentRun.id} · {runStatus[currentRun.status]}</h4>
                                    <p className="mt-1 text-xs text-muted-foreground">{currentRun.case_id} v{currentRun.case_revision} · {scoreLabel[currentRun.verdict]}</p>
                                </div>
                                {(currentRun.status === 'running' || currentRun.status === 'queued') && (
                                    <Button size="sm" variant="outline" disabled={cancel.isPending || currentRun.cancel_requested}
                                        onClick={() => void cancelRun(currentRun.id)}>
                                        <StopCircle className="mr-1 size-4" />{currentRun.cancel_requested ? '取消已登记' : '取消后续请求'}
                                    </Button>
                                )}
                            </div>
                            <div className="mt-3 grid grid-cols-3 gap-2 rounded-xl bg-muted/25 p-3 text-center">
                                <div><p className="text-xs text-muted-foreground">尝试</p><p className="text-sm font-semibold tabular-nums">{currentRun.target_requests}</p></div>
                                <div><p className="text-xs text-muted-foreground">输入 tokens</p><p className="text-sm font-semibold tabular-nums">{currentRun.input_tokens}</p></div>
                                <div><p className="text-xs text-muted-foreground">输出 tokens</p><p className="text-sm font-semibold tabular-nums">{currentRun.output_tokens}</p></div>
                            </div>
                            {currentRun.error_message && <p className="mt-3 rounded-xl border border-amber-400/20 bg-amber-400/5 p-3 text-xs text-amber-700">{currentRun.error_message}</p>}
                            <div className="mt-3 space-y-3">
                                {detailTurns.map((turn) => (
                                    <div key={turn.id} className="rounded-xl border border-border p-3">
                                        <div className="mb-2 flex items-center justify-between gap-2">
                                            <p className="text-xs font-semibold">第 {turn.sequence} 轮 · {runStatus[turn.status] ?? turn.status}</p>
                                            <span className="text-xs text-muted-foreground">{turn.input_tokens} / {turn.output_tokens} tokens</span>
                                        </div>
                                        <p className="mb-2 whitespace-pre-wrap break-words text-xs text-muted-foreground">{turn.input_text}</p>
                                        <div className="max-h-56 overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-muted/30 p-3 text-sm leading-6">
                                            {turn.response_text || '本轮无可见回答'}
                                        </div>
                                        {turn.response_clipped && <p className="mt-1 text-xs text-amber-600">证据仅保存回复前 4000 字；已停止向后续轮次回放截断内容。</p>}
                                    </div>
                                ))}
                                {!detailTurns.length && <div className="rounded-xl border border-dashed border-border p-5 text-center text-xs text-muted-foreground">暂无轮次证据；如有超时/重启，先确认费用再考虑新任务。</div>}
                            </div>
                            {currentRun.status === 'completed' && <p className="mt-3 flex items-start gap-2 text-xs leading-5 text-muted-foreground">
                                <CheckCircle2 className="mt-0.5 size-4 shrink-0" />这里只验证期望字面标记是否出现，不代表安全机制被绕过，也不是真实模型身份认证。
                            </p>}
                            <div className="mt-4 space-y-3 border-t border-border pt-4">
                                <h5 className="text-sm font-semibold">人工复核 · 独立于机器评分</h5>
                                {detailReviews.map((item) => (
                                    <div className="rounded-xl border border-border p-3 text-xs leading-5" key={item.id}>
                                        <div className="flex flex-wrap justify-between gap-2 text-muted-foreground">
                                            <span>复核 #{item.id} · {item.decision === 'confirmed' ? '已确认' : item.decision === 'rejected' ? '不支持该结论' : '证据不足'}</span>
                                            <span>{safeTime(item.created_at)}</span>
                                        </div>
                                        <p className="mt-2 whitespace-pre-wrap break-words">{item.reason}</p>
                                    </div>
                                ))}
                                {detailReviews.length === 0 && <p className="text-xs text-muted-foreground">尚未进行人工复核。字面命中不自动等于实际安全缺陷。</p>}
                                {(currentRun.status !== 'queued' && currentRun.status !== 'running') && (
                                    <div className="space-y-2 rounded-xl border border-border bg-muted/10 p-3">
                                        <label className="grid gap-1 text-xs text-muted-foreground">复核结论
                                            <select aria-label="V2人工复核结论" className={input} value={reviewDecision}
                                                onChange={(e) => setReviewDecision(e.target.value as 'confirmed' | 'rejected' | 'uncertain')}>
                                                <option value="uncertain">证据不足（默认）</option>
                                                <option value="confirmed">证据支持当前问题</option>
                                                <option value="rejected">证据不支持当前问题</option>
                                            </select>
                                        </label>
                                        <label className="grid gap-1 text-xs text-muted-foreground">复核说明（至少三字）
                                            <textarea aria-label="V2人工复核说明" className={area} maxLength={800}
                                                value={reviewReason} onChange={(e) => setReviewReason(e.target.value)}
                                                placeholder="记录为何支持或不支持结论；不能仅凭 marker 判定安全绕过" />
                                        </label>
                                        <Button size="sm" variant="outline" disabled={review.isPending || reviewReason.trim().length < 3}
                                            onClick={() => void saveReview()}>
                                            {review.isPending ? <Loader2 className="mr-1 size-4 animate-spin" /> : <CheckCircle2 className="mr-1 size-4" />}
                                            追加人工复核记录
                                        </Button>
                                    </div>
                                )}
                            </div>
                        </>}
                    </div>
                </div>
            )}
            <p className="mt-4 text-xs leading-5 text-muted-foreground">
                评估 V2 仅在明确启用并确认费用后发送；第三方案例生成、在线攻击服务与模型检测任务均不接入该入口。没有原始 TLS/JA3/H2 抓包时，不宣称已完成线级形状验收。
                <ChevronDown className="ml-1 inline size-3" />
            </p>
        </section>
    );
}
