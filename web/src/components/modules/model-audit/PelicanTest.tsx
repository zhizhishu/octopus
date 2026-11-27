'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { Code2, Download, Loader2, Maximize2, Sparkles, Star, Trash2 } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { SearchableSelect } from '@/components/ui/searchable-select';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import type { Channel } from '@/api/endpoints/channel';
import {
    fetchCreativePNG,
    uploadCreativePNG,
    useAuditBudget,
    useClearUnpinnedCreatives,
    useCreateCreativeRound,
    useCreativeRound,
    useCreativeRounds,
    useDeleteCreative,
    usePinCreative,
    type CreativeItem,
    type CreativeKind,
} from '@/api/endpoints/model-audit';
import { getSelectedChannelModels } from '@/components/modules/channel/channel-utils';
import { MultiPicker, type MultiPickerOption } from './MultiPicker';
import { PELICAN_PROMPT, creativeFailureText, creativeHasPNG, creativeMeta, creativePNGCandidate, creativePNGFileName, creativeSrcDoc } from './v6';
import { rasterizeCreativeSVG } from './creative-png';

/**
 * 鹈鹕测试：同一道创作题发给几个模型，各交一份 SVG/HTML，并排看谁画得像样。
 *
 * - 每个模型只发 1 次，计入每日检测额度（后端入队时先扣）。
 * - 图放在 `sandbox="allow-scripts"` 的隔离框里实时显示：SVG 自带的动画会动，
 *   但**不给 allow-same-origin**，框里的脚本碰不到本站的登录和接口；再加一条
 *   只允许内联资源的 CSP，连不了外网（见 v6.creativeSrcDoc）。
 * - 7 天后自动删除，收藏的保留；也可以单张删、一键清空未收藏。
 * - SVG 出图后会自动存一张 PNG（在隔离框内 canvas 栅格化、postMessage 回传，
 *   主页面不直接加载模型 SVG，随图同生同死，见 creative-png.ts）；整页 HTML 没有截图库，暂不自动存，放大框里有说明。
 */

const MAX_MODELS = 12;
const PROMPT_LIMIT = 4000;

const PRESETS: { kind: CreativeKind; label: string; prompt: string }[] = [
    { kind: 'pelican', label: '鹈鹕骑车', prompt: PELICAN_PROMPT },
    {
        kind: 'html',
        label: 'HTML 小页面',
        prompt: 'Create a single self-contained HTML page (inline CSS and JavaScript, no external files or network requests) that shows an animated pelican riding a bicycle. Reply with the complete HTML document in one code block.',
    },
    { kind: 'custom', label: '+ 自己写题', prompt: '' },
];

const SEP = '::';

function isChatChannel(channel: Channel): boolean {
    // 4 = 退役位，5 = 嵌入，7 = 决策模型：这几类回不了一段文字。
    return ![4, 5, 7].includes(channel.type);
}

function roundLabel(createdAt: string, total: number): string {
    const d = new Date(createdAt);
    if (Number.isNaN(d.getTime())) return `${total} 个模型`;
    const now = new Date();
    const time = d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    const day = d.toDateString() === now.toDateString()
        ? '今天'
        : `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`;
    return `${day} ${time} 这一轮 · ${total} 个模型`;
}

function CreativeFrame({ item, className }: { item: CreativeItem; className?: string }) {
    return (
        <iframe
            title={`${item.channel_name} · ${item.model}`}
            // 只给 allow-scripts：动画能动，但没有同源权限，碰不到本站的登录态和接口。
            sandbox="allow-scripts"
            srcDoc={creativeSrcDoc(item)}
            loading="lazy"
            referrerPolicy="no-referrer"
            className={cn('block h-full w-full border-0 bg-white', className)}
        />
    );
}

function CreativeCard({
    item,
    onZoom,
    onSource,
    onPin,
    onDelete,
    onDownloadPNG,
    busy,
}: {
    item: CreativeItem;
    onZoom: () => void;
    onSource: () => void;
    onPin: () => void;
    onDelete: () => void;
    onDownloadPNG: () => void;
    busy: boolean;
}) {
    const [confirming, setConfirming] = useState(false);
    const ok = item.status === 'done' && item.content !== '';
    const active = item.status === 'pending' || item.status === 'running';
    return (
        <article className="flex min-w-0 flex-col overflow-hidden rounded-3xl border border-border bg-card">
            <div className="relative aspect-[4/3] w-full bg-muted/30">
                {ok ? (
                    <>
                        <CreativeFrame item={item} />
                        {/* 透明遮罩接住点击：框里的脚本收不到这次点击，点哪都是放大。 */}
                        <button
                            type="button"
                            onClick={onZoom}
                            aria-label="放大看"
                            className="absolute inset-0 cursor-zoom-in bg-transparent"
                        />
                    </>
                ) : (
                    <div className="flex h-full flex-col items-center justify-center gap-1 px-4 text-center text-sm text-muted-foreground">
                        {active && <Loader2 className="mb-1 size-5 animate-spin text-primary" />}
                        <p>{creativeFailureText(item)}</p>
                        {!active && item.raw && (
                            <button type="button" onClick={onSource} className="text-xs font-medium text-primary hover:underline">
                                看原文 ›
                            </button>
                        )}
                    </div>
                )}
            </div>
            <div className="flex flex-1 flex-col gap-1 border-t border-border px-4 py-3">
                <p className="min-w-0 text-sm font-semibold [overflow-wrap:anywhere]">
                    {item.channel_name} · {item.model}
                </p>
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                    <span className="tabular-nums">{creativeMeta(item) || (active ? '' : '—')}</span>
                    <span className="ml-auto flex items-center gap-1">
                        {ok && (
                            <>
                                <button type="button" onClick={onZoom} title="放大看" className="rounded-lg p-1 hover:bg-muted">
                                    <Maximize2 className="size-3.5" />
                                </button>
                                <button type="button" onClick={onSource} title="看源码" className="rounded-lg p-1 hover:bg-muted">
                                    <Code2 className="size-3.5" />
                                </button>
                                {creativeHasPNG(item) && (
                                    <button
                                        type="button"
                                        onClick={onDownloadPNG}
                                        title="下载自动存的 PNG"
                                        aria-label="下载 PNG"
                                        className="rounded-lg p-1 hover:bg-muted"
                                    >
                                        <Download className="size-3.5" />
                                    </button>
                                )}
                            </>
                        )}
                        {!active && (
                            <button
                                type="button"
                                onClick={onPin}
                                disabled={busy}
                                className={cn('flex items-center gap-0.5 rounded-lg px-1.5 py-1 font-medium hover:bg-muted', item.pinned ? 'text-primary' : 'text-primary/80')}
                            >
                                <Star className={cn('size-3.5', item.pinned && 'fill-current')} />
                                {item.pinned ? '已收藏' : '收藏'}
                            </button>
                        )}
                        {item.status !== 'running' && (
                            <button
                                type="button"
                                onClick={() => setConfirming(true)}
                                disabled={busy}
                                title="删掉这张"
                                aria-label="删掉这张"
                                className="rounded-lg p-1 hover:bg-destructive/10 hover:text-destructive"
                            >
                                <Trash2 className="size-3.5" />
                            </button>
                        )}
                    </span>
                </div>
                {confirming && (
                    <div role="alertdialog" className="mt-1 flex flex-wrap items-center gap-2 rounded-xl bg-destructive/10 px-2 py-1.5 text-xs text-destructive">
                        <span className="min-w-0 flex-1">{item.pinned ? '这张已收藏，确定删掉？' : '删掉这张？'}</span>
                        <Button size="sm" variant="destructive" className="h-6 rounded-lg px-2 text-[11px]" onClick={() => { setConfirming(false); onDelete(); }}>
                            删除
                        </Button>
                        <Button size="sm" variant="ghost" className="h-6 rounded-lg px-2 text-[11px]" onClick={() => setConfirming(false)}>
                            算了
                        </Button>
                    </div>
                )}
            </div>
        </article>
    );
}

export function PelicanTest({ channels }: { channels: Channel[] }) {
    const [kind, setKind] = useState<CreativeKind>('pelican');
    const [customPrompt, setCustomPrompt] = useState('');
    const [targets, setTargets] = useState<string[]>([]);
    const [roundId, setRoundId] = useState('');
    const [zoom, setZoom] = useState<CreativeItem | null>(null);
    const [source, setSource] = useState<CreativeItem | null>(null);
    const [confirmClear, setConfirmClear] = useState(false);

    const { data: rounds, isError: roundsError, refetch: refetchRounds } = useCreativeRounds(30);
    const { data: budget } = useAuditBudget();
    const createRound = useCreateCreativeRound();
    const pin = usePinCreative();
    const remove = useDeleteCreative();
    const clearUnpinned = useClearUnpinnedCreatives();

    // 没选轮次时看最新一轮。
    const activeRoundId = roundId || rounds?.[0]?.round_id || '';
    const { data: items, isLoading: itemsLoading, isError: itemsError, refetch: refetchItems } = useCreativeRound(activeRoundId);
    const round = rounds?.find((item) => item.round_id === activeRoundId);
    // 读取失败（不是数据为空）：要说「没读到」，不能让用户以为图被删了。
    const readFailed = roundsError || itemsError;
    const retryRead = () => {
        if (roundsError) void refetchRounds();
        if (itemsError) void refetchItems();
    };

    // 自动存 PNG（只对 SVG）：列表里出现一张「出完的 SVG 且还没存过 PNG」的图，就在
    // 父页面把它栅格化成 PNG 上传一次（见 creative-png.ts 的说明：隔离框没有
    // allow-same-origin，读不到框里的像素，栅格化用的是同一段 SVG 源文字）。
    // 防重复：服务端 png_size>0 就不传，本会话内传过/失败的也只试一次；
    // 失败（SVG 解不开、网络错）静默放弃，不阻塞看图、不弹窗。
    const pngInFlight = useRef<Set<number>>(new Set());
    const pngDone = useRef<Set<number>>(new Set());
    const pngFailed = useRef<Set<number>>(new Set());
    useEffect(() => {
        if (!items) return;
        for (const item of items) {
            if (!creativePNGCandidate(item)) continue;
            if (pngDone.current.has(item.id) || pngInFlight.current.has(item.id) || pngFailed.current.has(item.id)) continue;
            pngInFlight.current.add(item.id);
            void (async () => {
                try {
                    const blob = await rasterizeCreativeSVG(item.content);
                    if (!blob) {
                        pngFailed.current.add(item.id);
                        return;
                    }
                    const res = await uploadCreativePNG(item.id, blob);
                    pngDone.current.add(item.id);
                    if (res.stored) {
                        // 存上了就刷新这一轮：png_size 更新后下载按钮出现，也不会再传。
                        void refetchItems();
                    }
                } catch {
                    pngFailed.current.add(item.id);
                } finally {
                    pngInFlight.current.delete(item.id);
                }
            })();
        }
    }, [items, activeRoundId, refetchItems]);

    // 下载自动存的 PNG：带鉴权拉字节再转 object URL（套路同日志另存图），
    // 文件名跟后端 Content-Disposition 一致。
    const downloadPNG = async (item: CreativeItem) => {
        try {
            const blob = await fetchCreativePNG(item.id);
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = creativePNGFileName(item);
            document.body.appendChild(a);
            a.click();
            a.remove();
            URL.revokeObjectURL(url);
        } catch (error) {
            toast.error('PNG 没取到', { description: error instanceof Error ? error.message : String(error) });
        }
    };

    const options = useMemo<MultiPickerOption[]>(
        () =>
            channels
                .filter((channel) => channel.enabled && isChatChannel(channel))
                .flatMap((channel) =>
                    getSelectedChannelModels(channel).map((model) => ({
                        value: `${channel.id}${SEP}${model}`,
                        label: `${channel.name} · ${model}`,
                        keywords: `#${channel.id} ${channel.name} ${model}`,
                    })),
                ),
        [channels],
    );
    // 渠道被删/停用后，已选里对不上的项不算数，免得按钮上的次数对不上。
    const validTargets = useMemo(() => {
        const valid = new Set(options.map((option) => option.value));
        return targets.filter((target) => valid.has(target));
    }, [options, targets]);

    const preset = PRESETS.find((item) => item.kind === kind) ?? PRESETS[0];
    const prompt = kind === 'custom' ? customPrompt : preset.prompt;
    const count = validTargets.length;
    const remaining = budget ? budget.remaining : null;
    const overBudget = remaining !== null && count > remaining;
    const disabledReason =
        count === 0
            ? '先选要出图的模型'
            : kind === 'custom' && customPrompt.trim() === ''
              ? '先写好题目'
              : overBudget
                ? `今天的检测额度只剩 ${remaining} 次，不够这一轮的 ${count} 次`
                : '';

    const start = () => {
        if (disabledReason) {
            toast.warning(disabledReason);
            return;
        }
        createRound.mutate(
            {
                kind,
                prompt: kind === 'custom' ? customPrompt.trim() : undefined,
                targets: validTargets.map((target) => {
                    const at = target.indexOf(SEP);
                    return { channel_id: Number(target.slice(0, at)), model: target.slice(at + SEP.length) };
                }),
            },
            {
                onSuccess: (res) => {
                    setRoundId(res.round_id);
                    toast.success(`已发出 ${count} 个模型，出完会自己显示在下面`);
                },
                onError: (error) => toast.error('没发出去', { description: error instanceof Error ? error.message : String(error) }),
            },
        );
    };

    const togglePin = (item: CreativeItem) =>
        pin.mutate(
            { id: item.id, pinned: !item.pinned },
            { onError: (error) => toast.error('收藏没改成', { description: error instanceof Error ? error.message : String(error) }) },
        );
    const deleteItem = (item: CreativeItem) =>
        remove.mutate(item.id, {
            onSuccess: () => toast.success('已删掉这张'),
            onError: (error) => toast.error('没删掉', { description: error instanceof Error ? error.message : String(error) }),
        });
    const runClear = () => {
        setConfirmClear(false);
        clearUnpinned.mutate(undefined, {
            onSuccess: (res) => {
                toast.success(res.deleted > 0 ? `清掉了 ${res.deleted} 张没收藏的图` : '没有可以清的图');
                setRoundId('');
            },
            onError: (error) => toast.error('没清成', { description: error instanceof Error ? error.message : String(error) }),
        });
    };

    const busy = pin.isPending || remove.isPending || clearUnpinned.isPending;

    return (
        <div className="min-w-0 space-y-4">
            <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                <div className="flex flex-col gap-4 lg:flex-row lg:items-start">
                    <div className="min-w-0 flex-1 space-y-2">
                        <div className="flex flex-wrap items-center gap-2">
                            <span className="text-xs text-muted-foreground">题目</span>
                            {PRESETS.map((item) => (
                                <button
                                    key={item.kind}
                                    type="button"
                                    aria-pressed={kind === item.kind}
                                    onClick={() => setKind(item.kind)}
                                    className={cn(
                                        'rounded-full border px-3 py-1 text-xs transition-colors',
                                        kind === item.kind ? 'border-primary bg-primary/10 font-semibold text-primary' : 'border-border text-muted-foreground hover:bg-muted',
                                    )}
                                >
                                    {item.label}
                                </button>
                            ))}
                        </div>
                        {kind === 'custom' ? (
                            <div>
                                <textarea
                                    value={customPrompt}
                                    onChange={(event) => setCustomPrompt(event.target.value.slice(0, PROMPT_LIMIT))}
                                    maxLength={PROMPT_LIMIT}
                                    rows={3}
                                    placeholder="比如：Generate an SVG of a cat playing piano（让它交 SVG 或 HTML 才能在框里显示）"
                                    className="w-full resize-y rounded-xl border border-border bg-muted/30 px-3 py-2 font-mono text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                />
                                <p className="text-right text-[11px] text-muted-foreground">{customPrompt.length} / {PROMPT_LIMIT}</p>
                            </div>
                        ) : (
                            <p className="rounded-xl bg-muted/50 px-3 py-2 font-mono text-sm [overflow-wrap:anywhere]">{prompt}</p>
                        )}
                    </div>
                    <div className="flex w-full flex-col gap-2 lg:w-72 lg:shrink-0">
                        <MultiPicker
                            value={validTargets}
                            onChange={setTargets}
                            options={options}
                            max={MAX_MODELS}
                            placeholder={options.length === 0 ? '没有可用的聊天渠道' : '选要出图的模型'}
                            summary={count > 0 ? `已选 ${count} 个模型` : undefined}
                            searchPlaceholder="搜渠道或模型…"
                            disabled={options.length === 0}
                            ariaLabel="选要出图的模型"
                            className="w-full"
                        />
                        <Button
                            type="button"
                            className="h-11 w-full rounded-xl text-base font-semibold"
                            disabled={!!disabledReason || createRound.isPending}
                            onClick={start}
                            title={disabledReason || undefined}
                        >
                            {createRound.isPending ? <Loader2 className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
                            一起出图 · {count} 次请求
                        </Button>
                        {disabledReason && count > 0 && <p className="text-xs text-amber-700 dark:text-amber-400">{disabledReason}</p>}
                    </div>
                </div>
                <p className="mt-3 text-xs text-muted-foreground">
                    每个模型只发 1 次 · 在隔离框里显示 · <span className="font-medium">7 天后自动删除，收藏的保留</span>
                    {budget && <> · 今天检测额度还剩 {budget.remaining} / {budget.limit} 次</>}
                </p>
            </section>

            <div className="flex flex-wrap items-center gap-2 px-1">
                <h3 className="min-w-0 flex-1 text-base font-semibold">
                    {round ? roundLabel(round.created_at, round.total) : readFailed ? '读取失败' : '还没有出过图'}
                </h3>
                {(rounds?.length ?? 0) > 0 && (
                    <SearchableSelect
                        value={activeRoundId}
                        onValueChange={setRoundId}
                        options={(rounds ?? []).map((item) => ({
                            value: item.round_id,
                            label: `${roundLabel(item.created_at, item.total)}${item.pinned > 0 ? ` · ★${item.pinned}` : ''}`,
                            keywords: item.prompt,
                        }))}
                        placeholder="历史轮次"
                        searchPlaceholder="搜题目…"
                        emptyText="没有匹配的轮次"
                        className="h-8 w-56 text-xs"
                    />
                )}
                <Button variant="ghost" size="sm" className="h-8 rounded-lg text-xs text-muted-foreground hover:text-destructive" disabled={busy || (rounds?.length ?? 0) === 0} onClick={() => setConfirmClear(true)}>
                    清空未收藏
                </Button>
            </div>
            {confirmClear && (
                <div role="alertdialog" className="flex flex-wrap items-center gap-2 rounded-xl bg-destructive/10 px-3 py-2 text-xs text-destructive">
                    <span className="min-w-0 flex-1">删掉所有没收藏的图（所有轮次）？收藏的和还在出的不动。</span>
                    <Button size="sm" variant="destructive" className="h-7 rounded-lg text-[11px]" onClick={runClear}>确定清空</Button>
                    <Button size="sm" variant="ghost" className="h-7 rounded-lg text-[11px]" onClick={() => setConfirmClear(false)}>算了</Button>
                </div>
            )}

            {round && round.prompt && (
                <p className="px-1 font-mono text-xs text-muted-foreground [overflow-wrap:anywhere]">题目：{round.prompt}</p>
            )}

            {readFailed ? (
                <div className="rounded-3xl border border-dashed border-destructive/40 bg-destructive/5 p-8 text-center text-sm">
                    <p className="font-medium text-destructive">读取失败</p>
                    <p className="mt-1 text-xs text-muted-foreground">没读到这一轮（网络或服务端出错），不是图被删了。</p>
                    <Button type="button" variant="outline" size="sm" className="mt-3 h-8 rounded-lg text-xs" onClick={retryRead}>
                        重试
                    </Button>
                </div>
            ) : activeRoundId === '' ? (
                <div className="rounded-3xl border border-dashed border-border/70 bg-muted/20 p-8 text-center text-sm text-muted-foreground">
                    选几个模型，点「一起出图」，它们画的鹈鹕会并排出现在这里。
                </div>
            ) : itemsLoading && !items ? (
                <p className="px-1 text-sm text-muted-foreground">正在读这一轮…</p>
            ) : (items ?? []).length === 0 ? (
                <div className="rounded-3xl border border-dashed border-border/70 bg-muted/20 p-8 text-center text-sm text-muted-foreground">
                    这一轮的图都删掉了。
                </div>
            ) : (
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
                    {(items ?? []).map((item) => (
                        <CreativeCard
                            key={item.id}
                            item={item}
                            busy={busy}
                            onZoom={() => setZoom(item)}
                            onSource={() => setSource(item)}
                            onPin={() => togglePin(item)}
                            onDelete={() => deleteItem(item)}
                            onDownloadPNG={() => void downloadPNG(item)}
                        />
                    ))}
                </div>
            )}

            <Dialog open={!!zoom} onOpenChange={(open) => { if (!open) setZoom(null); }}>
                <DialogContent className="flex h-[90vh] w-[95vw] max-w-[95vw] flex-col sm:max-w-[95vw]">
                    <DialogHeader>
                        <DialogTitle>{zoom ? `${zoom.channel_name} · ${zoom.model}` : ''}</DialogTitle>
                        <DialogDescription>
                            {zoom ? creativeMeta(zoom) : ''}
                            {zoom && zoom.status === 'done' && zoom.content !== '' && zoom.format === 'html' && (
                                <span className="ml-1">· 整页 HTML 暂不自动存 PNG（SVG 出图后会自动存）</span>
                            )}
                        </DialogDescription>
                        {zoom && zoom.status === 'done' && zoom.content !== '' && creativeHasPNG(zoom) && (
                            <Button
                                type="button"
                                size="sm"
                                variant="outline"
                                className="h-8 w-fit rounded-lg text-xs"
                                onClick={() => void downloadPNG(zoom)}
                            >
                                <Download className="size-3.5" />
                                下载 PNG
                            </Button>
                        )}
                    </DialogHeader>
                    <div className="min-h-0 flex-1 overflow-hidden rounded-xl border border-border">
                        {zoom && <CreativeFrame item={zoom} />}
                    </div>
                </DialogContent>
            </Dialog>

            <Dialog open={!!source} onOpenChange={(open) => { if (!open) setSource(null); }}>
                <DialogContent className="flex max-h-[90vh] w-[95vw] max-w-3xl flex-col">
                    <DialogHeader>
                        <DialogTitle>{source ? `${source.channel_name} · ${source.model}` : ''}</DialogTitle>
                        <DialogDescription>
                            {source?.content ? `提取出的 ${source.format === 'html' ? 'HTML' : 'SVG'}（原样显示，不执行）` : '模型的原文回复'}
                        </DialogDescription>
                    </DialogHeader>
                    <pre className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-all rounded-xl bg-muted/50 p-3 font-mono text-xs">
                        {source ? (source.content || source.raw || source.error || '（空）') : ''}
                    </pre>
                    {source?.content && source.raw && source.raw !== source.content && (
                        <details className="text-xs">
                            <summary className="cursor-pointer text-muted-foreground">看完整原文</summary>
                            <pre className="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-xl bg-muted/50 p-3 font-mono">{source.raw}</pre>
                        </details>
                    )}
                </DialogContent>
            </Dialog>
        </div>
    );
}
