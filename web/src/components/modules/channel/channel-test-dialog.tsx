'use client';

// 渠道内测试对话框（学 new-api）：多选端点 + 流/非流开关 + 模型多选批量测试，
// 复用 /api/v1/model/test 与统一的 shouldForceChannelTestStream + 180s。替代原独立「模型测试」页。
import { useMemo, useState } from 'react';
import { CheckCircle2, Fingerprint, Loader2, Lock, Play, RotateCw, XCircle } from 'lucide-react';
import type { Channel } from '@/api/endpoints/channel';
import { useChannelTestIdentity, useModelTest, type EndpointIdentity } from '@/api/endpoints/model';
import type { ApiError } from '@/api/types';
import { toast } from '@/components/common/Toast';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import {
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
} from '@/components/ui/table';
import { cn } from '@/lib/utils';
import { cleanOneMillionModelName } from '@/lib/model-aliases';
import {
    DEFAULT_MODEL_TEST_TIMEOUT_SECONDS,
    MODEL_TEST_ENDPOINTS,
    MODEL_TEST_ENDPOINT_LABELS,
    defaultModelTestEndpointForChannel,
    makeModelTestPrompt,
    shouldForceChannelTestStream,
    type ModelTestEndpoint,
} from '@/lib/channel-test';
import { getSelectedChannelModels } from './channel-utils';

type CellStatus = 'testing' | 'success' | 'error';
type CellResult = { status: CellStatus; ms?: number; error?: string };
// 重复次数 > 1 时的逐格汇总（治日志刷屏：不逐条落库、对话框看统计）
type CellSummary = { total: number; done: number; ok: number; capacity: number; timeout: number; other: number; msSum: number; capacityError?: string; timeoutError?: string; otherError?: string };

const MAX_REPEAT = 500;

// 端点自动模拟的 shape（与后端 runner 的端点→shape 映射一致；测试永远用渠道真实 profile）
const ENDPOINT_IDENTITY: Record<ModelTestEndpoint, 'codex' | 'claude' | 'generic'> = {
    openai_chat: 'generic',
    openai_responses: 'codex',
    anthropic_messages: 'claude',
    gemini_generate_content: 'generic',
};
const SHAPE_LABEL: Record<'codex' | 'claude' | 'generic', string> = {
    codex: 'codex CLI', claude: 'claude CLI', generic: '通用 UA',
};
const SHAPE_CLASS: Record<'codex' | 'claude' | 'generic', string> = {
    codex: 'bg-primary/15 text-primary',
    claude: 'bg-amber-500/15 text-amber-600 dark:text-amber-400',
    generic: 'bg-muted text-muted-foreground',
};

// 失败归类，供汇总模式统计（容量满是上游坏窗口特征，单列出来）
function classifyError(err?: string): 'capacity' | 'timeout' | 'other' {
    const e = err || '';
    if (/负载已(经)?达到?上限|已达上限|capacity|rate.?limit|429|529/i.test(e)) return 'capacity';
    if (/超时|timed?\s?out|timeout|deadline/i.test(e)) return 'timeout';
    return 'other';
}

// 兜底脱敏：上游报错原文万一带出上游身份（URL / 域名 / IP），一律抹成「上游」，绝不让上游身份泄漏到页面。
// 通用规则（不硬编码任何单一上游马甲名，公开仓也不留马甲字面量）：先抹整条 URL，再抹裸域名（含端口），再抹 IPv4。
function redactUpstreamIdentity(text: string): string {
    // 常见代码/文件扩展名：错误串里的 `config.json` / `node.js` / `main.go` 不是上游域名，别误抹。
    const FILE_EXT = 'js|mjs|cjs|jsx|ts|tsx|go|py|rs|rb|php|java|kt|c|h|cc|cpp|hpp|cs|swift|json|ya?ml|toml|ini|env|md|txt|csv|log|html?|css|scss|sh|bash|zsh|sql|proto|lock|png|jpe?g|gif|svg|webp|ico|pdf|zip|tar|gz|exe|dll|so|dylib|bin|db|sqlite';
    return text
        // 整条 URL（http/https）——最明确的泄漏向量
        .replace(/https?:\/\/[^\s"'）)\]]+/gi, '上游')
        // 裸域名 host（两段以上、末段 2+ 字母 TLD、可选 :端口）；末段是常见文件扩展名的排除掉
        .replace(new RegExp(`\\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\\.)+(?!(?:${FILE_EXT})\\b)[a-z]{2,}(?::\\d{2,5})?\\b`, 'gi'), '上游')
        // IPv4（含可选端口）
        .replace(/\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b/g, '上游');
}

const pillClass = (selected: boolean) =>
    cn(
        'nodrag rounded-full border px-2.5 py-1 text-xs font-medium transition-colors',
        selected
            ? 'border-primary/50 bg-primary/15 text-primary'
            : 'border-border/70 bg-background/60 text-muted-foreground hover:bg-muted',
    );

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

export function ChannelTestDialog({
    channel,
    open,
    onOpenChange,
}: {
    channel: Channel;
    open: boolean;
    onOpenChange: (open: boolean) => void;
}) {
    const modelTest = useModelTest();
    const allModels = useMemo(() => getSelectedChannelModels(channel), [channel]);
    const defaultEndpoint = defaultModelTestEndpointForChannel(channel.type);

    const [endpoints, setEndpoints] = useState<Set<ModelTestEndpoint>>(() => new Set<ModelTestEndpoint>([defaultEndpoint]));
    const [models, setModels] = useState<Set<string>>(() => new Set(allModels.slice(0, 1)));
    const [streamTest, setStreamTest] = useState(true);
    const [filter, setFilter] = useState('');
    const [results, setResults] = useState<Record<string, CellResult>>({});
    const [summaries, setSummaries] = useState<Record<string, CellSummary>>({});
    // 输入用字符串态，允许编辑中清空/半截；repeat 是派生的钳位值（供逻辑/展示用）。
    // 直接对数字态 value 钳位会在清空那一刻把 99 吃回 1（用户实测到的坑）。
    const [repeatInput, setRepeatInput] = useState('1');
    const repeat = Math.max(1, Math.min(MAX_REPEAT, Math.floor(Number(repeatInput)) || 1));
    const [testing, setTesting] = useState(false);
    const identityQuery = useChannelTestIdentity(channel.id, open);

    // Shape / Stream lock: 对当前选中的 models + endpoints + 渠道配置调用 SSOT
    const streamShapeLocked = useMemo(() => {
        const selectedModelList = Array.from(models);
        // 如果已选端点中有任何一个端点触发强制流式，或渠道级别触发强制流式，则强制流式
        if (
            shouldForceChannelTestStream({
                channelType: channel.type,
                cloakMode: channel.cloak?.mode,
                anthropicContext1M: channel.anthropic_context_1m,
            })
        ) {
            return true;
        }
        for (const ep of endpoints) {
            if (
                shouldForceChannelTestStream({
                    models: selectedModelList,
                    endpoint: ep,
                    anthropicContext1M: channel.anthropic_context_1m,
                    channelType: channel.type,
                    cloakMode: channel.cloak?.mode,
                })
            ) {
                return true;
            }
        }
        return false;
    }, [channel.type, channel.cloak?.mode, channel.anthropic_context_1m, models, endpoints]);

    const filteredModels = useMemo(() => {
        const q = filter.trim().toLowerCase();
        return q ? allModels.filter((m) => m.toLowerCase().includes(q)) : allModels;
    }, [allModels, filter]);

    const allFilteredSelected = filteredModels.length > 0 && filteredModels.every((m) => models.has(m));
    const cellKey = (endpoint: ModelTestEndpoint, model: string) => `${endpoint}|${model}`;

    const toggleEndpoint = (endpoint: ModelTestEndpoint) =>
        setEndpoints((prev) => {
            const next = new Set(prev);
            if (next.has(endpoint)) next.delete(endpoint); else next.add(endpoint);
            return next;
        });
    const toggleModel = (model: string) =>
        setModels((prev) => {
            const next = new Set(prev);
            if (next.has(model)) next.delete(model); else next.add(model);
            return next;
        });
    const toggleAllModels = () =>
        setModels((prev) => {
            const next = new Set(prev);
            if (allFilteredSelected) filteredModels.forEach((m) => next.delete(m));
            else filteredModels.forEach((m) => next.add(m));
            return next;
        });

    const runTest = async () => {
        const eps = [...endpoints];
        const mods = [...models];
        if (eps.length === 0 || mods.length === 0) {
            toast.error('请至少选一个端点和一个模型');
            return;
        }
        const times = Math.max(1, Math.min(Math.floor(repeat) || 1, MAX_REPEAT));
        const summaryMode = times > 1;
        setTesting(true);
        const prompt = makeModelTestPrompt();

        if (summaryMode) {
            // 汇总模式：探针不逐条落审计日志（audit_log:false，治刷屏），对话框看成功率 + 失败分类。
            setResults({});
            setSummaries(() => {
                const next: Record<string, CellSummary> = {};
                for (const e of eps) for (const m of mods)
                    next[cellKey(e, m)] = { total: times, done: 0, ok: 0, capacity: 0, timeout: 0, other: 0, msSum: 0 };
                return next;
            });
        } else {
            setSummaries({});
            setResults(() => {
                const next: Record<string, CellResult> = {};
                for (const e of eps) for (const m of mods) next[cellKey(e, m)] = { status: 'testing' };
                return next;
            });
        }

        // 每个「端点 × 模型」各发一个独立单模型请求（重复模式下发 times 次），谁先测完谁先填
        // 那一格；慢模型不卡快模型。后端逐模型 fan-out，故上游请求数 = E×M×times。
        const bump = (endpoint: ModelTestEndpoint, model: string, ok: boolean, ms: number, err?: string) =>
            setSummaries((prev) => {
                const cur = prev[cellKey(endpoint, model)];
                if (!cur) return prev;
                const c: CellSummary = { ...cur, done: cur.done + 1 };
                if (ok) { c.ok += 1; c.msSum += ms || 0; }
                else {
                    const kind = classifyError(err);
                    c[kind] += 1;
                    // 每类各留一条报错原文（治"只看到计数、看不到 76 满 / 23 其它各错在哪"）。
                    const clean = err && err.trim() ? redactUpstreamIdentity(err.trim()) : '';
                    if (clean) {
                        if (kind === 'capacity' && !c.capacityError) c.capacityError = clean;
                        else if (kind === 'timeout' && !c.timeoutError) c.timeoutError = clean;
                        else if (kind === 'other' && !c.otherError) c.otherError = clean;
                    }
                }
                return { ...prev, [cellKey(endpoint, model)]: c };
            });

        const jobs: Array<() => Promise<void>> = [];
        for (const endpoint of eps) {
            for (const model of mods) {
                const runOnce = async () => {
                    try {
                        const data = await modelTest.mutateAsync({
                            models: [model],
                            channel_id: channel.id,
                            endpoint,
                            prompt,
                            stream: streamShapeLocked ? true : streamTest,
                            timeout_seconds: DEFAULT_MODEL_TEST_TIMEOUT_SECONDS,
                            audit_log: !summaryMode,
                        });
                        const r = data.results?.[0];
                        if (summaryMode) {
                            bump(endpoint, model, !!r?.success, r?.duration_ms || 0, r?.error);
                        } else {
                            setResults((prev) => ({
                                ...prev,
                                [cellKey(endpoint, model)]: r
                                    ? (r.success ? { status: 'success', ms: r.duration_ms } : { status: 'error', error: r.error ? redactUpstreamIdentity(r.error) : r.error })
                                    : { status: 'error', error: '无结果' },
                            }));
                        }
                    } catch (error: unknown) {
                        const msg = apiErrorMessage(error) || '测试失败';
                        if (summaryMode) bump(endpoint, model, false, 0, msg);
                        else setResults((prev) => ({ ...prev, [cellKey(endpoint, model)]: { status: 'error', error: msg } }));
                    }
                };
                for (let i = 0; i < times; i++) jobs.push(runOnce);
            }
        }

        // 低并发 2 路 + 随机间隔：像真人一发一发挤进去，别像扫描器一次性把上游打爆。
        const LIMIT = 2;
        const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));
        const paceMs = () => 250 + Math.floor(Math.random() * 450); // 250–700ms 抖动
        let cursor = 0;
        const workers = Array.from({ length: Math.min(LIMIT, jobs.length) }, async (_v, workerIndex) => {
            await sleep(workerIndex * paceMs()); // 两路错峰起步，不在同一瞬间齐发
            while (cursor < jobs.length) {
                const mine = cursor++;
                await jobs[mine]();
                if (cursor < jobs.length) await sleep(paceMs()); // 每发之间留间隔
            }
        });
        try {
            await Promise.all(workers);
            toast.success(summaryMode ? `测试完成 · ${times} 轮，见下方汇总` : '测试完成，详情见下表（也已写入日志）');
        } finally {
            setTesting(false);
        }
    };

    const resultRows = useMemo(() => {
        const rows: Array<{ endpoint: ModelTestEndpoint; model: string; result: CellResult }> = [];
        for (const e of MODEL_TEST_ENDPOINTS) {
            if (!endpoints.has(e.value)) continue;
            for (const m of models) {
                const result = results[cellKey(e.value, m)];
                if (result) rows.push({ endpoint: e.value, model: m, result });
            }
        }
        return rows;
    }, [endpoints, models, results]);

    const summaryRows = useMemo(() => {
        const rows: Array<{ endpoint: ModelTestEndpoint; model: string; s: CellSummary }> = [];
        for (const e of MODEL_TEST_ENDPOINTS) {
            if (!endpoints.has(e.value)) continue;
            for (const m of models) {
                const s = summaries[cellKey(e.value, m)];
                if (s) rows.push({ endpoint: e.value, model: m, s });
            }
        }
        return rows;
    }, [endpoints, models, summaries]);

    const identity = identityQuery.data;
    const selectedShapes = useMemo(() => {
        const kinds = new Set<'codex' | 'claude' | 'generic'>();
        for (const e of endpoints) kinds.add(ENDPOINT_IDENTITY[e]);
        return kinds;
    }, [endpoints]);

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="max-h-[90vh] w-[calc(100vw-1rem)] max-w-3xl overflow-hidden rounded-3xl p-0">
                <DialogHeader className="border-b border-border/70 px-5 py-4">
                    <DialogTitle className="flex min-w-0 items-center gap-2">
                        <span className="shrink-0">测试渠道</span>
                        <span className="min-w-0 truncate font-mono text-sm text-muted-foreground">#{channel.id} · {channel.name}</span>
                    </DialogTitle>
                </DialogHeader>

                <div className="max-h-[74vh] space-y-4 overflow-y-auto px-5 py-4">
                    {/* 端点多选 */}
                    <div className="space-y-1.5">
                        <div className="text-xs font-bold tracking-[0.14em] text-muted-foreground">测试端点（多选）</div>
                        <div className="flex flex-wrap gap-2">
                            {MODEL_TEST_ENDPOINTS.map((e) => (
                                <button key={e.value} type="button" className={pillClass(endpoints.has(e.value))} onClick={() => toggleEndpoint(e.value)}>
                                    {e.label}
                                </button>
                            ))}
                        </div>
                    </div>

                    {/* 身份 / Shape（只读，端点自动配 shape · 用渠道真实 profile 不可覆盖） */}
                    {endpoints.size > 0 && (
                        <div className="space-y-1.5">
                            <div className="flex flex-wrap items-center gap-2 text-xs font-bold tracking-[0.14em] text-muted-foreground">
                                <Fingerprint className="size-3.5" />身份 / SHAPE
                                <span className="inline-flex items-center gap-1 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium tracking-normal text-muted-foreground">
                                    <Lock className="size-2.5" />渠道真实 profile · 不可覆盖
                                </span>
                            </div>
                            <div className="space-y-1 rounded-xl border border-border/60 bg-muted/10 p-2.5">
                                {identityQuery.isLoading ? (
                                    <span className="text-xs text-muted-foreground">解析身份中…</span>
                                ) : identity ? (
                                    <>
                                        {(['codex', 'claude', 'generic'] as const)
                                            .filter((k) => selectedShapes.has(k))
                                            .map((k) => {
                                                const ep: EndpointIdentity = identity[k];
                                                return (
                                                    <div key={k} className="flex items-center gap-2">
                                                        <span className={cn('shrink-0 rounded px-1.5 py-0.5 font-mono text-[10px] font-semibold', SHAPE_CLASS[k])}>{SHAPE_LABEL[k]}</span>
                                                        <span className="min-w-0 flex-1 truncate font-mono text-[10.5px] text-muted-foreground" title={ep.detail ? `${ep.user_agent}\n${ep.detail}` : ep.user_agent}>
                                                            {ep.user_agent || '(默认)'}
                                                        </span>
                                                    </div>
                                                );
                                            })}
                                        <div className="pt-0.5 text-[10.5px] text-muted-foreground">
                                            按端点自动取{identity.profile_name ? <> profile <span className="font-mono text-foreground">{identity.profile_name}</span></> : ' 全局默认身份'}的对应字段 · 与真实流量一字节一致
                                        </div>
                                    </>
                                ) : (
                                    <span className="text-xs text-muted-foreground">身份解析失败，测试仍按渠道真实 profile 进行</span>
                                )}
                            </div>
                        </div>
                    )}

                    {/* 流/非流 */}
                    <label className={cn('flex items-center gap-2 text-sm', streamShapeLocked ? 'cursor-not-allowed' : 'cursor-pointer')}>
                        <Switch
                            checked={streamShapeLocked || streamTest}
                            onCheckedChange={setStreamTest}
                            disabled={streamShapeLocked}
                            className="scale-90"
                        />
                        <span>流式优先</span>
                        {streamShapeLocked ? (
                            <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                                <Lock className="size-3" />codex / claude 恒流式（shape · 与真实流量一致，不可关）
                            </span>
                        ) : (
                            <span className="text-xs text-muted-foreground">（chat / gemini 真开真关；关掉后思考模型可能因非流超时，日志会如实标注）</span>
                        )}
                    </label>

                    {/* 模型多选 */}
                    <div className="space-y-1.5">
                        <div className="flex items-center justify-between gap-2">
                            <span className="text-xs font-bold tracking-[0.14em] text-muted-foreground">模型（{models.size}/{allModels.length}）</span>
                            <Button type="button" variant="outline" size="sm" className="h-6 rounded-lg px-2 text-xs" onClick={toggleAllModels} disabled={filteredModels.length === 0}>
                                {allFilteredSelected ? '全不选' : '全选'}
                            </Button>
                        </div>
                        <Input value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="过滤模型…" className="h-8 text-xs" />
                        <div className="flex max-h-40 flex-wrap gap-1.5 overflow-y-auto rounded-xl border border-border/60 bg-muted/10 p-2">
                            {filteredModels.length === 0 ? (
                                <span className="p-2 text-xs text-muted-foreground">该渠道没有可测模型</span>
                            ) : (
                                filteredModels.map((m) => (
                                    <button key={m} type="button" className={pillClass(models.has(m))} onClick={() => toggleModel(m)}>
                                        {cleanOneMillionModelName(m)}
                                    </button>
                                ))
                            )}
                        </div>
                    </div>

                    {/* 重复次数 */}
                    <div className="flex flex-wrap items-center gap-2 text-sm">
                        <RotateCw className="size-4 shrink-0 text-muted-foreground" />
                        <span className="shrink-0 text-xs font-medium">重复次数</span>
                        <Input
                            type="number"
                            min={1}
                            max={MAX_REPEAT}
                            value={repeatInput}
                            onChange={(event) => setRepeatInput(event.target.value)}
                            onBlur={() => setRepeatInput(String(repeat))}
                            className="h-8 w-20 text-center text-xs"
                        />
                        <span className="text-xs text-muted-foreground">
                            {repeat > 1 ? `汇总模式：跑 ${repeat} 轮 · 不逐条写日志 · 看成功率` : `默认 1 次（照常写日志）· 填 99 做容量/稳定性压测`}
                        </span>
                    </div>

                    {/* 运行 */}
                    <Button type="button" className="w-full rounded-xl" onClick={runTest} disabled={testing || endpoints.size === 0 || models.size === 0}>
                        {testing ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
                        测试选中（{endpoints.size} 端点 × {models.size} 模型{repeat > 1 ? ` × ${repeat} 轮` : ''}）
                    </Button>

                    {/* 汇总结果（重复次数 > 1） */}
                    {summaryRows.length > 0 && (
                        <div className="overflow-hidden rounded-xl border border-border/60">
                            <Table>
                                <TableHeader>
                                    <TableRow>
                                        <TableHead className="w-24">端点</TableHead>
                                        <TableHead>模型</TableHead>
                                        <TableHead className="w-28">成功率</TableHead>
                                        <TableHead>失败分类</TableHead>
                                    </TableRow>
                                </TableHeader>
                                <TableBody>
                                    {summaryRows.map(({ endpoint, model, s }) => {
                                        const rate = s.total > 0 ? Math.round((s.ok / s.total) * 100) : 0;
                                        const avg = s.ok > 0 ? (s.msSum / s.ok / 1000).toFixed(2) : null;
                                        const rateColor = rate >= 90 ? 'text-emerald-600 dark:text-emerald-400' : rate >= 50 ? 'text-amber-600 dark:text-amber-400' : 'text-destructive';
                                        const barColor = rate >= 90 ? 'bg-emerald-500' : rate >= 50 ? 'bg-amber-500' : 'bg-destructive';
                                        return (
                                            <TableRow key={`sum|${endpoint}|${model}`}>
                                                <TableCell className="text-xs text-muted-foreground">{MODEL_TEST_ENDPOINT_LABELS[endpoint]}</TableCell>
                                                <TableCell className="max-w-[15rem] break-all align-top font-mono text-xs">{cleanOneMillionModelName(model)}</TableCell>
                                                <TableCell>
                                                    <div className="flex items-center gap-1.5">
                                                        <span className={cn('text-xs font-semibold', rateColor)}>{s.ok}/{s.total}</span>
                                                        {avg && <span className="text-[10px] text-muted-foreground">·{avg}s</span>}
                                                    </div>
                                                    <div className="mt-1 h-1 w-full overflow-hidden rounded-full bg-muted">
                                                        <div className={cn('h-full transition-all', barColor)} style={{ width: `${rate}%` }} />
                                                    </div>
                                                </TableCell>
                                                <TableCell className="align-top text-xs text-muted-foreground">
                                                    {s.done < s.total ? (
                                                        <span className="inline-flex items-center gap-1"><Loader2 className="size-3 animate-spin" />{s.done}/{s.total}</span>
                                                    ) : (s.capacity + s.timeout + s.other) === 0 ? (
                                                        <span className="font-semibold text-emerald-600 dark:text-emerald-400">全通过</span>
                                                    ) : (
                                                        <div className="space-y-1.5">
                                                            {s.capacity > 0 && (
                                                                <div>
                                                                    <span className="text-amber-600 dark:text-amber-400">容量满 {s.capacity}</span>
                                                                    {s.capacityError && (
                                                                        <div className="mt-0.5 whitespace-pre-wrap break-words text-[10.5px] leading-snug text-muted-foreground/80" title={s.capacityError}>{s.capacityError}</div>
                                                                    )}
                                                                </div>
                                                            )}
                                                            {s.timeout > 0 && (
                                                                <div>
                                                                    <span className="text-destructive">超时 {s.timeout}</span>
                                                                    {s.timeoutError && (
                                                                        <div className="mt-0.5 whitespace-pre-wrap break-words text-[10.5px] leading-snug text-muted-foreground/80" title={s.timeoutError}>{s.timeoutError}</div>
                                                                    )}
                                                                </div>
                                                            )}
                                                            {s.other > 0 && (
                                                                <div>
                                                                    <span>其它 {s.other}</span>
                                                                    {s.otherError && (
                                                                        <div className="mt-0.5 whitespace-pre-wrap break-words text-[10.5px] leading-snug text-muted-foreground/80" title={s.otherError}>{s.otherError}</div>
                                                                    )}
                                                                </div>
                                                            )}
                                                        </div>
                                                    )}
                                                </TableCell>
                                            </TableRow>
                                        );
                                    })}
                                </TableBody>
                            </Table>
                            {summaryRows.some((r) => r.s.capacity > 0) && (
                                <div className="border-t border-border/50 px-3 py-2 text-[10.5px] leading-relaxed text-muted-foreground">
                                    「容量满」= 上游满载拒绝（上游坏窗口）。<b className="text-foreground">这是真实的失败次数、非假数据、也不是渠道故障</b>，稍后重试即可。
                                </div>
                            )}
                        </div>
                    )}

                    {/* 结果 */}
                    {resultRows.length > 0 && (
                        <div className="overflow-hidden rounded-xl border border-border/60">
                            <Table>
                                <TableHeader>
                                    <TableRow>
                                        <TableHead className="w-24">端点</TableHead>
                                        <TableHead>模型</TableHead>
                                        <TableHead className="w-20">状态</TableHead>
                                        <TableHead>结果</TableHead>
                                    </TableRow>
                                </TableHeader>
                                <TableBody>
                                    {resultRows.map(({ endpoint, model, result }) => (
                                        <TableRow key={`${endpoint}|${model}`}>
                                            <TableCell className="text-xs text-muted-foreground">{MODEL_TEST_ENDPOINT_LABELS[endpoint]}</TableCell>
                                            <TableCell className="max-w-[15rem] break-all align-top font-mono text-xs">{cleanOneMillionModelName(model)}</TableCell>
                                            <TableCell>
                                                {result.status === 'testing' ? (
                                                    <span className="inline-flex items-center gap-1 text-xs text-muted-foreground"><Loader2 className="size-3 animate-spin" />测试中</span>
                                                ) : result.status === 'success' ? (
                                                    <span className="inline-flex items-center gap-1 text-xs font-semibold text-emerald-600 dark:text-emerald-400"><CheckCircle2 className="size-3" />成功</span>
                                                ) : (
                                                    <span className="inline-flex items-center gap-1 text-xs font-semibold text-destructive"><XCircle className="size-3" />失败</span>
                                                )}
                                            </TableCell>
                                            <TableCell className="whitespace-pre-wrap break-words align-top text-xs text-muted-foreground" title={result.error}>
                                                {result.status === 'success' ? (result.ms != null ? `${(result.ms / 1000).toFixed(2)}s` : '-') : (result.error || '-')}
                                            </TableCell>
                                        </TableRow>
                                    ))}
                                </TableBody>
                            </Table>
                        </div>
                    )}
                </div>
            </DialogContent>
        </Dialog>
    );
}
