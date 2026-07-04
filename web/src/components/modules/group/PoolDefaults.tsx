'use client';

import { useEffect, useRef, useState, type MutableRefObject } from 'react';
import { useTranslations } from 'next-intl';
import { Clock, HelpCircle, Radio, Zap } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';

// PoolGlobalDefaults surfaces the three model-pool-wide routing defaults right at the
// top of the Model Pool page (instead of burying them deep in System settings), so an
// admin managing the pool sees them at a glance next to the per-group config:
//   - route-mode override (forces every group to spread / fill-first)
//   - global first-token timeout default (fallback when a group's own is 0)
//   - global session-keep default (fallback sticky window when a group's own is 0)
// All three are backend settings; this component reads/writes the same keys the relay
// resolves in applyGroupGlobalDefaults / the balancer session fallback.
export function PoolGlobalDefaults() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [routeMode, setRouteMode] = useState('');
    const [firstToken, setFirstToken] = useState('0');
    const [sessionKeep, setSessionKeep] = useState('0');
    const initialRouteMode = useRef('');
    const initialFirstToken = useRef('0');
    const initialSessionKeep = useRef('0');

    useEffect(() => {
        if (!settings) return;
        const rm = settings.find((s) => s.key === SettingKey.RouteModeOverride);
        const ft = settings.find((s) => s.key === SettingKey.FirstTokenTimeOutDefault);
        const sk = settings.find((s) => s.key === SettingKey.SessionKeepTimeDefault);
        if (rm) { setRouteMode(rm.value || ''); initialRouteMode.current = rm.value || ''; }
        if (ft) { setFirstToken(ft.value || '0'); initialFirstToken.current = ft.value || '0'; }
        if (sk) { setSessionKeep(sk.value || '0'); initialSessionKeep.current = sk.value || '0'; }
    }, [settings]);

    const commit = (key: string, value: string, ref: MutableRefObject<string>) => {
        if (value === ref.current) return;
        setSetting.mutate({ key, value }, {
            onSuccess: () => { ref.current = value; toast.success(t('saved')); },
            onError: (error) => toast.error((error as Error).message),
        });
    };

    const commitNumeric = (
        raw: string,
        setState: (value: string) => void,
        ref: MutableRefObject<string>,
        key: string,
        invalidKey: string,
    ) => {
        const trimmed = raw.trim();
        const numericValue = Number(trimmed);
        if (!trimmed || !Number.isInteger(numericValue) || numericValue < 0) {
            setState(ref.current || '0');
            toast.error(t(invalidKey));
            return;
        }
        const normalized = String(numericValue);
        setState(normalized);
        commit(key, normalized, ref);
    };

    return (
        <section className="rounded-2xl border border-border/60 bg-card/60 p-3 sm:p-4">
            <div className="mb-3 flex items-center gap-2">
                <span className="text-sm font-semibold text-card-foreground">{t('poolDefaults.title')}</span>
                <TooltipProvider>
                    <Tooltip>
                        <TooltipTrigger asChild>
                            <HelpCircle className="size-4 cursor-help text-muted-foreground" />
                        </TooltipTrigger>
                        <TooltipContent className="max-w-xs">{t('poolDefaults.hint')}</TooltipContent>
                    </Tooltip>
                </TooltipProvider>
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
                <div className="flex items-center justify-between gap-2 rounded-xl border border-border/40 bg-background/50 px-3 py-2">
                    <div className="flex min-w-0 items-center gap-2">
                        <Zap className="size-4 shrink-0 text-muted-foreground" />
                        <span className="truncate text-xs font-medium">{t('routeModeOverride.label')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-3.5 shrink-0 cursor-help text-muted-foreground" />
                                </TooltipTrigger>
                                <TooltipContent className="max-w-xs">{t('routeModeOverride.hint')}</TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <select
                        value={routeMode}
                        onChange={(event) => {
                            const value = event.target.value;
                            setRouteMode(value);
                            commit(SettingKey.RouteModeOverride, value, initialRouteMode);
                        }}
                        aria-label={t('routeModeOverride.label')}
                        className="h-8 shrink-0 rounded-lg border border-input bg-background px-2 text-xs text-foreground"
                    >
                        <option value="">{t('routeModeOverride.followGroup')}</option>
                        <option value="spread">{t('routeModeOverride.spread')}</option>
                        <option value="fill_first">{t('routeModeOverride.fillFirst')}</option>
                    </select>
                </div>

                <div className="flex items-center justify-between gap-2 rounded-xl border border-border/40 bg-background/50 px-3 py-2">
                    <div className="flex min-w-0 items-center gap-2">
                        <Clock className="size-4 shrink-0 text-muted-foreground" />
                        <span className="truncate text-xs font-medium">{t('firstTokenTimeOutDefault.label')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-3.5 shrink-0 cursor-help text-muted-foreground" />
                                </TooltipTrigger>
                                <TooltipContent className="max-w-xs">{t('firstTokenTimeOutDefault.hint')}</TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <div className="flex shrink-0 items-center gap-1">
                        <Input
                            type="number"
                            min="0"
                            step="1"
                            inputMode="numeric"
                            value={firstToken}
                            onChange={(event) => setFirstToken(event.target.value)}
                            onBlur={() => commitNumeric(firstToken, setFirstToken, initialFirstToken, SettingKey.FirstTokenTimeOutDefault, 'firstTokenTimeOutDefault.invalid')}
                            aria-label={t('firstTokenTimeOutDefault.label')}
                            className="h-8 w-16 rounded-lg text-xs"
                        />
                        <span className="text-xs text-muted-foreground">{t('firstTokenTimeOutDefault.seconds')}</span>
                    </div>
                </div>

                <div className="flex items-center justify-between gap-2 rounded-xl border border-border/40 bg-background/50 px-3 py-2">
                    <div className="flex min-w-0 items-center gap-2">
                        <Radio className="size-4 shrink-0 text-muted-foreground" />
                        <span className="truncate text-xs font-medium">{t('sessionKeepTimeDefault.label')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-3.5 shrink-0 cursor-help text-muted-foreground" />
                                </TooltipTrigger>
                                <TooltipContent className="max-w-xs">{t('sessionKeepTimeDefault.hint')}</TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <div className="flex shrink-0 items-center gap-1">
                        <Input
                            type="number"
                            min="0"
                            step="1"
                            inputMode="numeric"
                            value={sessionKeep}
                            onChange={(event) => setSessionKeep(event.target.value)}
                            onBlur={() => commitNumeric(sessionKeep, setSessionKeep, initialSessionKeep, SettingKey.SessionKeepTimeDefault, 'sessionKeepTimeDefault.invalid')}
                            aria-label={t('sessionKeepTimeDefault.label')}
                            className="h-8 w-16 rounded-lg text-xs"
                        />
                        <span className="text-xs text-muted-foreground">{t('sessionKeepTimeDefault.seconds')}</span>
                    </div>
                </div>
            </div>
        </section>
    );
}
