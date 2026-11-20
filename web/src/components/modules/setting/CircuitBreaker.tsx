'use client';

import { useEffect, useState, useRef } from 'react';
import { useTranslations } from 'next-intl';
import { Zap, Hash, Timer, TimerOff, HelpCircle, ListFilter } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';

export function SettingCircuitBreaker() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [threshold, setThreshold] = useState('');
    const [cooldown, setCooldown] = useState('');
    const [maxCooldown, setMaxCooldown] = useState('');

    const initialThreshold = useRef('');
    const initialCooldown = useRef('');
    const initialMaxCooldown = useRef('');
    const [rules, setRules] = useState('');
    const initialRules = useRef('');

    useEffect(() => {
        if (settings) {
            const th = settings.find(s => s.key === SettingKey.CircuitBreakerThreshold);
            const cd = settings.find(s => s.key === SettingKey.CircuitBreakerCooldown);
            const mcd = settings.find(s => s.key === SettingKey.CircuitBreakerMaxCooldown);
            const rl = settings.find(s => s.key === SettingKey.CircuitBreakerErrorRules);
            if (rl && rl.value !== initialRules.current) {
                queueMicrotask(() => setRules(prettyRules(rl.value)));
                initialRules.current = rl.value;
            }
            if (th) {
                if (th.value !== initialThreshold.current) {
                    queueMicrotask(() => setThreshold(th.value));
                    initialThreshold.current = th.value;
                }
            }
            if (cd) {
                if (cd.value !== initialCooldown.current) {
                    queueMicrotask(() => setCooldown(cd.value));
                    initialCooldown.current = cd.value;
                }
            }
            if (mcd) {
                if (mcd.value !== initialMaxCooldown.current) {
                    queueMicrotask(() => setMaxCooldown(mcd.value));
                    initialMaxCooldown.current = mcd.value;
                }
            }
        }
    }, [settings]);

    const handleSave = (key: string, value: string, initialValue: string) => {
        if (value === initialValue) return;
        if (setSetting.isPending) return;

        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(t('saved'));
                if (key === SettingKey.CircuitBreakerThreshold) {
                    initialThreshold.current = value;
                } else if (key === SettingKey.CircuitBreakerCooldown) {
                    initialCooldown.current = value;
                } else if (key === SettingKey.CircuitBreakerMaxCooldown) {
                    initialMaxCooldown.current = value;
                }
            },
            onError: () => {
                // 合法值也可能因 401/403/500/断网而失败：必须可见，且不能假装已保存。
                // initial ref 未变，展示值回滚到已确认值后，再次失焦即可重试。
                toast.error(t('saveFailed'));
                if (key === SettingKey.CircuitBreakerThreshold) {
                    setThreshold(initialThreshold.current);
                } else if (key === SettingKey.CircuitBreakerCooldown) {
                    setCooldown(initialCooldown.current);
                } else if (key === SettingKey.CircuitBreakerMaxCooldown) {
                    setMaxCooldown(initialMaxCooldown.current);
                }
            }
        });
    };

    const handleSaveRules = () => {
        if (setSetting.isPending) return;
        let compact = '[]';
        try {
            const parsed = rules.trim() === '' ? [] : JSON.parse(rules);
            if (!Array.isArray(parsed)) throw new Error('not an array');
            compact = JSON.stringify(parsed);
        } catch {
            toast.error(t('circuitBreaker.rules.invalid'));
            return;
        }
        if (compact === initialRules.current) return;
        setSetting.mutate({ key: SettingKey.CircuitBreakerErrorRules, value: compact }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialRules.current = compact;
            },
            // 服务端会严格校验（scope / cooldown_seconds / 条件），失败时保留输入方便修改。
            onError: (error) => toast.error(error?.message || t('saveFailed')),
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <Zap className="h-5 w-5" />
                {t('circuitBreaker.title')}
                <TooltipProvider>
                    <Tooltip>
                        <TooltipTrigger asChild>
                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                        </TooltipTrigger>
                        <TooltipContent>
                            {t('circuitBreaker.hint')}
                        </TooltipContent>
                    </Tooltip>
                </TooltipProvider>
            </h2>

            {/* 熔断触发阈值 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Hash className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('circuitBreaker.threshold.label')}</span>
                </div>
                <Input
                    type="number"
                    value={threshold}
                    onChange={(e) => setThreshold(e.target.value)}
                    onBlur={() => handleSave(SettingKey.CircuitBreakerThreshold, threshold, initialThreshold.current)}
                    placeholder={t('circuitBreaker.threshold.placeholder')}
                    className="w-32 shrink-0 rounded-xl"
                />
            </div>

            {/* 基础冷却时间 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Timer className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('circuitBreaker.cooldown.label')}</span>
                </div>
                <Input
                    type="number"
                    value={cooldown}
                    onChange={(e) => setCooldown(e.target.value)}
                    onBlur={() => handleSave(SettingKey.CircuitBreakerCooldown, cooldown, initialCooldown.current)}
                    placeholder={t('circuitBreaker.cooldown.placeholder')}
                    className="w-32 shrink-0 rounded-xl"
                />
            </div>

            {/* 最大冷却时间 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <TimerOff className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('circuitBreaker.maxCooldown.label')}</span>
                </div>
                <Input
                    type="number"
                    value={maxCooldown}
                    onChange={(e) => setMaxCooldown(e.target.value)}
                    onBlur={() => handleSave(SettingKey.CircuitBreakerMaxCooldown, maxCooldown, initialMaxCooldown.current)}
                    placeholder={t('circuitBreaker.maxCooldown.placeholder')}
                    className="w-32 shrink-0 rounded-xl"
                />
            </div>

            {/* 错误规则：状态码/关键字 → 按模型/key/渠道立即冷却 */}
            <div className="space-y-2">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <ListFilter className="h-5 w-5 text-muted-foreground" />
                        <span className="text-sm font-medium">{t('circuitBreaker.rules.label')}</span>
                    </div>
                    <Button size="sm" variant="outline" className="rounded-xl" onClick={handleSaveRules} disabled={setSetting.isPending}>
                        {t('circuitBreaker.rules.save')}
                    </Button>
                </div>
                <p className="text-xs text-muted-foreground">{t('circuitBreaker.rules.hint')}</p>
                <textarea
                    value={rules}
                    onChange={(e) => setRules(e.target.value)}
                    placeholder={RULES_PLACEHOLDER}
                    spellCheck={false}
                    rows={8}
                    className="w-full rounded-xl border border-border bg-background px-3 py-2 font-mono text-xs"
                />
            </div>
        </div>
    );
}

const RULES_PLACEHOLDER = `[
  {"name": "quota", "keywords": ["insufficient_quota"], "scope": "key", "cooldown_seconds": 1800},
  {"name": "not-found", "status_codes": [404], "keywords": ["model_not_found"], "scope": "model", "cooldown_seconds": 600}
]`;

function prettyRules(value: string): string {
    try {
        const parsed = JSON.parse(value || '[]');
        return Array.isArray(parsed) && parsed.length === 0 ? '' : JSON.stringify(parsed, null, 2);
    } catch {
        return value;
    }
}
