'use client';

import { useEffect, useRef, useState } from 'react';
import { useTranslations } from 'next-intl';
import { useQueryClient } from '@tanstack/react-query';
import { ShieldAlert } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import {
    USER_ACCESS_ALERT_LEVELS,
    useUserAccessConfig,
    type UserAccessAlertConfig,
    type UserAccessAlertLevel,
} from '@/api/endpoints/user';
import { useAccessRuleLines } from '@/components/modules/user/components/user-access';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { cn } from '@/lib/utils';

// 「自定义」五个数的本地草稿（字符串，跟输入框一致）。
type CustomDraft = { windowMin: string; manyIPs: string; newIP: boolean; newClient: boolean; ipv4Prefix: string };

const CUSTOM_KEYS = {
    windowMin: SettingKey.UserAccessAlertWindowMin,
    manyIPs: SettingKey.UserAccessAlertManyIPs,
    newIP: SettingKey.UserAccessAlertNewIP,
    newClient: SettingKey.UserAccessAlertNewClient,
    ipv4Prefix: SettingKey.UserAccessAlertIPv4Prefix,
} as const;

function isLevel(v: string | undefined): v is UserAccessAlertLevel {
    return !!v && (USER_ACCESS_ALERT_LEVELS as readonly string[]).includes(v);
}

/** 与后端 Setting.Validate 同一套范围。 */
export function customDraftValid(field: 'windowMin' | 'manyIPs', value: string): boolean {
    if (!/^\d+$/.test(value.trim())) return false;
    const n = Number(value);
    if (field === 'windowMin') return n >= 5 && n <= 1440;
    return n === 0 || (n >= 2 && n <= 100);
}

export function draftToConfig(d: CustomDraft): UserAccessAlertConfig {
    return {
        level: 'custom',
        window_sec: Number(d.windowMin) * 60,
        many_ips: Number(d.manyIPs),
        new_ip: d.newIP,
        new_client: d.newClient,
        ipv4_prefix: d.ipv4Prefix === '24' ? 24 : 32,
    };
}

export function SettingUserAccessAlert() {
    const t = useTranslations('setting.userAccess');
    const tSetting = useTranslations('setting');
    const tLevel = useTranslations('user.access.level');
    const ruleLines = useAccessRuleLines();
    const queryClient = useQueryClient();
    const { data: settings } = useSettingList();
    const { data: config } = useUserAccessConfig();
    const setSetting = useSetSetting();

    const [level, setLevel] = useState<UserAccessAlertLevel>('standard');
    const [draft, setDraft] = useState<CustomDraft>({ windowMin: '60', manyIPs: '3', newIP: true, newClient: true, ipv4Prefix: '32' });
    // 最近一次从服务端读到的值：失败回滚、判断「没改」都看它。
    const [savedDraft, setSavedDraft] = useState<CustomDraft | null>(null);
    const lastSeen = useRef('');

    useEffect(() => {
        if (!settings) return;
        const get = (key: string) => settings.find((s) => s.key === key)?.value;
        const rawLevel = get(SettingKey.UserAccessAlertLevel);
        const next = {
            level: isLevel(rawLevel) ? rawLevel : 'standard',
            draft: {
                windowMin: get(CUSTOM_KEYS.windowMin) ?? '60',
                manyIPs: get(CUSTOM_KEYS.manyIPs) ?? '3',
                newIP: (get(CUSTOM_KEYS.newIP) ?? 'true') === 'true',
                newClient: (get(CUSTOM_KEYS.newClient) ?? 'true') === 'true',
                ipv4Prefix: get(CUSTOM_KEYS.ipv4Prefix) === '24' ? '24' : '32',
            },
        };
        const sig = JSON.stringify(next);
        if (sig === lastSeen.current) return;
        lastSeen.current = sig;
        queueMicrotask(() => {
            setLevel(next.level);
            setDraft(next.draft);
            setSavedDraft(next.draft);
        });
    }, [settings]);

    const save = (key: string, value: string, onFail: () => void) => {
        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(tSetting('saved'));
                // 用户页的提示和抽屉按新阈值重算。
                queryClient.invalidateQueries({ queryKey: ['users', 'access'] });
            },
            onError: () => {
                toast.error(tSetting('saveFailed'));
                onFail();
            },
        });
    };

    const pickLevel = (next: UserAccessAlertLevel) => {
        if (next === level || setSetting.isPending) return;
        const prev = level;
        setLevel(next);
        save(SettingKey.UserAccessAlertLevel, next, () => setLevel(prev));
    };

    const saveNumber = (field: 'windowMin' | 'manyIPs') => {
        const value = draft[field].trim();
        if (savedDraft && value === savedDraft[field]) return;
        const revert = () => setDraft((d) => ({ ...d, [field]: savedDraft?.[field] ?? d[field] }));
        if (!customDraftValid(field, value)) {
            toast.error(t('outOfRange'));
            revert();
            return;
        }
        save(CUSTOM_KEYS[field], String(Number(value)), revert);
    };
    const saveFlag = (field: 'newIP' | 'newClient', value: boolean) => {
        setDraft((d) => ({ ...d, [field]: value }));
        save(CUSTOM_KEYS[field], String(value), () => setDraft((d) => ({ ...d, [field]: !value })));
    };
    const saveIPv4 = (value: '32' | '24') => {
        if (value === draft.ipv4Prefix) return;
        const prev = draft.ipv4Prefix;
        setDraft((d) => ({ ...d, ipv4Prefix: value }));
        save(CUSTOM_KEYS.ipv4Prefix, value, () => setDraft((d) => ({ ...d, ipv4Prefix: prev })));
    };

    const levelConfig = (l: UserAccessAlertLevel): UserAccessAlertConfig | undefined => {
        if (l !== 'custom') return config?.presets?.[l];
        const valid = customDraftValid('windowMin', draft.windowMin) && customDraftValid('manyIPs', draft.manyIPs);
        return valid ? draftToConfig(draft) : (savedDraft ? draftToConfig(savedDraft) : undefined);
    };
    const activeLines = ruleLines(levelConfig(level));

    return (
        <div data-setting-user-access className="min-w-0 space-y-4 rounded-3xl border border-border bg-card p-4 sm:p-6">
            <h2 className="flex min-w-0 items-center gap-2 text-lg font-bold text-card-foreground">
                <ShieldAlert className="h-5 w-5 shrink-0" />
                {t('title')}
            </h2>
            <p className="text-xs leading-5 text-muted-foreground">{t('hint')}</p>

            <div className="space-y-2">
                <div className="text-sm font-medium">{t('level')}</div>
                <TooltipProvider>
                    <div role="radiogroup" aria-label={t('level')} className="grid gap-2 sm:grid-cols-5">
                        {USER_ACCESS_ALERT_LEVELS.map((l) => {
                            const active = l === level;
                            const lines = ruleLines(levelConfig(l));
                            return (
                                // 浮层内容在打开时才取：按内容做 key，预设阈值加载完 / 自定义改了数后浮层跟着换。
                                <Tooltip key={`${l}:${lines.join('|')}`}>
                                    <TooltipTrigger asChild>
                                        <button
                                            type="button"
                                            role="radio"
                                            aria-checked={active}
                                            data-level={l}
                                            onClick={() => pickLevel(l)}
                                            className={cn(
                                                'flex min-w-0 items-center justify-between gap-2 rounded-2xl border px-3 py-2.5 text-left transition-colors sm:flex-col sm:items-start sm:justify-start sm:gap-0.5',
                                                active
                                                    ? 'border-primary bg-primary/10 text-foreground ring-1 ring-primary/40'
                                                    : 'border-border bg-background hover:bg-muted/60',
                                            )}
                                        >
                                            <span className="text-sm font-semibold">{tLevel(l)}</span>
                                            <span className={cn('text-xs', active ? 'text-primary' : 'text-muted-foreground')}>{t(`tagline.${l}`)}</span>
                                        </button>
                                    </TooltipTrigger>
                                    <TooltipContent className="max-w-xs">
                                        <span className="block space-y-1">
                                            <span className="block font-medium">{tLevel(l)} · {t('flags')}</span>
                                            {lines.map((line) => <span key={line} className="block">· {line}</span>)}
                                        </span>
                                    </TooltipContent>
                                </Tooltip>
                            );
                        })}
                    </div>
                </TooltipProvider>
            </div>

            <div data-access-rule className="rounded-2xl border border-border/70 bg-muted/40 px-4 py-3 text-sm">
                <div className="font-medium">{tLevel(level)} · {t('flags')}</div>
                <ul className="mt-1.5 space-y-1 text-muted-foreground">
                    {activeLines.map((line) => (
                        <li key={line} className="flex gap-2"><span aria-hidden>·</span><span className="min-w-0">{line}</span></li>
                    ))}
                </ul>
            </div>

            {level === 'custom' && (
                <div data-access-custom className="grid gap-3 border-t border-border/60 pt-4 sm:grid-cols-2">
                    <label className="flex items-center justify-between gap-3">
                        <span className="min-w-0">
                            <span className="block text-sm font-medium">{t('custom.window')}</span>
                            <span className="block text-xs text-muted-foreground">{t('custom.windowHint')}</span>
                        </span>
                        <Input
                            type="number"
                            inputMode="numeric"
                            min={5}
                            max={1440}
                            value={draft.windowMin}
                            onChange={(e) => setDraft((d) => ({ ...d, windowMin: e.target.value }))}
                            onBlur={() => saveNumber('windowMin')}
                            className="w-28 shrink-0 rounded-xl"
                        />
                    </label>
                    <label className="flex items-center justify-between gap-3">
                        <span className="min-w-0">
                            <span className="block text-sm font-medium">{t('custom.manyIPs')}</span>
                            <span className="block text-xs text-muted-foreground">{t('custom.manyIPsHint')}</span>
                        </span>
                        <Input
                            type="number"
                            inputMode="numeric"
                            min={0}
                            max={100}
                            value={draft.manyIPs}
                            onChange={(e) => setDraft((d) => ({ ...d, manyIPs: e.target.value }))}
                            onBlur={() => saveNumber('manyIPs')}
                            className="w-28 shrink-0 rounded-xl"
                        />
                    </label>
                    <div className="flex items-center justify-between gap-3">
                        <span className="text-sm font-medium">{t('custom.newIP')}</span>
                        <Switch checked={draft.newIP} onCheckedChange={(v) => saveFlag('newIP', v)} aria-label={t('custom.newIP')} />
                    </div>
                    <div className="flex items-center justify-between gap-3">
                        <span className="text-sm font-medium">{t('custom.newClient')}</span>
                        <Switch checked={draft.newClient} onCheckedChange={(v) => saveFlag('newClient', v)} aria-label={t('custom.newClient')} />
                    </div>
                    <div className="flex flex-wrap items-center justify-between gap-2 sm:col-span-2">
                        <span className="text-sm font-medium">{t('custom.ipv4')}</span>
                        <div role="radiogroup" aria-label={t('custom.ipv4')} className="inline-flex rounded-xl border border-border bg-background p-0.5">
                            {(['32', '24'] as const).map((v) => (
                                <button
                                    key={v}
                                    type="button"
                                    role="radio"
                                    aria-checked={draft.ipv4Prefix === v}
                                    onClick={() => saveIPv4(v)}
                                    className={cn(
                                        'rounded-lg px-3 py-1 text-xs font-medium transition-colors',
                                        draft.ipv4Prefix === v ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:text-foreground',
                                    )}
                                >
                                    {v === '32' ? t('custom.ipv4Full') : t('custom.ipv4Net')}
                                </button>
                            ))}
                        </div>
                    </div>
                </div>
            )}

            <p className="text-xs text-muted-foreground">{t('note')}</p>
        </div>
    );
}
