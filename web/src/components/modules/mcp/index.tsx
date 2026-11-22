'use client';

import { useState, useSyncExternalStore } from 'react';
import { useTranslations } from 'next-intl';
import { Check, Copy, ImagePlus, Plug, Wrench, ChartColumn, ShieldCheck } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useAuthStore } from '@/api/endpoints/user';
import { useModelList } from '@/api/endpoints/model';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useMCPStats } from '@/api/endpoints/mcp';

const card = 'rounded-3xl border border-border bg-card p-6 space-y-5';
const subscribeOrigin = () => () => {};
const getOrigin = () => window.location.origin;
const getServerOrigin = () => '';

function AdminTools() {
    const t = useTranslations('mcp');
    const settings = useSettingList();
    const models = useModelList();
    const update = useSetSetting();
    const value = (key: string) => settings.data?.find((item) => item.key === key)?.value;
    const defaultModel = value(SettingKey.MCPDefaultModel) ?? '';
    const options = [...new Set([defaultModel, ...(models.data ?? []).map((model) => model.name)].filter(Boolean))];
    const disabled = !settings.data || update.isPending;

    return <section className={card}>
        <div className="flex flex-wrap items-center justify-between gap-4">
            <div className="flex items-center gap-3"><div className="rounded-xl bg-muted p-2.5"><Wrench className="size-5" /></div><div><h2 className="text-lg font-bold">{t('tools')}</h2><p className="text-xs text-muted-foreground">{t('toolsHint')}</p></div></div>
            <label className="flex items-center gap-3 text-sm font-medium">{t('globalEnabled')}<Switch aria-label={t('globalEnabled')} checked={value(SettingKey.MCPEnabled) === 'true'} disabled={disabled} onCheckedChange={(on) => update.mutate({ key: SettingKey.MCPEnabled, value: String(on) })} /></label>
        </div>
        {(settings.isError || models.isError || update.isError) && <p role="alert" className="text-sm text-destructive">{t('settingsError')}</p>}
        {settings.isLoading ? <p className="text-sm text-muted-foreground">{t('loading')}</p> : <>
            <div className="flex flex-wrap items-center gap-3"><label htmlFor="mcp-model" className="text-sm">{t('defaultModel')}</label><Select value={defaultModel} disabled={disabled || models.isLoading} onValueChange={(value) => update.mutate({ key: SettingKey.MCPDefaultModel, value })}><SelectTrigger id="mcp-model" className="w-72 rounded-xl text-xs"><SelectValue placeholder={t('defaultModel')} /></SelectTrigger><SelectContent>{options.map((name) => <SelectItem key={name} value={name}>{name}</SelectItem>)}</SelectContent></Select></div>
            <Table><TableHeader><TableRow><TableHead>{t('tool')}</TableHead><TableHead>{t('description')}</TableHead><TableHead className="text-right">{t('enabled')}</TableHead></TableRow></TableHeader>
                <TableBody>{[
                    { name: 'generate_image', key: SettingKey.MCPGenerateEnabled, description: 'generateDescription' },
                    { name: 'edit_image', key: SettingKey.MCPEditEnabled, description: 'editDescription' },
                ].map((tool) => <TableRow key={tool.name}><TableCell className="py-4"><div className="flex items-center gap-2"><ImagePlus className="size-4 text-muted-foreground" /><code className="text-xs font-medium">{tool.name}</code></div></TableCell><TableCell className="text-sm text-muted-foreground">{t(tool.description)}</TableCell><TableCell className="text-right"><Switch aria-label={`${tool.name} ${t('enabled')}`} checked={value(tool.key) === 'true'} disabled={disabled} onCheckedChange={(on) => update.mutate({ key: tool.key, value: String(on) })} /></TableCell></TableRow>)}</TableBody>
            </Table>
        </>}
    </section>;
}

export function MCP() {
    const t = useTranslations('mcp');
    const user = useAuthStore((state) => state.user);
    const stats = useMCPStats(user?.id);
    const origin = useSyncExternalStore(subscribeOrigin, getOrigin, getServerOrigin);
    const [copyState, setCopyState] = useState<'idle' | 'copied' | 'error'>('idle');
    const clientConfig = JSON.stringify({ mcpServers: { octopus: { url: `${origin}/mcp`, headers: { Authorization: 'Bearer sk-octopus-xxxxxxxx' } } } }, null, 2);

    async function copyConfig() {
        try { await navigator.clipboard.writeText(clientConfig); setCopyState('copied'); }
        catch { setCopyState('error'); }
    }

    return <div className="h-full overflow-y-auto overscroll-contain pb-24 md:pb-6"><PageWrapper className="space-y-5">
        <p className="px-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        <section className={card}>
            <div className="flex items-center gap-3"><div className="rounded-xl bg-muted p-2.5"><Plug className="size-5" /></div><div><h2 className="text-lg font-bold">{t('endpoint')}</h2><p className="text-xs text-muted-foreground">{t('endpointHint')}</p></div></div>
            <div className="grid gap-3 sm:grid-cols-2">{[['Streamable HTTP', '/mcp'], ['SSE', '/mcp/sse']].map(([label, path]) => <div key={label} className="rounded-2xl border border-border bg-muted/25 px-4 py-3"><div className="mb-1.5 text-xs font-medium text-muted-foreground">{label}</div><code className="break-all text-sm font-medium">{origin}{path}</code></div>)}</div>
            <div className="flex items-start gap-2 rounded-xl bg-primary/5 px-3 py-2.5 text-sm"><ShieldCheck className="mt-0.5 size-4 shrink-0 text-primary" /><p>{t('billingNote')}</p></div>
            <div className="overflow-hidden rounded-2xl border border-border"><div className="flex items-center justify-between border-b border-border bg-muted/40 px-4 py-2"><span className="text-xs font-medium">{t('clientConfig')}</span><Button size="sm" variant="ghost" disabled={!origin} onClick={copyConfig}>{copyState === 'copied' ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}{t(copyState === 'copied' ? 'copied' : copyState === 'error' ? 'copyFailed' : 'copy')}</Button></div><pre className="overflow-x-auto p-4 text-xs leading-relaxed"><code>{clientConfig}</code></pre></div>
        </section>
        {user?.role === 'admin' && <AdminTools />}
        <section className={card}>
            <div className="flex items-center gap-3"><div className="rounded-xl bg-muted p-2.5"><ChartColumn className="size-5" /></div><div><h2 className="text-lg font-bold">{t('usage')}</h2><p className="text-xs text-muted-foreground">{t(user?.role === 'admin' ? 'usageHint' : 'ownUsageHint')}</p></div></div>
            {stats.isLoading ? <p className="text-sm text-muted-foreground">{t('loading')}</p> : stats.isError ? <p role="alert" className="text-sm text-destructive">{t('usageError')}</p> : stats.data && <>
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">{(['calls', 'images', 'cost', 'failed'] as const).map((label) => <div key={label} className="rounded-2xl border border-border bg-muted/20 p-4"><div className="text-xs text-muted-foreground">{t(label)}</div><div className="mt-2 text-2xl font-semibold tracking-tight tabular-nums">{label === 'cost' ? `$${stats.data.totals.cost.toFixed(4)}` : stats.data.totals[label]}</div></div>)}</div>
                <div><h3 className="mb-2 text-sm font-medium">{t('topKeys')}</h3><Table><TableHeader><TableRow><TableHead>{t('keyName')}</TableHead>{['calls', 'images', 'cost', 'failed'].map((label) => <TableHead key={label} className="text-right">{t(label)}</TableHead>)}</TableRow></TableHeader><TableBody>{stats.data.top_keys.length === 0 ? <TableRow><TableCell colSpan={5} className="py-6 text-center text-muted-foreground">{t('empty')}</TableCell></TableRow> : stats.data.top_keys.map((key) => <TableRow key={key.api_key_id}><TableCell className="py-3 font-mono text-xs">{key.name || `#${key.api_key_id}`}</TableCell><TableCell className="text-right tabular-nums">{key.calls}</TableCell><TableCell className="text-right tabular-nums">{key.images}</TableCell><TableCell className="text-right tabular-nums">${key.cost.toFixed(4)}</TableCell><TableCell className="text-right tabular-nums">{key.failed}</TableCell></TableRow>)}</TableBody></Table></div>
            </>}
            <p className="text-xs text-muted-foreground">{t('usageDelay')}</p>
        </section>
    </PageWrapper></div>;
}
