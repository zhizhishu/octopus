'use client';

import { useEffect, useState, useRef } from 'react';
import { useTranslations } from 'next-intl';
import { Zap, Hash, Timer, TimerOff, HelpCircle, ListFilter, Radar, ChevronDown, ChevronUp } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useProbeStats } from '@/api/endpoints/channel';
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
    const [auditConfig, setAuditConfig] = useState('');
    const initialAuditConfig = useRef('');

    // 主动探测（P3）：开关存 'true'/'false'，上限/停手次数存十进制整数（与后端 seed 默认值同形）。
    const [probeOpen, setProbeOpen] = useState(false);
    const [probeEnabled, setProbeEnabled] = useState('false');
    const [probeDailyCap, setProbeDailyCap] = useState('');
    const [probeChannelCap, setProbeChannelCap] = useState('');
    const [probeMaxFailures, setProbeMaxFailures] = useState('');
    const [probeEmbeddings, setProbeEmbeddings] = useState('false');
    const initialProbeEnabled = useRef('false');
    const initialProbeDailyCap = useRef('');
    const initialProbeChannelCap = useRef('');
    const initialProbeMaxFailures = useRef('');
    const initialProbeEmbeddings = useRef('false');
    // 今日用量：探测统计不在 settings 列表里，单独查（后端偶尔没给就显示 —，不编 0）。
    const { data: probeStats } = useProbeStats();

    useEffect(() => {
        if (settings) {
            const th = settings.find(s => s.key === SettingKey.CircuitBreakerThreshold);
            const cd = settings.find(s => s.key === SettingKey.CircuitBreakerCooldown);
            const mcd = settings.find(s => s.key === SettingKey.CircuitBreakerMaxCooldown);
            const rl = settings.find(s => s.key === SettingKey.CircuitBreakerErrorRules);
            const ed = settings.find(s => s.key === SettingKey.CircuitProbeEnabled);
            const dc = settings.find(s => s.key === SettingKey.CircuitProbeDailyCap);
            const cc = settings.find(s => s.key === SettingKey.CircuitProbeChannelDailyCap);
            const mf = settings.find(s => s.key === SettingKey.CircuitProbeMaxConsecutiveFailures);
            const em = settings.find(s => s.key === SettingKey.CircuitProbeEmbeddingsEnabled);
            if (em && em.value !== initialProbeEmbeddings.current) {
                queueMicrotask(() => setProbeEmbeddings(em.value));
                initialProbeEmbeddings.current = em.value;
            }
            if (ed && ed.value !== initialProbeEnabled.current) {
                queueMicrotask(() => setProbeEnabled(ed.value));
                initialProbeEnabled.current = ed.value;
            }
            if (dc && dc.value !== initialProbeDailyCap.current) {
                queueMicrotask(() => setProbeDailyCap(dc.value));
                initialProbeDailyCap.current = dc.value;
            }
            if (cc && cc.value !== initialProbeChannelCap.current) {
                queueMicrotask(() => setProbeChannelCap(cc.value));
                initialProbeChannelCap.current = cc.value;
            }
            if (mf && mf.value !== initialProbeMaxFailures.current) {
                queueMicrotask(() => setProbeMaxFailures(mf.value));
                initialProbeMaxFailures.current = mf.value;
            }
            if (rl && rl.value !== initialRules.current) {
                queueMicrotask(() => setRules(prettyRules(rl.value)));
                initialRules.current = rl.value;
            }
            const ra = settings.find(s => s.key === 'request_audit');
            if (ra && ra.value !== initialAuditConfig.current) {
                queueMicrotask(() => setAuditConfig(ra.value));
                initialAuditConfig.current = ra.value;
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

        // 探测的四个数值/开关字段共用同一套「确认值 / 回滚」规则，不另建一套。
        const probeField = (() => {
            if (key === SettingKey.CircuitProbeDailyCap) {
                return { ref: initialProbeDailyCap, set: setProbeDailyCap };
            }
            if (key === SettingKey.CircuitProbeChannelDailyCap) {
                return { ref: initialProbeChannelCap, set: setProbeChannelCap };
            }
            if (key === SettingKey.CircuitProbeMaxConsecutiveFailures) {
                return { ref: initialProbeMaxFailures, set: setProbeMaxFailures };
            }
            return null;
        })();

        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(t('saved'));
                if (key === SettingKey.CircuitBreakerThreshold) {
                    initialThreshold.current = value;
                } else if (key === SettingKey.CircuitBreakerCooldown) {
                    initialCooldown.current = value;
                } else if (key === SettingKey.CircuitBreakerMaxCooldown) {
                    initialMaxCooldown.current = value;
                } else if (probeField) {
                    probeField.ref.current = value;
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
                } else if (probeField) {
                    probeField.set(probeField.ref.current);
                }
            }
        });
    };

    // 开关立即生效（和 System.tsx 的开关同一套写法）：失败就回滚到已确认值。
    const handleProbeSwitch = (key: string, checked: boolean, initial: { current: string }, setter: (v: string) => void) => {
        const next = checked ? 'true' : 'false';
        if (setSetting.isPending) return;
        setter(next);
        if (next === initial.current) return;
        setSetting.mutate({ key, value: next }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initial.current = next;
            },
            onError: () => {
                toast.error(t('saveFailed'));
                setter(initial.current);
            },
        });
    };

    /** 探测上限/停手次数：负数归 0，非数字回落到已确认值（与 Log.tsx 数值键同一口径）。 */
    const saveProbeNumber = (key: string, raw: string, initial: { current: string }, setter: (v: string) => void) => {
        const parsed = Number(raw);
        const fallback = initial.current === '' ? '0' : initial.current;
        const next = Number.isFinite(parsed) ? String(Math.max(0, Math.trunc(parsed))) : fallback;
        if (next !== raw) setter(next);
        handleSave(key, next, initial.current);
    };

    // 上游前审查配置（request_audit）：JSON 编辑 + 客户端预检，与后端
    // requestaudit.ParseConfig 同口径（id 唯一/非空、instructions 非空、
    // action ∈ block|warn、threshold ∈ (0,1]）；服务端 Validate 是最终判官。
    const handleSaveAuditConfig = () => {
        if (setSetting.isPending) return;
        const raw = auditConfig.trim();
        let compact = '';
        if (raw !== '') {
            try {
                const parsed = JSON.parse(raw);
                const rules = Array.isArray(parsed.rules) ? parsed.rules : [];
                const seen = new Set();
                for (const r of rules) {
                    if (!r.id || seen.has(r.id)) throw new Error('bad rule id');
                    seen.add(r.id);
                    if (!String(r.instructions ?? '').trim()) throw new Error('empty instructions');
                    const action = r.action ?? 'block';
                    if (action !== 'block' && action !== 'warn') throw new Error('bad action');
                    const th = r.threshold ?? 0.8;
                    if (!(th > 0 && th <= 1)) throw new Error('bad threshold');
                }
                compact = JSON.stringify(parsed);
            } catch {
                toast.error(t('circuitBreaker.rules.invalid'));
                return;
            }
        }
        if (compact === initialAuditConfig.current) return;
        setSetting.mutate({ key: 'request_audit', value: compact }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialAuditConfig.current = compact;
            },
            onError: (error) => toast.error(error?.message || t('saveFailed')),
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

    // 折叠标题行的摘要：开关状态 + 今日已用/上限，一眼就能判断要不要点开。
    // 取不到统计时显示破折号，不拿 0 冒充「今天一次都没探」。
    const probeCapFromSettings = Number(probeDailyCap);
    const probeTotals = probeStats?.totals ?? {
        used: 0,
        cap: Number.isFinite(probeCapFromSettings) ? probeCapFromSettings : 0,
        recovered: 0,
        failed: 0,
        skipped: 0,
    };
    const probeUsedText = probeStats ? String(probeTotals.used) : '—';
    const probeUsageNumbers = probeTotals.cap > 0 ? `${probeUsedText} / ${probeTotals.cap}` : probeUsedText;
    const probeUsageWidth = probeTotals.cap > 0
        ? Math.min(100, Math.round((probeTotals.used / probeTotals.cap) * 1000) / 10)
        : 0;
    const probeSummary = `${probeEnabled === 'true' ? t('circuitBreaker.probe.summaryOn') : t('circuitBreaker.probe.summaryOff')} · ${t('circuitBreaker.probe.usedToday')} ${probeUsageNumbers}`;

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

            {/* 上游前风控审查（request_audit）：默认全关；审查渠道 id + 规则 JSON。 */}
            <div className="space-y-2">
                <div className="flex items-center justify-between gap-4">
                    <span className="text-sm font-medium">{t('circuitBreaker.audit.label')}</span>
                    <Button size="sm" variant="outline" className="rounded-xl" onClick={handleSaveAuditConfig} disabled={setSetting.isPending}>
                        {t('circuitBreaker.rules.save')}
                    </Button>
                </div>
                <p className="text-xs text-muted-foreground">{t('circuitBreaker.audit.hint')}</p>
                <textarea
                    value={auditConfig}
                    onChange={(e) => setAuditConfig(e.target.value)}
                    placeholder='{"enabled":true,"channel_id":68,"rules":[{"id":"r1","name":"violence","instructions":"是否含违规内容","action":"block","threshold":0.8}]}'
                    spellCheck={false}
                    rows={5}
                    className="w-full rounded-xl border border-border bg-background px-3 py-2 font-mono text-xs"
                />
            </div>

            {/* 主动探测（P3）：整块可折叠，标题行就是摘要（开关状态 + 今日已用/上限）。 */}
            <div className="rounded-2xl border border-border bg-muted/20">
                <button
                    type="button"
                    onClick={() => setProbeOpen((open) => !open)}
                    aria-expanded={probeOpen}
                    aria-label={t('circuitBreaker.probe.title')}
                    className="flex w-full min-w-0 flex-wrap items-center gap-x-2 gap-y-1 rounded-2xl px-3 py-2.5 text-left transition-colors hover:bg-muted/40 max-sm:min-h-11"
                >
                    <Radar className="size-4 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('circuitBreaker.probe.title')}</span>
                    <span
                        className="min-w-0 flex-1 truncate text-xs text-muted-foreground sm:text-right"
                        data-probe-summary
                    >
                        {probeSummary}
                    </span>
                    {probeOpen
                        ? <ChevronUp className="size-4 shrink-0 text-muted-foreground" />
                        : <ChevronDown className="size-4 shrink-0 text-muted-foreground" />}
                </button>

                {probeOpen && (
                    <div className="space-y-4 border-t border-border/60 px-3 pb-3 pt-3">
                        {/* 总开关 */}
                        <div className="flex items-start justify-between gap-4">
                            <div className="min-w-0 space-y-1">
                                <div className="text-sm font-medium">{t('circuitBreaker.probe.master.label')}</div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('circuitBreaker.probe.master.hint')}</p>
                            </div>
                            <Switch
                                checked={probeEnabled === 'true'}
                                onCheckedChange={(checked) => handleProbeSwitch(SettingKey.CircuitProbeEnabled, checked, initialProbeEnabled, setProbeEnabled)}
                                aria-label={t('circuitBreaker.probe.master.label')}
                                className="shrink-0"
                            />
                        </div>

                        {/* 全站每日上限 */}
                        <div className="flex items-center justify-between gap-4">
                            <div className="min-w-0 space-y-1">
                                <div className="text-sm font-medium">{t('circuitBreaker.probe.dailyCap.label')}</div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('circuitBreaker.probe.dailyCap.hint')}</p>
                            </div>
                            <Input
                                type="number"
                                min={0}
                                value={probeDailyCap}
                                onChange={(e) => setProbeDailyCap(e.target.value)}
                                onBlur={() => saveProbeNumber(SettingKey.CircuitProbeDailyCap, probeDailyCap, initialProbeDailyCap, setProbeDailyCap)}
                                placeholder={t('circuitBreaker.probe.dailyCap.placeholder')}
                                aria-label={t('circuitBreaker.probe.dailyCap.label')}
                                className="w-28 shrink-0 rounded-xl"
                            />
                        </div>

                        {/* 单渠道每日上限 */}
                        <div className="flex items-center justify-between gap-4">
                            <div className="min-w-0 space-y-1">
                                <div className="text-sm font-medium">{t('circuitBreaker.probe.channelCap.label')}</div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('circuitBreaker.probe.channelCap.hint')}</p>
                            </div>
                            <Input
                                type="number"
                                min={0}
                                value={probeChannelCap}
                                onChange={(e) => setProbeChannelCap(e.target.value)}
                                onBlur={() => saveProbeNumber(SettingKey.CircuitProbeChannelDailyCap, probeChannelCap, initialProbeChannelCap, setProbeChannelCap)}
                                placeholder={t('circuitBreaker.probe.channelCap.placeholder')}
                                aria-label={t('circuitBreaker.probe.channelCap.label')}
                                className="w-28 shrink-0 rounded-xl"
                            />
                        </div>

                        {/* 连续失败停手 */}
                        <div className="flex items-center justify-between gap-4">
                            <div className="min-w-0 space-y-1">
                                <div className="text-sm font-medium">{t('circuitBreaker.probe.maxFailures.label')}</div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('circuitBreaker.probe.maxFailures.hint')}</p>
                            </div>
                            <Input
                                type="number"
                                min={0}
                                value={probeMaxFailures}
                                onChange={(e) => setProbeMaxFailures(e.target.value)}
                                onBlur={() => saveProbeNumber(SettingKey.CircuitProbeMaxConsecutiveFailures, probeMaxFailures, initialProbeMaxFailures, setProbeMaxFailures)}
                                placeholder={t('circuitBreaker.probe.maxFailures.placeholder')}
                                aria-label={t('circuitBreaker.probe.maxFailures.label')}
                                className="w-28 shrink-0 rounded-xl"
                            />
                        </div>

                        {/* 向量（embeddings）也探：不打开的话嵌入模型永远不会被探 */}
                        <div className="flex items-start justify-between gap-4">
                            <div className="min-w-0 space-y-1">
                                <div className="text-sm font-medium">{t('circuitBreaker.probe.embeddings.label')}</div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('circuitBreaker.probe.embeddings.hint')}</p>
                            </div>
                            <Switch
                                checked={probeEmbeddings === 'true'}
                                onCheckedChange={(checked) => handleProbeSwitch(SettingKey.CircuitProbeEmbeddingsEnabled, checked, initialProbeEmbeddings, setProbeEmbeddings)}
                                aria-label={t('circuitBreaker.probe.embeddings.label')}
                                className="shrink-0"
                            />
                        </div>

                        <div className="space-y-1 rounded-xl bg-background/60 p-3 text-xs leading-6 text-muted-foreground">
                            <p><span className="font-medium text-card-foreground">{t('circuitBreaker.probe.note.whenTitle')}</span>{t('circuitBreaker.probe.note.when')}</p>
                            <p><span className="font-medium text-card-foreground">{t('circuitBreaker.probe.note.whatTitle')}</span>{t('circuitBreaker.probe.note.what')}</p>
                            <p><span className="font-medium text-card-foreground">{t('circuitBreaker.probe.note.skipTitle')}</span>{t('circuitBreaker.probe.note.skip')}</p>
                        </div>

                        {/* 今日已用：分母优先用后端给的 cap，拿不到才回落到设置里的上限。 */}
                        <div className="space-y-2">
                            <div className="flex items-center justify-between gap-2">
                                <span className="text-sm font-medium">{t('circuitBreaker.probe.usedToday')}</span>
                                <span className="font-mono text-sm tabular-nums" data-probe-usage>{probeUsageNumbers}</span>
                            </div>
                            <div className="h-2 overflow-hidden rounded-full bg-muted" role="presentation">
                                <div className="h-full rounded-full bg-primary" style={{ width: `${probeUsageWidth}%` }} />
                            </div>
                            <div className="flex flex-wrap items-center gap-1.5">
                                <span className="rounded-full bg-emerald-500/15 px-2 py-0.5 text-xs text-emerald-700 dark:text-emerald-300">
                                    {t('circuitBreaker.probe.recovered', { count: probeTotals.recovered })}
                                </span>
                                <span className="rounded-full bg-red-500/15 px-2 py-0.5 text-xs text-red-700 dark:text-red-300">
                                    {t('circuitBreaker.probe.failed', { count: probeTotals.failed })}
                                </span>
                                <span className="rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">
                                    {t('circuitBreaker.probe.skipped', { count: probeTotals.skipped })}
                                </span>
                            </div>
                        </div>
                    </div>
                )}
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
