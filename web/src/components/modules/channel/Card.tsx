import {
    MorphingDialog,
    MorphingDialogTrigger,
    MorphingDialogContainer,
    MorphingDialogContent,
} from '@/components/ui/morphing-dialog';
import { Activity, AlertTriangle, CheckCircle2, Copy, DollarSign, Key, Layers, Loader2, MessageSquare, Play, RotateCcw, XCircle, Server } from 'lucide-react';
import { type StatsMetricsFormatted } from '@/api/endpoints/stats';
import { type Channel, useCopyChannel, useEnableChannel, useResetChannelCircuit } from '@/api/endpoints/channel';
import { CardContent } from './CardContent';
import { useTranslations } from 'next-intl';
import { Tooltip, TooltipTrigger, TooltipContent } from '@/components/animate-ui/components/animate/tooltip';
import { toast } from '@/components/common/Toast';
import { Switch } from '@/components/ui/switch';
import { useMemo, useState, type MouseEvent } from 'react';
import { cn } from '@/lib/utils';
import { ChannelTestDialog } from './channel-test-dialog';
import {
    getChannelEndpointFamily,
    getPrimaryChannelModel,
    getSelectedChannelModels,
} from './channel-utils';

export function Card({ channel, stats, layout = 'list' }: { channel: Channel; stats: StatsMetricsFormatted; layout?: 'grid' | 'list' }) {
    const t = useTranslations('channel.card');
    const tMetrics = useTranslations('channel.detail.metrics');
    const enableChannel = useEnableChannel();
    const copyChannel = useCopyChannel();
    const resetCircuit = useResetChannelCircuit();
    const [testDialogOpen, setTestDialogOpen] = useState(false);
    const circuitLabel = channel.circuit_remaining_seconds > 0
        ? t('circuit.remaining', { seconds: channel.circuit_remaining_seconds })
        : t('circuit.open');

    const testModels = useMemo(() => getSelectedChannelModels(channel), [channel]);
    const modelCount = testModels.length;
    const firstModel = testModels[0] || '';
    const enabledKeyCount = channel.keys.filter((item) => item.enabled).length;
    const isGridLayout = layout === 'grid';
    const family = getChannelEndpointFamily(channel);
    const primaryModel = getPrimaryChannelModel(channel);
    const handleEnableChange = (checked: boolean) => {
        enableChannel.mutate(
            { id: channel.id, enabled: checked },
            {
                onSuccess: () => {
                    toast.success(checked ? t('toast.enabled') : t('toast.disabled'));
                },
                onError: (error) => {
                    toast.error(error.message);
                },
            }
        );
    };

    const handleCopyChannel = (event: MouseEvent<HTMLButtonElement>) => {
        event.preventDefault();
        event.stopPropagation();
        copyChannel.mutate(channel, {
            onSuccess: ({ newName }) => {
                toast.success(t('copySuccess', { name: newName }));
            },
            onError: (error) => {
                toast.error(error?.message || t('copyFailed'));
            },
        });
    };

    const handleResetCircuit = (event: MouseEvent<HTMLButtonElement>) => {
        event.preventDefault();
        event.stopPropagation();
        resetCircuit.mutate(
            { id: channel.id },
            {
                onSuccess: () => toast.success(t('toast.circuitReset')),
                onError: (error) => toast.error(error.message),
            }
        );
    };


    return (
        <MorphingDialog>
            <MorphingDialogTrigger className="w-full">
                <article className={cn(
                    'rounded-lg border bg-card text-card-foreground transition-colors hover:bg-muted/30',
                    channel.circuit_tripped ? 'border-destructive/70' : 'border-border',
                    isGridLayout
                        ? 'flex min-h-[232px] flex-col gap-3 p-4'
                        : 'grid min-h-[84px] grid-cols-1 items-center gap-3 px-3 py-2 md:grid-cols-[minmax(0,1.4fr)_minmax(0,2fr)_auto] md:px-4'
                )}>
                    <div className="flex min-w-0 items-center gap-3">
                        <span className={cn(
                            'flex h-9 w-9 shrink-0 items-center justify-center rounded-md',
                            channel.enabled ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400' : 'bg-muted text-muted-foreground'
                        )}>
                            <Server className="size-4" />
                        </span>
                        <div className="min-w-0">
                            <div className="flex min-w-0 items-center gap-2">
                                <span className={cn('h-2 w-2 shrink-0 rounded-full', channel.enabled ? 'bg-emerald-500' : 'bg-muted-foreground/50')} />
                                <Tooltip side="top" sideOffset={10} align="center">
                                    <TooltipTrigger asChild>
                                        <h3 className="min-w-0 truncate text-sm font-semibold md:text-base">{channel.name}</h3>
                                    </TooltipTrigger>
                                    <TooltipContent key={channel.name}>{channel.name}</TooltipContent>
                                </Tooltip>
                            </div>
                            <div className="mt-1 flex min-w-0 flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                                <span>#{channel.id}</span>
                                <span>P{channel.priority ?? 0}</span>
                                <span>{family.shortLabel}</span>
                                {primaryModel && <span className={cn('truncate', isGridLayout ? 'max-w-full' : 'max-w-[12rem]')}>{primaryModel}</span>}
                            </div>
                        </div>
                    </div>

                    <dl className={cn(
                        'grid min-w-0 gap-2',
                        isGridLayout ? 'grid-cols-2' : 'grid-cols-3 lg:grid-cols-6'
                    )}>
                        <div className="min-w-0">
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <MessageSquare className="size-3.5 text-primary" />
                                {t('requestCount')}
                            </dt>
                            <dd className="truncate text-sm font-semibold">
                                {stats.request_count.formatted.value}
                                <span className="ml-1 text-xs text-muted-foreground">{stats.request_count.formatted.unit}</span>
                            </dd>
                        </div>
                        <div className="min-w-0">
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <Layers className="size-3.5 text-primary" />
                                模型
                            </dt>
                            <dd className="text-sm font-semibold">{modelCount}</dd>
                        </div>
                        <div className="min-w-0">
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <Key className="size-3.5 text-primary" />
                                Key
                            </dt>
                            <dd className="text-sm font-semibold">{enabledKeyCount}/{channel.keys.length}</dd>
                        </div>
                        <div className={cn('min-w-0', !isGridLayout && 'hidden lg:block')}>
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <CheckCircle2 className="size-3.5 text-emerald-500" />
                                {tMetrics('successRequests')}
                            </dt>
                            <dd className="text-sm font-semibold">
                                {stats.request_success.formatted.value}
                                <span className="ml-1 text-xs text-muted-foreground">{stats.request_success.formatted.unit}</span>
                            </dd>
                        </div>
                        <div className={cn('min-w-0', !isGridLayout && 'hidden lg:block')}>
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <XCircle className="size-3.5 text-destructive" />
                                {tMetrics('failedRequests')}
                            </dt>
                            <dd className="text-sm font-semibold">
                                {stats.request_failed.formatted.value}
                                <span className="ml-1 text-xs text-muted-foreground">{stats.request_failed.formatted.unit}</span>
                            </dd>
                        </div>
                        <div className={cn('min-w-0', !isGridLayout && 'hidden lg:block')}>
                            <dt className="flex items-center gap-1 text-[11px] text-muted-foreground">
                                <DollarSign className="size-3.5 text-primary" />
                                {t('totalCost')}
                            </dt>
                            <dd className="truncate text-sm font-semibold">
                                {stats.total_cost.formatted.value}
                                <span className="ml-1 text-xs text-muted-foreground">{stats.total_cost.formatted.unit}</span>
                            </dd>
                        </div>
                    </dl>

                    <div className={cn(
                        'flex shrink-0 flex-wrap items-center gap-2',
                        isGridLayout ? 'mt-auto justify-between border-t border-border/60 pt-3' : 'justify-start md:justify-end'
                    )}
                        onClick={(event) => event.stopPropagation()}
                        onPointerDown={(event) => event.stopPropagation()}
                    >
                        <Tooltip side="top" sideOffset={10} align="center">
                            <TooltipTrigger asChild>
                                <button
                                    type="button"
                                    className="inline-flex h-8 max-w-full items-center gap-1 overflow-hidden rounded-lg border border-border px-2 text-xs font-medium leading-none text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
                                    onClick={(event) => { event.preventDefault(); event.stopPropagation(); setTestDialogOpen(true); }}
                                    aria-label="测试模型"
                                >
                                    <Play className="size-3.5" />
                                    <span className="min-w-0 truncate">测试</span>
                                </button>
                            </TooltipTrigger>
                            <TooltipContent>{firstModel ? `测试 ${firstModel}` : '打开测试面板（该渠道当前没有已选模型）'}</TooltipContent>
                        </Tooltip>
                        <ChannelTestDialog channel={channel} open={testDialogOpen} onOpenChange={setTestDialogOpen} />
                        <Tooltip side="top" sideOffset={10} align="center">
                            <TooltipTrigger asChild>
                                <button
                                    type="button"
                                    className="inline-flex h-8 max-w-full items-center gap-1 overflow-hidden rounded-lg border border-border px-2 text-xs font-medium leading-none text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
                                    onClick={handleCopyChannel}
                                    disabled={copyChannel.isPending}
                                    aria-label={t('copy')}
                                >
                                    <Copy className="size-3.5" />
                                    <span className="min-w-0 truncate">{t('copy')}</span>
                                </button>
                            </TooltipTrigger>
                            <TooltipContent>{t('copyTooltip')}</TooltipContent>
                        </Tooltip>
                        {channel.circuit_tripped && (
                            <Tooltip side="top" sideOffset={10} align="center">
                                <TooltipTrigger asChild>
                                    <button
                                        type="button"
                                        className="inline-flex h-8 max-w-[13rem] items-center gap-1 overflow-hidden rounded-lg border border-destructive/40 px-2 text-xs font-medium leading-none text-destructive hover:bg-destructive/10 disabled:opacity-50"
                                        onClick={handleResetCircuit}
                                        disabled={resetCircuit.isPending}
                                        aria-label={t('circuit.reset')}
                                    >
                                        <AlertTriangle className="size-3.5" />
                                        <span className="min-w-0 truncate">{circuitLabel}</span>
                                        <RotateCcw className="size-3.5" />
                                    </button>
                                </TooltipTrigger>
                                <TooltipContent>{t('circuit.tooltip', { count: channel.circuit_open_keys })}</TooltipContent>
                            </Tooltip>
                        )}
                        <Tooltip side="top" sideOffset={10} align="center">
                            <TooltipTrigger asChild>
                                <div className="inline-flex h-8 max-w-full items-center gap-2 overflow-hidden rounded-lg border border-border bg-background px-2 text-xs leading-none text-muted-foreground">
                                    <Activity className={cn('size-3.5', channel.enabled ? 'text-emerald-500' : 'text-muted-foreground')} />
                                    <Switch
                                        checked={channel.enabled}
                                        onCheckedChange={handleEnableChange}
                                        disabled={enableChannel.isPending}
                                        aria-label="启用渠道"
                                    />
                                </div>
                            </TooltipTrigger>
                            <TooltipContent>{channel.enabled ? '禁用后会自动排到列表后面。' : '启用后会回到可用通道队列。'}</TooltipContent>
                        </Tooltip>
                    </div>
                </article>
            </MorphingDialogTrigger>

            <MorphingDialogContainer>
                <MorphingDialogContent className="w-full bg-card text-card-foreground px-4 py-3 rounded-3xl max-h-[92vh] overflow-y-auto md:max-w-6xl">
                    <CardContent channel={channel} stats={stats} />
                </MorphingDialogContent>
            </MorphingDialogContainer>
        </MorphingDialog>
    );
}
