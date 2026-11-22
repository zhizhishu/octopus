'use client';

import { Fragment, useState } from 'react';
import { useTranslations } from 'next-intl';
import { ChevronDown, ChevronRight, Download } from 'lucide-react';
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useAuthStore, useUserList } from '@/api/endpoints/user';
import { downloadUsageReport, useUsageReport, useUsageReportKeys, type UsageReportQuery, type UsageReportRow } from '@/api/endpoints/usage-report';
import { cacheRateBase, fromLog, type LogUsage } from '@/components/modules/dashboard/model';
import { useSettingStore, type Locale } from '@/stores/setting';

const colors = ['#8b9cf7', '#a8d9c5', '#e8bb7a', '#d1a7df', '#7dc5d9', '#d99e9e'];
const selectStyle = 'h-9 rounded-xl border border-input bg-background px-3 text-sm';
const number = (v: number) => new Intl.NumberFormat('en', { notation: v >= 100000 ? 'compact' : 'standard', maximumFractionDigits: 2 }).format(v);
const money = (v: number, digits = 4) => `$${v.toFixed(digits)}`;
export function usageCacheRate(u: LogUsage): string {
    const base = cacheRateBase(fromLog(u));
    return base > 0 ? `${(u.cache_hit_token / base * 100).toFixed(1)}%` : '—';
}

function Filter({ label, value, options, onChange }: { label: string; value: string; options: [string, string][]; onChange: (value: string) => void }) {
    return <select className={selectStyle} aria-label={label} value={value} onChange={e => onChange(e.target.value)}>{options.map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select>;
}

export function Usage() {
    const t = useTranslations('billingMock');
    const user = useAuthStore(s => s.user);
    const admin = user?.role === 'admin';
    const { locale, setLocale } = useSettingStore();
    const users = useUserList({ enabled: admin });
    const keys = useUsageReportKeys();
    const [query, setQuery] = useState<UsageReportQuery>({ range: '30d', group: 'user' });
    const [expanded, setExpanded] = useState<string | null>(null);
    const [exporting, setExporting] = useState(false);
    const [exportError, setExportError] = useState(false);
    const report = useUsageReport(query);
    const data = report.data;
    const patch = (next: Partial<UsageReportQuery>) => { setQuery(q => ({ ...q, ...next })); setExpanded(null); };
    const availableKeys = (keys.data ?? []).filter(k => !query.user_id || k.user_id === query.user_id);
    const models = [...new Set(data?.daily.flatMap(d => Object.keys(d.models)) ?? [])].sort();
    const chart = data?.daily.map(d => ({ date: d.date.slice(5), ...Object.fromEntries(models.map((m, i) => [`m${i}`, d.models[m] ?? 0])) })) ?? [];
    const headers = ['name', 'requests', 'input', 'output', 'cacheHit', 'cacheWrite', 'hitRate', 'cost'];
    const exportCSV = async () => {
        setExporting(true); setExportError(false);
        try { await downloadUsageReport(query); } catch { setExportError(true); } finally { setExporting(false); }
    };
    const cells = (u: LogUsage) => [number(u.request_success + u.request_failed), number(u.input_token), number(u.output_token), number(u.cache_hit_token), number(u.cache_write_token), usageCacheRate(u), money(u.total_cost)].map((value, i) => <TableCell key={i} className="tabular-nums">{value}</TableCell>);
    const modelRow = (row: UsageReportRow) => <TableRow key={row.id} className="bg-muted/30 text-xs text-muted-foreground"><TableCell className="pl-8">↳ {row.name}</TableCell>{cells(row.usage)}</TableRow>;
    return <div className="h-full overflow-y-auto pb-16"><PageWrapper className="space-y-5">
        <header className="flex items-start justify-between gap-3"><div><h1 className="text-2xl font-semibold tracking-tight">{t('usage')}</h1><p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p></div><Select value={locale} onValueChange={v => setLocale(v as Locale)}><SelectTrigger className="w-32 rounded-xl" aria-label="Language"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="zh_hans">简体中文</SelectItem><SelectItem value="zh_hant">繁體中文</SelectItem><SelectItem value="en">English</SelectItem></SelectContent></Select></header>
        <div className="flex flex-wrap items-center gap-3">
            <Filter label={t('period')} value={query.range} onChange={v => patch({ range: v as UsageReportQuery['range'] })} options={['today', '7d', '30d', 'custom'].map(v => [v, t(v)])} />
            {query.range === 'custom' && <><Input type="date" className="w-40" aria-label={t('from')} value={query.start ?? ''} onChange={e => patch({ start: e.target.value })} /><Input type="date" className="w-40" aria-label={t('to')} value={query.end ?? ''} onChange={e => patch({ end: e.target.value })} /></>}
            <div className="flex rounded-xl border p-1">{(['user', 'apikey', 'model', 'day'] as const).map(g => <Button key={g} size="sm" variant={query.group === g ? 'secondary' : 'ghost'} onClick={() => patch({ group: g })}>{t(g === 'apikey' ? 'key' : g)}</Button>)}</div>
            {admin && <Filter label={t('user')} value={String(query.user_id ?? '')} onChange={v => patch({ user_id: v ? Number(v) : undefined, api_key_id: undefined })} options={[[ '', t('allUsers')], ...(users.data ?? []).map(u => [String(u.id), u.username] as [string, string])]} />}
            <Filter label={t('key')} value={String(query.api_key_id ?? '')} onChange={v => patch({ api_key_id: v ? Number(v) : undefined })} options={[[ '', t('allKeys')], ...availableKeys.map(k => [String(k.id), k.name] as [string, string])]} />
            <Input className="w-44" aria-label={t('model')} placeholder={t('modelFilter')} value={query.model ?? ''} onChange={e => patch({ model: e.target.value })} />
            <Button variant="outline" className="ml-auto" disabled={exporting || !data || report.isFetching || report.isError} onClick={exportCSV}><Download className="size-4" />{t(exporting ? 'exporting' : 'export')}</Button>
        </div>
        {(report.isError || exportError) && <div role="alert" className="flex items-center justify-between rounded-2xl border border-destructive/40 p-4 text-sm text-destructive">{t(exportError ? 'exportError' : 'loadError')}<Button variant="outline" size="sm" onClick={() => { setExportError(false); void report.refetch(); }}>{t('retry')}</Button></div>}
        {report.isLoading && <p role="status" className="text-sm text-muted-foreground">{t('loading')}</p>}
        {query.range === 'custom' && (!query.start || !query.end) && <p className="text-sm text-muted-foreground">{t('chooseDates')}</p>}
        {data && <>
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-6">{[['requests', number(data.summary.request_success + data.summary.request_failed), t('successFailedCount', { success: number(data.summary.request_success), failed: number(data.summary.request_failed) })], ['input', number(data.summary.input_token), t('tokens')], ['output', number(data.summary.output_token), t('tokens')], ['cacheHit', number(data.summary.cache_hit_token), t('tokens')], ['hitRate', usageCacheRate(data.summary), t('cacheNote')], ['cost', money(data.summary.total_cost, 2), 'USD']].map(([label, value, sub]) => <div key={label} className="rounded-2xl border bg-card p-4"><p className="text-xs text-muted-foreground">{t(label)}</p><p className="my-2 text-2xl font-semibold tabular-nums">{value}</p><p className="text-xs text-muted-foreground">{sub}</p></div>)}</div>
            <section className="rounded-3xl border bg-card p-6"><div className="mb-4 flex justify-between"><h2 className="font-medium">{t('dailyCost')}</h2><span className="text-xs text-muted-foreground">{data.start} — {data.end} · USD</span></div><div className="h-44"><ResponsiveContainer width="100%" height="100%"><BarChart data={chart} barSize={22}><CartesianGrid vertical={false} stroke="var(--border)" /><XAxis dataKey="date" axisLine={false} tickLine={false} tick={{ fontSize: 11 }} /><YAxis axisLine={false} tickLine={false} tick={{ fontSize: 11 }} tickFormatter={v => `$${v}`} /><Tooltip formatter={(v: number) => money(v)} />{models.map((m, i) => <Bar key={m} dataKey={`m${i}`} name={m} stackId="cost" fill={colors[i % colors.length]} />)}</BarChart></ResponsiveContainer></div><div className="mt-2 flex flex-wrap justify-center gap-6 text-xs text-muted-foreground">{models.map((m, i) => <span key={m}><span style={{ color: colors[i % colors.length] }}>●</span> {m}</span>)}</div></section>
            <section className="overflow-hidden rounded-3xl border bg-card"><Table><TableHeader><TableRow>{headers.map(h => <TableHead key={h}>{t(h)}</TableHead>)}</TableRow></TableHeader><TableBody>{data.rows.map(row => <Fragment key={row.id}><TableRow><TableCell><button aria-expanded={expanded === row.id} className="flex items-center gap-2 font-medium" onClick={() => setExpanded(expanded === row.id ? null : row.id)}>{expanded === row.id ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}{row.name}</button></TableCell>{cells(row.usage)}</TableRow>{expanded === row.id && row.models.map(modelRow)}</Fragment>)}</TableBody></Table>{data.rows.length === 0 && <p className="p-10 text-center text-sm text-muted-foreground">{t('emptyUsage')}</p>}</section>
        </>}
        <p className="text-xs text-muted-foreground">{t('snapshot')}</p>
    </PageWrapper></div>;
}
