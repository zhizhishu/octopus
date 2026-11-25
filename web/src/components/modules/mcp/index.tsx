'use client';

import { useEffect, useMemo, useState, useSyncExternalStore } from 'react';
import { useTranslations } from 'next-intl';
import { Check, Copy, ChevronDown, ImagePlus, Plug, Settings2, ChartColumn } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useAuthStore } from '@/api/endpoints/user';
import { useAPIKeyList } from '@/api/endpoints/apikey';
import { useModelList } from '@/api/endpoints/model';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useMCPStats, MCP_RANGES, type MCPRange, type MCPUsage } from '@/api/endpoints/mcp';

const card = 'rounded-3xl border border-border bg-card p-6 space-y-5';
const subscribeOrigin = () => () => {};
const getOrigin = () => window.location.origin;
const getServerOrigin = () => '';
const PLACEHOLDER_KEY = 'sk-octopus-xxxxxxxx';

const money = (value: number) => `$${value.toFixed(value >= 100 ? 2 : 4)}`;
const maskKey = (key: string) => (key.length > 12 ? `${key.slice(0, 7)}…${key.slice(-4)}` : key);

/** 管理员总开关：关掉后所有客户端立即无法调用。 */
function AdminSwitch() {
    const t = useTranslations('mcp');
    const settings = useSettingList();
    const update = useSetSetting();
    const enabled = settings.data?.find((item) => item.key === SettingKey.MCPEnabled)?.value === 'true';
    return <div className="flex items-center justify-between gap-4 rounded-2xl bg-muted/40 px-4 py-3">
        <div><div className="text-sm font-semibold">{t('globalEnabled')}</div><div className="text-xs text-muted-foreground">{t(enabled ? 'globalOnHint' : 'globalOffHint')}</div></div>
        <Switch aria-label={t('globalEnabled')} checked={enabled} disabled={!settings.data || update.isPending} onCheckedChange={(on) => update.mutate({ key: SettingKey.MCPEnabled, value: String(on) })} />
    </div>;
}

/** 高级设置：默认模型与单个工具开关，平时折叠。 */
function AdminAdvanced() {
    const t = useTranslations('mcp');
    const [open, setOpen] = useState(false);
    const settings = useSettingList();
    const models = useModelList();
    const update = useSetSetting();
    const value = (key: string) => settings.data?.find((item) => item.key === key)?.value;
    const defaultModel = value(SettingKey.MCPDefaultModel) ?? '';
    const options = [...new Set([defaultModel, ...(models.data ?? []).map((model) => model.name)].filter(Boolean))];
    const disabled = !settings.data || update.isPending;

    return <section className="rounded-3xl border border-border bg-card px-6 py-4">
        <button type="button" className="flex w-full items-center justify-between gap-3 text-left" aria-expanded={open} onClick={() => setOpen(!open)}>
            <span className="flex items-center gap-3"><Settings2 className="size-4 text-muted-foreground" /><span className="text-sm font-semibold">{t('advanced')}</span><span className="text-xs text-muted-foreground">{t('advancedHint')}</span></span>
            <ChevronDown className={`size-4 text-muted-foreground transition-transform ${open ? 'rotate-180' : ''}`} />
        </button>
        {open && <div className="mt-4 space-y-4">
            {(settings.isError || models.isError || update.isError) && <p role="alert" className="text-sm text-destructive">{t('settingsError')}</p>}
            <div className="flex flex-wrap items-center gap-3"><label htmlFor="mcp-model" className="text-sm">{t('fallbackModel')}</label><Select value={defaultModel} disabled={disabled || models.isLoading} onValueChange={(next) => update.mutate({ key: SettingKey.MCPDefaultModel, value: next })}><SelectTrigger id="mcp-model" className="w-72 rounded-xl text-xs"><SelectValue placeholder={t('fallbackModel')} /></SelectTrigger><SelectContent>{options.map((name) => <SelectItem key={name} value={name}>{name}</SelectItem>)}</SelectContent></Select><span className="text-xs text-muted-foreground">{t('fallbackModelHint')}</span></div>
            <Table><TableHeader><TableRow><TableHead>{t('tool')}</TableHead><TableHead>{t('description')}</TableHead><TableHead className="text-right">{t('enabled')}</TableHead></TableRow></TableHeader>
                <TableBody>{[
                    { name: 'generate_image', key: SettingKey.MCPGenerateEnabled, description: 'generateDescription' },
                    { name: 'edit_image', key: SettingKey.MCPEditEnabled, description: 'editDescription' },
                ].map((tool) => <TableRow key={tool.name}><TableCell className="py-3"><div className="flex items-center gap-2"><ImagePlus className="size-4 text-muted-foreground" /><code className="text-xs font-medium">{tool.name}</code></div></TableCell><TableCell className="text-sm text-muted-foreground">{t(tool.description)}</TableCell><TableCell className="text-right"><Switch aria-label={`${tool.name} ${t('enabled')}`} checked={value(tool.key) === 'true'} disabled={disabled} onCheckedChange={(on) => update.mutate({ key: tool.key, value: String(on) })} /></TableCell></TableRow>)}</TableBody>
            </Table>
        </div>}
    </section>;
}

/** 选当前账号自己的 Key；只在网页账号登录时挂载（API Key 登录不调用列表接口）。 */
function KeyPicker({ userId, value, onChange }: { userId?: number; value: string; onChange: (key: string) => void }) {
    const t = useTranslations('mcp');
    const keys = useAPIKeyList();
    const own = useMemo(() => (keys.data ?? []).filter((key) => key.enabled && (key.user_id === undefined || key.user_id === userId)), [keys.data, userId]);
    const current = own.find((key) => key.api_key === value) ?? own[0];
    useEffect(() => { if (current && current.api_key !== value) onChange(current.api_key); }, [current, value, onChange]);
    if (keys.isLoading) return <span className="text-xs text-muted-foreground">{t('loading')}</span>;
    if (own.length === 0) return <span className="text-xs text-muted-foreground">{t('noKey')}</span>;
    return <div className="flex items-center gap-2"><span className="text-xs text-muted-foreground">{t('useKey')}</span>
        <Select value={current?.api_key ?? ''} onValueChange={onChange}><SelectTrigger aria-label={t('useKey')} className="w-64 rounded-xl text-xs"><SelectValue /></SelectTrigger>
            <SelectContent>{own.map((key) => <SelectItem key={key.id} value={key.api_key}>{key.name || `#${key.id}`} · {maskKey(key.api_key)}</SelectItem>)}</SelectContent></Select></div>;
}

function Breakdown({ title, rows, label, empty }: { title: string; rows: (MCPUsage & { id: number; name: string })[]; label: string; empty: string }) {
    const t = useTranslations('mcp');
    const total = rows.reduce((sum, row) => sum + row.cost, 0);
    return <section className={card}>
        <h2 className="text-base font-semibold">{title}</h2>
        <Table><TableHeader><TableRow><TableHead>{label}</TableHead><TableHead className="text-right">{t('calls')}</TableHead><TableHead className="text-right">{t('images')}</TableHead><TableHead className="text-right">{t('cost')}</TableHead><TableHead className="text-right">{t('failed')}</TableHead><TableHead className="w-28">{t('share')}</TableHead></TableRow></TableHeader>
            <TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={6} className="py-6 text-center text-muted-foreground">{empty}</TableCell></TableRow> : rows.map((row) => <TableRow key={row.id}>
                <TableCell className="py-3">{row.name || `#${row.id}`}</TableCell>
                <TableCell className="text-right tabular-nums">{row.calls}</TableCell>
                <TableCell className="text-right tabular-nums">{row.images}</TableCell>
                <TableCell className="text-right tabular-nums">{money(row.cost)}</TableCell>
                <TableCell className={`text-right tabular-nums ${row.failed > 0 ? 'text-destructive' : ''}`}>{row.failed}</TableCell>
                <TableCell><div className="h-1.5 w-24 overflow-hidden rounded-full bg-muted"><div className="h-full rounded-full bg-primary" style={{ width: `${total > 0 ? Math.max(2, (row.cost / total) * 100) : 0}%` }} /></div></TableCell>
            </TableRow>)}</TableBody></Table>
    </section>;
}

export function MCP() {
    const t = useTranslations('mcp');
    const user = useAuthStore((state) => state.user);
    const isAPIKeyAuth = useAuthStore((state) => state.isAPIKeyAuth);
    const token = useAuthStore((state) => state.token);
    const isAdmin = user?.role === 'admin';
    const [days, setDays] = useState<MCPRange>(7);
    const stats = useMCPStats(user?.id, days);
    const origin = useSyncExternalStore(subscribeOrigin, getOrigin, getServerOrigin);
    const [picked, setPicked] = useState('');
    const [copied, setCopied] = useState<'' | 'url' | 'config' | 'error'>('');
    const apiKey = (isAPIKeyAuth ? token : picked) || PLACEHOLDER_KEY;
    const url = `${origin}/mcp`;
    const config = (key: string) => JSON.stringify({ mcpServers: { octopus: { url, headers: { Authorization: `Bearer ${key}` } } } }, null, 2);

    async function copy(kind: 'url' | 'config') {
        try { await navigator.clipboard.writeText(kind === 'url' ? url : config(apiKey)); setCopied(kind); }
        catch { setCopied('error'); }
    }
    const rangeLabel: Record<MCPRange, string> = { 1: t('range1'), 7: t('range7'), 30: t('range30') };
    const copyLabel = (kind: 'url' | 'config') => copied === kind ? t('copied') : copied === 'error' ? t('copyFailed') : t(kind === 'url' ? 'copyUrl' : 'copyConfig');

    return <div className="h-full overflow-y-auto overscroll-contain pb-24 md:pb-6"><PageWrapper className="space-y-5">
        <p className="px-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        <section className={card}>
            <div className="flex flex-wrap items-center justify-between gap-4">
                <div className="flex items-center gap-3"><div className="rounded-xl bg-muted p-2.5"><Plug className="size-5" /></div><div><h2 className="text-lg font-bold">{t('endpoint')}</h2><p className="text-xs text-muted-foreground">{t('billingNote')}</p></div></div>
                {!isAPIKeyAuth && user && <KeyPicker userId={user.id} value={picked} onChange={setPicked} />}
            </div>
            {isAdmin && <AdminSwitch />}
            <div className="flex flex-wrap items-center gap-2 rounded-2xl border border-border bg-muted/25 px-4 py-2.5">
                <code className="min-w-0 flex-1 break-all text-sm font-medium">{url}</code>
                <Button size="sm" variant="secondary" disabled={!origin} onClick={() => copy('url')}>{copied === 'url' ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{copyLabel('url')}</Button>
                <Button size="sm" disabled={!origin} onClick={() => copy('config')}>{copied === 'config' ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{copyLabel('config')}</Button>
            </div>
            <pre className="overflow-x-auto rounded-2xl border border-border bg-muted/25 p-4 text-xs leading-relaxed"><code>{config(apiKey === PLACEHOLDER_KEY ? PLACEHOLDER_KEY : maskKey(apiKey))}</code></pre>
            <p className="text-xs text-muted-foreground">{t(apiKey === PLACEHOLDER_KEY ? 'keyMissingHint' : 'keyFilledHint')} {t('sseHint')} <code>{origin}/mcp/sse</code></p>
        </section>
        <section className={card}>
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex items-center gap-3"><div className="rounded-xl bg-muted p-2.5"><ChartColumn className="size-5" /></div><div><h2 className="text-lg font-bold">{t(isAdmin ? 'usage' : 'ownUsage')}</h2><p className="text-xs text-muted-foreground">{t('usageDelay')}</p></div></div>
                <div className="flex gap-1 rounded-xl bg-muted p-1" role="tablist">{MCP_RANGES.map((range) => <button key={range} type="button" role="tab" aria-selected={days === range} onClick={() => setDays(range)} className={`rounded-lg px-3 py-1 text-xs font-medium ${days === range ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground'}`}>{rangeLabel[range]}</button>)}</div>
            </div>
            {stats.isLoading ? <p className="text-sm text-muted-foreground">{t('loading')}</p> : stats.isError ? <p role="alert" className="text-sm text-destructive">{t('usageError')}</p> : stats.data &&
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">{(['calls', 'images', 'cost', 'failed'] as const).map((label) => <div key={label} className="rounded-2xl bg-muted/40 p-4"><div className="text-xs text-muted-foreground">{t(label)}</div><div className="mt-2 text-2xl font-semibold tracking-tight tabular-nums">{label === 'cost' ? money(stats.data.totals.cost) : stats.data.totals[label]}</div></div>)}</div>}
        </section>
        {isAdmin && stats.data && <div className="grid gap-5 xl:grid-cols-2">
            <Breakdown title={t('byChannel')} label={t('channel')} empty={t('empty')} rows={(stats.data.top_channels ?? []).map((row) => ({ ...row, id: row.channel_id }))} />
            <Breakdown title={t('byUser')} label={t('user')} empty={t('empty')} rows={(stats.data.top_users ?? []).map((row) => ({ ...row, id: row.user_id }))} />
        </div>}
        {isAdmin && <AdminAdvanced />}
    </PageWrapper></div>;
}
