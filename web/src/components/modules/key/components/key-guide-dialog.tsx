'use client';

import { useState, useSyncExternalStore } from 'react';
import { CircleAlert, Copy, Info } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import type { APIKey } from '@/api/endpoints/apikey';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { CopyIconButton } from '@/components/common/CopyButton';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import { Seg, btn } from '@/components/modules/user/components/ui-bits';
import { keyFamilies, keyModels, keyStatus, maskKeyLong } from '../lib/key-filters';
import { CLIENT_FAMILY, CURL_FAMILIES, GUIDE_CLIENTS, buildGuide, type CurlFamily, type GuideBlock, type GuideClient, type GuideShell } from '../lib/key-guide';
import { inputCls, useFamilyLabel } from './bits';

export function getBrowserBaseUrl(): string {
    if (typeof window === 'undefined') return 'https://your-octopus.example';
    return window.location.origin;
}

export function useBaseUrl() {
    return useSyncExternalStore(() => () => undefined, getBrowserBaseUrl, () => 'https://your-octopus.example');
}

// 接入说明（照 sub2api 的 UseKeyModal）：选客户端 → 复制那一段。
// 代码里的 Key 显示时遮住中间，复制按钮复制的是带完整 Key 的那份。
// 换一把 Key 打开时父组件用 key 重新挂载，选中的 Key / 方案跟着重置。
export function KeyGuideDialog({ open, onOpenChange, keys, keyID, spentOf }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    keys: APIKey[];
    keyID: number | null;
    spentOf: (id: number) => number;
}) {
    const t = useTranslations('keys.guide');
    const famLabel = useFamilyLabel();
    const baseUrl = useBaseUrl();
    const [selectedID, setSelectedID] = useState<number | null>(keyID);
    const [client, setClient] = useState<GuideClient>('curl');
    const [curlFamily, setCurlFamily] = useState<CurlFamily>('openai-compatible');
    const [shell, setShell] = useState<GuideShell>('unix');
    const [planSlug, setPlanSlug] = useState('');
    const [nowSec] = useState(() => Math.floor(Date.now() / 1000));

    const apiKey = keys.find((k) => k.id === selectedID) ?? keys[0];
    const families = apiKey ? keyFamilies(apiKey) : [];
    const plans = apiKey?.access_plans ?? [];
    const defaultPlanID = apiKey?.default_access_plan_id || plans[0]?.id;
    const needFamily = client === 'curl' ? curlFamily : CLIENT_FAMILY[client];
    const familyOff = !!apiKey && !families.includes(needFamily);
    const status = apiKey ? keyStatus(apiKey, spentOf(apiKey.id), nowSec) : 'ok';

    const make = (key: string): GuideBlock[] => buildGuide(client, {
        baseUrl,
        apiKey: key,
        plan: planSlug || undefined,
        models: apiKey ? keyModels(apiKey) : [],
        shell,
    }, curlFamily);
    const shown = apiKey ? make(maskKeyLong(apiKey.api_key)) : [];
    const full = apiKey ? make(apiKey.api_key) : [];

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent data-key-guide className="max-h-[92vh] overflow-y-auto sm:max-w-3xl">
                <DialogHeader>
                    <DialogTitle>{apiKey ? t('titleFor', { name: apiKey.name }) : t('title')}</DialogTitle>
                    <DialogDescription>{t('desc')}</DialogDescription>
                </DialogHeader>
                {!apiKey ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">{t('noKey')}</div>
                ) : (
                    <div className="min-w-0 space-y-3">
                        <div className="flex flex-wrap items-center gap-2 rounded-2xl bg-muted/40 px-3 py-2 text-sm">
                            <span className="text-muted-foreground">{t('endpoint')}</span>
                            <code className="min-w-0 truncate font-mono">{baseUrl}</code>
                            <CopyIconButton text={baseUrl} className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground hover:bg-background hover:text-foreground" copyIconClassName="size-3.5" checkIconClassName="size-3.5" />
                            <div className="ml-auto flex flex-wrap items-center gap-2">
                                {keys.length > 1 && (
                                    <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                                        {t('key')}
                                        <select className={cn(inputCls, 'h-8 w-40 text-xs')} value={apiKey.id} onChange={(e) => { setSelectedID(Number(e.target.value)); setPlanSlug(''); }}>
                                            {keys.map((k) => <option key={k.id} value={k.id}>{k.name}</option>)}
                                        </select>
                                    </label>
                                )}
                                {plans.length > 1 && (
                                    <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                                        {t('plan')}
                                        <select className={cn(inputCls, 'h-8 w-36 text-xs')} value={planSlug} onChange={(e) => setPlanSlug(e.target.value)}>
                                            {plans.map((p) => (
                                                <option key={p.id} value={p.id === defaultPlanID ? '' : p.slug}>
                                                    {(p.display_name?.trim() || p.slug) + (p.id === defaultPlanID ? t('planDefault') : '')}
                                                </option>
                                            ))}
                                        </select>
                                    </label>
                                )}
                            </div>
                        </div>

                        <div className="-mx-1 overflow-x-auto px-1">
                            <Seg value={client} onChange={setClient} options={GUIDE_CLIENTS.map((c) => ({ value: c, label: t(`client.${c}`) }))} />
                        </div>
                        <div className="flex flex-wrap items-center gap-2">
                            {client === 'curl' && (
                                <Seg size="sm" value={curlFamily} onChange={setCurlFamily} options={CURL_FAMILIES.map((f) => ({ value: f, label: famLabel(f) }))} />
                            )}
                            {(client === 'claude' || client === 'codex' || client === 'gemini') && (
                                <Seg size="sm" value={shell} onChange={setShell} options={[{ value: 'unix', label: t('shellUnix') }, { value: 'powershell', label: t('shellPowershell') }]} />
                            )}
                            <span className="text-xs text-muted-foreground">{t(`clientHint.${client}`)}</span>
                        </div>

                        {status !== 'ok' && (
                            <div className="flex items-start gap-2 rounded-xl bg-amber-500/10 px-3 py-2 text-xs text-amber-800 dark:text-amber-300">
                                <CircleAlert className="mt-0.5 size-3.5 shrink-0" />{t(`statusWarn.${status}`)}
                            </div>
                        )}
                        {familyOff ? (
                            <div className="flex items-start gap-2 rounded-xl border border-dashed border-border px-3 py-4 text-sm text-muted-foreground">
                                <CircleAlert className="mt-0.5 size-4 shrink-0" />{t('familyOff', { family: famLabel(needFamily) })}
                            </div>
                        ) : shown.map((b, i) => (
                            <CodeBlock key={`${client}-${b.id}-${i}`} title={t(`block.${client}.${b.id}`)} file={b.file} code={b.code} copyText={full[i]?.code ?? ''} />
                        ))}

                        {!familyOff && (
                            <div className="flex items-start gap-1.5 text-xs text-muted-foreground">
                                <Info className="mt-0.5 size-3.5 shrink-0" />
                                <span>{client === 'gemini' && planSlug ? t('geminiNoPlan') : keyModels(apiKey).length > 0 ? t('modelsLimited') : t('modelsHint')}</span>
                            </div>
                        )}
                    </div>
                )}
            </DialogContent>
        </Dialog>
    );
}

function CodeBlock({ title, file, code, copyText }: { title: string; file?: string; code: string; copyText: string }) {
    const t = useTranslations('keys.guide');
    const [, copy] = useCopyToClipboard();
    return (
        <div className="min-w-0 overflow-hidden rounded-2xl border border-border/70">
            <div className="flex items-center justify-between gap-2 bg-muted/30 px-3 py-2">
                <div className="min-w-0 truncate text-xs text-muted-foreground">
                    {title}{file && <code className="ml-1.5 rounded bg-muted px-1.5 py-0.5 font-mono text-[11px] text-foreground">{file}</code>}
                </div>
                <button type="button" className={cn(btn, 'h-7 shrink-0 rounded-lg px-2 text-xs')} onClick={async () => { await copy(copyText); toast.success(t('copied')); }}>
                    <Copy className="size-3.5" />{t('copy')}
                </button>
            </div>
            <pre className="overflow-x-auto bg-zinc-950 p-3 text-xs leading-5 text-zinc-100"><code>{code}</code></pre>
        </div>
    );
}
