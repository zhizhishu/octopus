'use client';

import { useMemo, useState } from 'react';
import { Check, Copy, Loader2, Search } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import { useCreateGuestKey, useGuestKeyModels, useUpdateGuestKey, type GuestKey } from '@/api/endpoints/guest-key';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Switch } from '@/components/ui/switch';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import { CheckMark, HelpTip, btn, btnPrimary, chip, chipOn, label } from '@/components/modules/user/components/ui-bits';
import { localInputToUnix, normalizeMoneyInput, unixToLocalInput } from '../lib/key-filters';
import {
    DEFAULT_GUEST_PRESET,
    GUEST_EXPIRE_PRESETS,
    guestModels,
    presetExpireAt,
    validateGuestDraft,
    type GuestDraft,
    type GuestExpirePreset,
} from '../lib/guest-key';
import { inputCls } from '../components/bits';
import { useBaseUrl } from '../components/key-guide-dialog';

// 新建 / 编辑访客 Key：桌面一个宽对话框（左边基本信息，右边选模型），手机铺满整屏、按钮贴底。
// 建好后同一个框里直接给出「接入地址 + Key」，复制给客人就行。

const dialogCls = 'gap-0 p-0 sm:max-w-[min(56rem,calc(100vw-2rem))] max-sm:inset-0 max-sm:top-0 max-sm:left-0 max-sm:h-[100dvh] max-sm:max-h-[100dvh] max-sm:w-screen max-sm:max-w-none max-sm:translate-x-0 max-sm:translate-y-0 max-sm:rounded-none max-sm:border-0 flex flex-col';

function draftOf(k: GuestKey | null, nowSec: number): { draft: GuestDraft; preset: GuestExpirePreset } {
    if (!k) {
        return {
            draft: { name: '', note: '', models: [], expireAt: presetExpireAt(DEFAULT_GUEST_PRESET, nowSec) ?? 0, cap: '', enabled: true },
            preset: DEFAULT_GUEST_PRESET,
        };
    }
    return {
        draft: { name: k.name, note: k.note ?? '', models: guestModels(k), expireAt: k.expire_at ?? 0, cap: k.max_cost ? String(k.max_cost) : '', enabled: k.enabled },
        preset: 'custom',
    };
}

export function GuestKeyDialog({ open, onOpenChange, apiKey, nowSec }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    /** null = 新建 */
    apiKey: GuestKey | null;
    nowSec: number;
}) {
    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className={dialogCls} data-guest-dialog>
                {open && <Body key={apiKey?.id ?? 'new'} apiKey={apiKey} nowSec={nowSec} onClose={() => onOpenChange(false)} />}
            </DialogContent>
        </Dialog>
    );
}

function Body({ apiKey, nowSec, onClose }: { apiKey: GuestKey | null; nowSec: number; onClose: () => void }) {
    const t = useTranslations('keys.guest.dialog');
    const init = useMemo(() => draftOf(apiKey, nowSec), [apiKey, nowSec]);
    const [draft, setDraft] = useState<GuestDraft>(init.draft);
    const [preset, setPreset] = useState<GuestExpirePreset>(init.preset);
    const [created, setCreated] = useState<GuestKey | null>(null);
    // 对话框打开那一刻（校验和日期下限用；真正的预设到期时间在点保存时按当时算）。
    const [now] = useState(() => Math.floor(Date.now() / 1000));
    const create = useCreateGuestKey();
    const update = useUpdateGuestKey();
    const pending = create.isPending || update.isPending;
    const set = (p: Partial<GuestDraft>) => setDraft((d) => ({ ...d, ...p }));

    if (created) return <Created apiKey={created} onClose={onClose} />;

    const expireChanged = !apiKey || draft.expireAt !== (apiKey.expire_at ?? 0);
    const err = validateGuestDraft(draft, now, expireChanged);
    const submit = () => {
        // 预设按「点保存的这一刻」算，不按打开对话框的时刻。
        const expireAt = preset === 'custom' ? draft.expireAt : presetExpireAt(preset, Math.floor(Date.now() / 1000)) ?? draft.expireAt;
        const body = { name: draft.name.trim(), note: draft.note.trim(), models: draft.models, expire_at: expireAt, max_cost: Number(draft.cap), enabled: draft.enabled };
        const onError = (e: unknown) => toast.error(t('saveFailed'), { description: (e as { message?: string } | null)?.message });
        if (apiKey) {
            update.mutate({ ...body, id: apiKey.id }, { onSuccess: () => { toast.success(t('saved', { name: body.name })); onClose(); }, onError });
        } else {
            create.mutate(body, { onSuccess: (k) => setCreated(k), onError });
        }
    };

    return (
        <>
            <DialogHeader className="border-b border-border/70 px-5 pt-5 pb-4 text-left sm:px-6">
                <DialogTitle>{apiKey ? t('editTitle') : t('createTitle')}</DialogTitle>
                <DialogDescription>{t('desc')}</DialogDescription>
            </DialogHeader>
            <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4 sm:px-6">
                <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
                    <div className="space-y-4">
                        <div>
                            <div className={label}>{t('name')}</div>
                            <input data-guest-name value={draft.name} maxLength={100} onChange={(e) => set({ name: e.target.value })} placeholder={t('namePh')} className={inputCls} />
                        </div>
                        <div>
                            <div className={label}>{t('note')}</div>
                            <textarea value={draft.note} maxLength={500} rows={2} onChange={(e) => set({ note: e.target.value })} placeholder={t('notePh')} className={cn(inputCls, 'h-auto min-h-16 py-2')} />
                        </div>
                        <div>
                            <div className={label}>{t('expire')}<HelpTip content={t('expireTip')} /></div>
                            <div className="grid grid-cols-3 gap-1.5 sm:grid-cols-6">
                                {[...GUEST_EXPIRE_PRESETS.map((p) => p.id), 'custom' as const].map((id) => (
                                    <button key={id} type="button" onClick={() => {
                                        setPreset(id);
                                        if (id !== 'custom') set({ expireAt: presetExpireAt(id, Math.floor(Date.now() / 1000)) ?? 0 });
                                    }} className={cn(chip, 'h-9 justify-center px-2', preset === id && chipOn)}>
                                        {preset === id && <Check className="size-3.5" />}{t(`presets.${id}`)}
                                    </button>
                                ))}
                            </div>
                            {preset === 'custom' && (
                                <input type="datetime-local" value={unixToLocalInput(draft.expireAt)} min={unixToLocalInput(now + 60)} onChange={(e) => set({ expireAt: localInputToUnix(e.target.value) })} className={cn(inputCls, 'mt-2')} />
                            )}
                            <div className="mt-1.5 text-xs text-muted-foreground">
                                {draft.expireAt ? t('expireHint', { at: new Date(draft.expireAt * 1000).toLocaleString([], { year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) }) : t('needExpire')}
                            </div>
                        </div>
                        <div>
                            <div className={label}>{t('cap')}<HelpTip content={t('capTip')} /></div>
                            <div className="relative">
                                <span className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-sm text-muted-foreground">$</span>
                                <input data-guest-cap inputMode="decimal" required aria-required value={draft.cap} onChange={(e) => set({ cap: normalizeMoneyInput(e.target.value) })} placeholder={t('capPh')} className={cn(inputCls, 'pl-6')} />
                            </div>
                            <div className="mt-1.5 text-xs text-muted-foreground">{t('billing')}</div>
                        </div>
                        <label className="flex cursor-pointer items-center justify-between gap-3 rounded-2xl border border-border/70 px-4 py-3">
                            <span className="text-sm">{draft.enabled ? t('enabledOn') : t('enabledOff')}</span>
                            <Switch checked={draft.enabled} onCheckedChange={(v) => set({ enabled: v })} />
                        </label>
                    </div>
                    <ModelPicker models={draft.models} onChange={(models) => set({ models })} />
                </div>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-border/70 px-5 py-3 max-sm:pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:px-6">
                {err && <span className="mr-auto min-w-0 truncate text-xs text-destructive">{t(`err.${err}`)}</span>}
                <button type="button" className={btn} onClick={onClose}>{t('cancel')}</button>
                <button type="button" className={btnPrimary} disabled={!!err || pending} onClick={submit}>
                    {pending && <Loader2 className="size-4 animate-spin" />}{apiKey ? t('save') : t('create')}
                </button>
            </div>
        </>
    );
}

function ModelPicker({ models, onChange }: { models: string[]; onChange: (m: string[]) => void }) {
    const t = useTranslations('keys.guest.dialog');
    const { data: available = [], isLoading } = useGuestKeyModels();
    const [q, setQ] = useState('');
    const list = useMemo(() => {
        const extra = models.filter((m) => !available.includes(m));
        const s = q.trim().toLowerCase();
        return [...extra, ...available].filter((m) => !s || m.toLowerCase().includes(s));
    }, [available, models, q]);
    const toggle = (m: string) => onChange(models.includes(m) ? models.filter((x) => x !== m) : [...models, m]);
    return (
        <section className="flex min-h-0 flex-col rounded-2xl border border-border/70 p-4">
            <div className="mb-2 flex items-center justify-between gap-2">
                <h4 className="flex items-center gap-1 text-sm font-semibold">{t('models')}<HelpTip content={t('modelsTip')} align="end" /></h4>
                <span className="text-xs text-muted-foreground">{t('modelsPicked', { n: models.length })}</span>
            </div>
            <div className="flex items-center gap-2">
                <label className="flex h-9 min-w-0 flex-1 items-center gap-2 rounded-xl border border-input bg-background px-3 text-sm">
                    <Search className="size-4 shrink-0 text-muted-foreground" />
                    <input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('modelsSearch')} className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-muted-foreground" />
                </label>
                {models.length > 0 && <button type="button" className="shrink-0 text-xs text-primary hover:underline" onClick={() => onChange([])}>{t('modelsClear')}</button>}
            </div>
            <div className="mt-2 max-h-72 min-h-40 flex-1 overflow-y-auto rounded-xl border border-border/70 md:max-h-[22rem]">
                {isLoading ? (
                    <div className="flex justify-center py-8 text-muted-foreground"><Loader2 className="size-4 animate-spin" /></div>
                ) : list.length === 0 ? (
                    <div className="px-3 py-8 text-center text-xs text-muted-foreground">{available.length === 0 ? t('modelsNone') : t('modelsNoMatch')}</div>
                ) : list.map((m) => {
                    const on = models.includes(m);
                    const gone = !available.includes(m);
                    return (
                        <button key={m} type="button" onClick={() => toggle(m)} className={cn('flex w-full items-center gap-2 border-b border-border/50 px-3 py-2 text-left text-sm last:border-b-0 hover:bg-muted/50', on && 'bg-primary/5')}>
                            <CheckMark checked={on} />
                            <span className="min-w-0 flex-1 truncate font-mono text-xs">{m}</span>
                            {gone && <span className="shrink-0 text-[11px] text-amber-700 dark:text-amber-400">{t('modelGone')}</span>}
                        </button>
                    );
                })}
            </div>
            <div className="mt-1.5 text-xs text-muted-foreground">{t('modelsHint')}</div>
        </section>
    );
}

function Created({ apiKey, onClose }: { apiKey: GuestKey; onClose: () => void }) {
    const t = useTranslations('keys.guest.dialog');
    const baseUrl = useBaseUrl();
    const [, copy] = useCopyToClipboard();
    const both = `${t('baseUrl')}: ${baseUrl}\nKey: ${apiKey.api_key}`;
    const row = (name: string, value: string) => (
        <div>
            <div className={label}>{name}</div>
            <div className="flex items-center gap-2 rounded-xl border border-border bg-muted/30 px-3 py-2">
                <code className="min-w-0 flex-1 font-mono text-[13px] break-all">{value}</code>
                <button type="button" className={cn(btn, 'h-8 shrink-0 px-2.5')} onClick={async () => { await copy(value); toast.success(t('copied')); }}><Copy className="size-3.5" />{t('copy')}</button>
            </div>
        </div>
    );
    return (
        <>
            <DialogHeader className="border-b border-border/70 px-5 pt-5 pb-4 text-left sm:px-6">
                <DialogTitle className="flex items-center gap-2"><span className="inline-flex size-6 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600"><Check className="size-4" /></span>{t('createdTitle', { name: apiKey.name })}</DialogTitle>
                <DialogDescription>{t('createdHint')}</DialogDescription>
            </DialogHeader>
            <div className="min-h-0 flex-1 space-y-4 overflow-y-auto px-5 py-4 sm:px-6">
                {row(t('baseUrl'), baseUrl)}
                {row('Key', apiKey.api_key)}
                <div className="rounded-xl bg-muted/40 px-3 py-2 text-xs text-muted-foreground">{t('createdModels', { list: guestModels(apiKey).join('、') })}</div>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-border/70 px-5 py-3 max-sm:pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:px-6">
                <button type="button" className={btn} onClick={async () => { await copy(both); toast.success(t('copied')); }}><Copy className="size-4" />{t('copyBoth')}</button>
                <button type="button" className={btnPrimary} onClick={onClose}>{t('done')}</button>
            </div>
        </>
    );
}
