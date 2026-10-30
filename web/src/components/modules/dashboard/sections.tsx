'use client';

import { useMemo, useState, type ReactNode } from 'react';
import { ArrowRight, ScrollText } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Area, AreaChart, CartesianGrid, Cell, Customized, Line, LineChart, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import type { StatsBreakdown, StatsBreakdownItem, StatsUserTrend } from '@/api/endpoints/dashboard';
import { cn } from '@/lib/utils';
import { CostBar, OTHERS_COLOR, Panel, SLICE_COLORS, Segmented } from './parts';
import { formatCountText, formatMoneyAxis, formatMoneyText, formatPercentText, shortDate } from './format';
import { fromLog, shareSlices, totalTokens, type TrendMetric, type TrendPoint } from './model';

export type ShareDimension = 'model' | 'channel' | 'plan';

function formatMetric(metric: TrendMetric, value: number | null | undefined): string {
    if (value === null || value === undefined) return '—';
    if (metric === 'cost') return formatMoneyText(value);
    if (metric === 'success') return `${value.toFixed(1)}%`;
    return formatCountText(value);
}

function formatMetricAxis(metric: TrendMetric, value: number): string {
    if (metric === 'cost') return formatMoneyAxis(value);
    if (metric === 'success') return `${Math.round(value)}%`;
    return formatCountText(value);
}

/* ---------------- 趋势 ---------------- */

export function TrendPanel({ points, metric, onMetric, currentLabel, previousLabel }: {
    points: TrendPoint[];
    metric: TrendMetric;
    onMetric: (m: TrendMetric) => void;
    currentLabel: string;
    previousLabel: string;
}) {
    const t = useTranslations('dashboard.trend');
    return (
        <Panel
            title={t('title')}
            tip={t('tip')}
            right={
                <Segmented
                    size="sm"
                    value={metric}
                    onChange={onMetric}
                    options={[
                        { value: 'cost', label: t('cost') },
                        { value: 'count', label: t('count') },
                        { value: 'tokens', label: t('tokens') },
                        { value: 'success', label: t('success'), className: 'hidden sm:block' },
                    ]}
                />
            }
        >
            <div className="mb-1 flex items-center gap-4 text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1.5"><span className="h-0.5 w-4 rounded bg-[var(--chart-1)]" />{currentLabel}</span>
                <span className="inline-flex items-center gap-1.5"><span className="w-4 border-t border-dashed border-muted-foreground" />{previousLabel}</span>
            </div>
            <div className="h-44 w-full md:h-52">
                <ResponsiveContainer width="100%" height="100%">
                    <AreaChart data={points} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
                        <defs>
                            <linearGradient id="dash-trend-fill" x1="0" y1="0" x2="0" y2="1">
                                <stop offset="5%" stopColor="var(--chart-1)" stopOpacity={0.45} />
                                <stop offset="95%" stopColor="var(--chart-1)" stopOpacity={0.03} />
                            </linearGradient>
                        </defs>
                        <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="var(--border)" />
                        {/* 按小时是 24 个点：每 3 小时一个刻度（0:00、3:00 …），和按天一样整齐。 */}
                        <XAxis dataKey="label" tickLine={false} axisLine={false} fontSize={11} interval={points.length === 24 ? 2 : 'preserveStartEnd'} minTickGap={12} />
                        <YAxis tickLine={false} axisLine={false} fontSize={11} width={44} tickFormatter={(v: number) => formatMetricAxis(metric, v)} />
                        <Tooltip
                            cursor={{ stroke: 'var(--border)' }}
                            contentStyle={{ borderRadius: 12, fontSize: 12, background: 'var(--popover)', border: '1px solid var(--border)' }}
                            formatter={(value: number, name: string) => [formatMetric(metric, value), name === 'current' ? currentLabel : previousLabel]}
                        />
                        <Area type="linear" dataKey="previous" stroke="var(--muted-foreground)" strokeDasharray="4 4" strokeWidth={1.25} fill="none" dot={false} connectNulls isAnimationActive={false} />
                        <Area type="linear" dataKey="current" stroke="var(--chart-1)" strokeWidth={2} fill="url(#dash-trend-fill)" dot={false} isAnimationActive={false} />
                    </AreaChart>
                </ResponsiveContainer>
            </div>
        </Panel>
    );
}

/* ---------------- 花在哪 ---------------- */

export function sliceName(item: { id: number; name: string }, dim: ShareDimension, t: (key: string) => string): string {
    if (dim === 'channel' && item.id === 0) return t('noChannel');
    if (dim === 'plan' && item.id === 0) return t('noPlan');
    return item.name || `#${item.id}`;
}

export function SharePanel({ data, dim, onDim, periodLabel, onOpen }: {
    data: StatsBreakdown | undefined;
    dim: ShareDimension;
    onDim: (d: ShareDimension) => void;
    periodLabel: string;
    onOpen: (item: StatsBreakdownItem) => void;
}) {
    const t = useTranslations('dashboard.share');
    const total = data?.total.total_cost ?? 0;
    const slices = useMemo(() => {
        if (!data) return [];
        const named = data.items.map((item) => ({ key: item.key, name: sliceName(item, dim, t), total_cost: item.total_cost }));
        return shareSlices(named, data.others, data.others_count, total);
    }, [data, dim, t, total]);
    const itemsByKey = useMemo(() => new Map((data?.items ?? []).map((item) => [item.key, item])), [data]);
    const clickable = (key: string) => {
        if (dim === 'plan') return false;
        const item = itemsByKey.get(key);
        if (!item) return false;
        return dim === 'model' || item.id > 0;
    };
    const color = (i: number, isOthers?: boolean) => (isOthers ? OTHERS_COLOR : SLICE_COLORS[i % SLICE_COLORS.length]);

    return (
        <Panel
            title={t('title')}
            tip={t('tip')}
            right={
                <Segmented
                    size="sm"
                    value={dim}
                    onChange={onDim}
                    options={[
                        { value: 'model', label: t('model') },
                        { value: 'channel', label: t('channel') },
                        { value: 'plan', label: t('plan') },
                    ]}
                />
            }
        >
            {total <= 0 ? (
                <div className="flex h-44 items-center justify-center text-sm text-muted-foreground">{t('empty')}</div>
            ) : (
                <>
                    {/* 手机：一条横条 + 两列图例 */}
                    <div className="md:hidden">
                        <div className="flex h-3 w-full overflow-hidden rounded-full">
                            {slices.map((s, i) => (
                                <div key={s.key} style={{ width: `${s.share * 100}%`, background: color(i, s.isOthers) }} />
                            ))}
                        </div>
                    </div>
                    <div className="flex min-w-0 items-center gap-3">
                        <div className="relative hidden size-36 shrink-0 md:block">
                            <ResponsiveContainer width="100%" height="100%">
                                <PieChart>
                                    <Pie data={slices} dataKey="cost" nameKey="name" innerRadius={46} outerRadius={70} paddingAngle={1.5} stroke="none" isAnimationActive={false}>
                                        {slices.map((s, i) => (
                                            <Cell key={s.key} fill={color(i, s.isOthers)} />
                                        ))}
                                    </Pie>
                                </PieChart>
                            </ResponsiveContainer>
                            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
                                <span className="text-sm font-semibold">{formatMoneyText(total)}</span>
                                <span className="text-[11px] text-muted-foreground">{t('center', { period: periodLabel })}</span>
                            </div>
                        </div>
                        <ul className="mt-3 grid min-w-0 flex-1 grid-cols-2 gap-x-4 gap-y-1 md:mt-0 md:grid-cols-1">
                            {slices.map((s, i) => {
                                const canOpen = !s.isOthers && clickable(s.key);
                                const name = s.isOthers ? t('others', { count: s.othersCount ?? 0 }) : s.name;
                                return (
                                    <li key={s.key} className="min-w-0">
                                        <button
                                            type="button"
                                            disabled={!canOpen}
                                            onClick={() => {
                                                const item = itemsByKey.get(s.key);
                                                if (item) onOpen(item);
                                            }}
                                            title={dim === 'plan' && !s.isOthers ? t('planNoLink') : name}
                                            className={cn('flex w-full min-w-0 items-center gap-1.5 rounded-lg px-1 py-1 text-left text-xs', canOpen ? 'hover:bg-muted/60' : 'cursor-default')}
                                        >
                                            <span className="size-2.5 shrink-0 rounded-full" style={{ background: color(i, s.isOthers) }} />
                                            <span className="min-w-0 flex-1 truncate">{name}</span>
                                            <span className="w-8 shrink-0 text-right tabular-nums text-muted-foreground">{formatPercentText(s.share, 0)}</span>
                                            <span className="hidden w-14 shrink-0 text-right tabular-nums md:inline">{formatMoneyText(s.cost)}</span>
                                        </button>
                                    </li>
                                );
                            })}
                        </ul>
                    </div>
                    <div className="mt-3 hidden items-center justify-between border-t border-border pt-3 text-xs text-muted-foreground md:flex">
                        <span>{dim === 'plan' ? t('planNoLink') : t('footer')}</span>
                        <ArrowRight className="size-3.5" />
                    </div>
                </>
            )}
        </Panel>
    );
}

/* ---------------- 两张排行 ---------------- */

function MissingBadge() {
    const t = useTranslations('dashboard.users');
    return <span className="ml-1 shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">{t('deleted')}</span>;
}

function userLabel(item: { id: number; name: string }, t: (key: string) => string) {
    if (item.id === 0) return t('noUser');
    return item.name || `#${item.id}`;
}

export function UserRankPanel({ data, onUser, onLogs, onGoto }: {
    data: StatsBreakdown | undefined;
    onUser: (id: number) => void;
    onLogs: (item: StatsBreakdownItem) => void;
    onGoto: () => void;
}) {
    const t = useTranslations('dashboard.users');
    const items = data?.items ?? [];
    const max = items.reduce((m, item) => Math.max(m, item.total_cost), 0);
    return (
        <Panel title={t('title')} tip={t('tip')} right={<GotoLink label={t('goto')} onClick={onGoto} />}>
            <RankTable
                firstHeader={t('user')}
                items={items}
                max={max}
                renderName={(item) => (
                    <button
                        type="button"
                        disabled={item.id === 0 || item.missing}
                        onClick={() => onUser(item.id)}
                        title={item.id > 0 ? t('openUser', { name: item.name }) : undefined}
                        className="flex min-w-0 items-center text-left hover:underline disabled:no-underline"
                    >
                        <span className="truncate">{userLabel(item, t)}</span>
                        {item.missing ? <MissingBadge /> : null}
                    </button>
                )}
                onLogs={onLogs}
            />
        </Panel>
    );
}

export function KeyRankPanel({ data, onUser, onLogs, onGoto }: {
    data: StatsBreakdown | undefined;
    onUser: (id: number) => void;
    onLogs: (item: StatsBreakdownItem) => void;
    onGoto: () => void;
}) {
    const t = useTranslations('dashboard.keys');
    const tu = useTranslations('dashboard.users');
    const items = data?.items ?? [];
    const max = items.reduce((m, item) => Math.max(m, item.total_cost), 0);
    return (
        <Panel title={t('title')} tip={t('tip')} right={<GotoLink label={t('goto')} onClick={onGoto} />}>
            <RankTable
                firstHeader={t('key')}
                items={items}
                max={max}
                renderName={(item) => (
                    <span className="flex min-w-0 items-baseline gap-1.5">
                        <button type="button" onClick={() => onLogs(item)} className="max-w-full shrink-0 truncate text-left hover:underline" title={tu('viewLogs')}>
                            {item.name || `#${item.id}`}
                        </button>
                        {item.missing ? <MissingBadge /> : null}
                        {item.user_id ? (
                            <button type="button" onClick={() => onUser(item.user_id!)} className="min-w-0 truncate text-xs text-muted-foreground hover:underline">
                                {item.user_name || `#${item.user_id}`}
                            </button>
                        ) : null}
                    </span>
                )}
                onLogs={onLogs}
            />
        </Panel>
    );
}

function GotoLink({ label, onClick }: { label: string; onClick: () => void }) {
    return (
        <button type="button" onClick={onClick} className="inline-flex shrink-0 items-center gap-1 text-xs text-muted-foreground hover:text-foreground">
            {label}
            <ArrowRight className="size-3.5" />
        </button>
    );
}

function RankTable({ firstHeader, items, max, renderName, onLogs }: {
    firstHeader: string;
    items: StatsBreakdownItem[];
    max: number;
    renderName: (item: StatsBreakdownItem) => ReactNode;
    onLogs: (item: StatsBreakdownItem) => void;
}) {
    const t = useTranslations('dashboard');
    if (items.length === 0) {
        return <div className="flex h-40 items-center justify-center text-sm text-muted-foreground">{t('empty')}</div>;
    }
    return (
        <table className="w-full table-fixed text-sm">
            <thead>
                <tr className="text-left text-xs text-muted-foreground">
                    <th className="w-7 pb-2 font-normal">#</th>
                    <th className="pb-2 font-normal">{firstHeader}</th>
                    <th className="w-12 pb-2 text-right font-normal">{t('users.requests')}</th>
                    <th className="w-14 pb-2 text-right font-normal">{t('users.tokens')}</th>
                    <th className="w-32 pb-2 pl-3 font-normal">{t('users.cost')}</th>
                    <th className="w-7 pb-2" />
                </tr>
            </thead>
            <tbody>
                {items.map((item, i) => {
                    const usage = fromLog(item);
                    return (
                        <tr key={item.key} className="border-t border-border/60">
                            <td className="py-2 text-xs font-semibold text-muted-foreground">{i + 1}</td>
                            <td className="min-w-0 py-2 pr-2">{renderName(item)}</td>
                            <td className="py-2 text-right tabular-nums text-muted-foreground">{formatCountText(usage.success + usage.failed)}</td>
                            <td className="py-2 text-right tabular-nums text-muted-foreground">{formatCountText(totalTokens(usage))}</td>
                            <td className="py-2 pl-3">
                                <div className="flex items-center gap-2">
                                    <CostBar ratio={max > 0 ? item.total_cost / max : 0} className="flex-1" />
                                    <span className="w-16 shrink-0 text-right tabular-nums">{formatMoneyText(item.total_cost)}</span>
                                </div>
                            </td>
                            <td className="py-2 text-right">
                                <button type="button" onClick={() => onLogs(item)} title={t('users.viewLogs')} aria-label={t('users.viewLogs')} className="inline-flex size-6 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground">
                                    <ScrollText className="size-3.5" />
                                </button>
                            </td>
                        </tr>
                    );
                })}
            </tbody>
        </table>
    );
}

/** 手机：一张卡，用户 / Key 切换，前 5 名，名字 + 花费 + 横条。 */
export function MobileRankPanel({ users, keys, onUser, onUserLogs, onKeyLogs }: {
    users: StatsBreakdown | undefined;
    keys: StatsBreakdown | undefined;
    onUser: (id: number) => void;
    onUserLogs: (item: StatsBreakdownItem) => void;
    onKeyLogs: (item: StatsBreakdownItem) => void;
}) {
    const t = useTranslations('dashboard');
    const [tab, setTab] = useState<'users' | 'keys'>('users');
    const items = ((tab === 'users' ? users : keys)?.items ?? []).slice(0, 5);
    const max = items.reduce((m, item) => Math.max(m, item.total_cost), 0);
    return (
        <Panel
            title={t('users.title')}
            tip={tab === 'users' ? t('users.tip') : t('keys.tip')}
            right={<Segmented size="sm" value={tab} onChange={setTab} options={[{ value: 'users', label: t('mobile.users') }, { value: 'keys', label: t('mobile.keys') }]} />}
            className="md:hidden"
        >
            {items.length === 0 ? (
                <div className="flex h-24 items-center justify-center text-sm text-muted-foreground">{t('empty')}</div>
            ) : (
                <ol className="space-y-3">
                    {items.map((item, i) => (
                        <li key={item.key}>
                            <button
                                type="button"
                                onClick={() => {
                                    if (tab === 'keys') onKeyLogs(item);
                                    else if (item.id > 0 && !item.missing) onUser(item.id);
                                    else onUserLogs(item);
                                }}
                                className="w-full text-left"
                            >
                                <div className="flex items-center gap-3 text-sm">
                                    <span className="w-4 text-xs font-semibold text-muted-foreground">{i + 1}</span>
                                    <span className="min-w-0 flex-1 truncate">{tab === 'users' ? userLabel(item, (k) => t(`users.${k}`)) : item.name || `#${item.id}`}</span>
                                    <span className="shrink-0 tabular-nums">{formatMoneyText(item.total_cost)}</span>
                                </div>
                                <CostBar ratio={max > 0 ? item.total_cost / max : 0} className="mt-1.5 ml-7 w-[calc(100%-1.75rem)]" />
                            </button>
                        </li>
                    ))}
                </ol>
            )}
        </Panel>
    );
}

/* ---------------- 前 5 名每天花多少 ---------------- */

export function UserTrendPanel({ data }: { data: StatsUserTrend | undefined }) {
    const t = useTranslations('dashboard.topTrend');
    const [metric, setMetric] = useState<'cost' | 'tokens'>('cost');
    const users = useMemo(() => data?.users ?? [], [data]);
    const points = useMemo(() => (data?.dates ?? []).map((date, i) => {
        const row: Record<string, string | number> = { label: shortDate(date) };
        for (const u of users) {
            const usage = fromLog(u.points[i]);
            row[`u${u.user_id}`] = metric === 'cost' ? usage.cost : totalTokens(usage);
        }
        return row;
    }), [data, users, metric]);
    const last = points.length - 1;

    return (
        <Panel
            title={t('title')}
            tip={t('tip')}
            className="hidden md:block"
            right={<Segmented size="sm" value={metric} onChange={setMetric} options={[{ value: 'cost', label: t('cost') }, { value: 'tokens', label: t('tokens') }]} />}
        >
            {users.length === 0 ? (
                <div className="flex h-40 items-center justify-center text-sm text-muted-foreground">{t('empty')}</div>
            ) : (
                <div className="h-52 w-full">
                    <ResponsiveContainer width="100%" height="100%">
                        <LineChart data={points} margin={{ top: 8, right: 80, bottom: 0, left: 0 }}>
                            <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="var(--border)" />
                            <XAxis dataKey="label" tickLine={false} axisLine={false} fontSize={11} minTickGap={12} />
                            <YAxis tickLine={false} axisLine={false} fontSize={11} width={44} tickFormatter={(v: number) => (metric === 'cost' ? formatMoneyAxis(v) : formatCountText(v))} />
                            <Tooltip
                                contentStyle={{ borderRadius: 12, fontSize: 12, background: 'var(--popover)', border: '1px solid var(--border)' }}
                                formatter={(value: number, key: string) => {
                                    const u = users.find((x) => `u${x.user_id}` === key);
                                    return [metric === 'cost' ? formatMoneyText(value) : formatCountText(value), u?.name ?? key];
                                }}
                            />
                            {users.map((u, i) => (
                                <Line
                                    key={u.user_id}
                                    type="linear"
                                    dataKey={`u${u.user_id}`}
                                    stroke={SLICE_COLORS[i % SLICE_COLORS.length]}
                                    strokeWidth={1.75}
                                    isAnimationActive={false}
                                    dot={(props: { cx?: number; cy?: number; index?: number }) => (
                                        props.index === last && props.cx !== undefined && props.cy !== undefined
                                            ? <circle key={`end-${u.user_id}`} cx={props.cx} cy={props.cy} r={3} fill={SLICE_COLORS[i % SLICE_COLORS.length]} />
                                            : <g key={`dot-${u.user_id}-${props.index}`} />
                                    )}
                                />
                            ))}
                            <Customized component={(chart: EndLabelChart) => <EndLabels chart={chart} users={users} />} />
                        </LineChart>
                    </ResponsiveContainer>
                </div>
            )}
        </Panel>
    );
}


type EndLabelChart = {
    // recharts 2：每条线的像素点在 props.points，dataKey 在原始元素 item.props 上。
    formattedGraphicalItems?: { item?: { props?: { dataKey?: unknown } }; props?: { points?: { x?: number; y?: number | null }[] } }[];
    offset?: { top?: number; height?: number };
};

/** 线尾的名字：按末端高低排好，挨得太近就上下推开，免得叠在一起。 */
function EndLabels({ chart, users }: { chart: EndLabelChart; users: StatsUserTrend['users'] }) {
    const GAP = 13;
    const top = chart.offset?.top ?? 0;
    const bottom = top + (chart.offset?.height ?? 0) + 4;
    const labels = (chart.formattedGraphicalItems ?? []).flatMap((item) => {
        const key = String(item.item?.props?.dataKey ?? '');
        const idx = users.findIndex((u) => `u${u.user_id}` === key);
        const end = item.props?.points?.at(-1);
        if (idx < 0 || end?.x === undefined || end.y === undefined || end.y === null) return [];
        const name = users[idx].name;
        return [{ key, x: end.x, y: end.y, color: SLICE_COLORS[idx % SLICE_COLORS.length], name: name.length > 10 ? `${name.slice(0, 10)}…` : name }];
    }).sort((a, b) => a.y - b.y);
    const ys = labels.map((l) => l.y);
    for (let i = 1; i < ys.length; i++) ys[i] = Math.max(ys[i], ys[i - 1] + GAP);
    if (ys.length && ys[ys.length - 1] > bottom) {
        ys[ys.length - 1] = bottom;
        for (let i = ys.length - 2; i >= 0; i--) ys[i] = Math.min(ys[i], ys[i + 1] - GAP);
    }
    return (
        <g>
            {labels.map((l, i) => (
                <text key={l.key} x={l.x + 8} y={ys[i] + 4} fontSize={11} fill="var(--foreground)">
                    <tspan fill={l.color}>●</tspan> {l.name}
                </text>
            ))}
        </g>
    );
}
