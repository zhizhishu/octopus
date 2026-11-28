'use client';

// 结构化错误规则编辑器（feat/error-rules-ui）。
// 字段语义照抄 gpt-load web/src/shared/error-rules.ts 与 ErrorRulesEditor.vue 的
// 条件/动作模型（状态码+关键词 → retry+effect+冷却秒数），校验口径与后端
// internal/model/circuit_rules.go ParseCircuitErrorRules 一致（≤100 条、状态码
// 100..599、关键词小写非空、冷却 1..86400、legacy 持 Scope 必填）——前端只做
// 输入辅助与早失败提示，最终判官仍是后端 Validate（保存失败原文回显）。

import { useEffect, useState } from 'react';
import { useTranslations } from 'next-intl';
import { Plus, Trash2 } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

export type ErrorRule = {
    name: string;
    status_codes?: number[];
    keywords?: string[];
    scope?: string;
    cooldown_seconds?: number;
    retry?: string;
    effect?: string;
    disabled?: boolean;
};

const RETRY_VALUES = ['', 'same_key', 'next_key', 'next_channel'] as const;
const EFFECT_VALUES = ['', 'cooldown_key', 'cooldown_model', 'cooldown_channel', 'disable_key', 'disable_channel', 'record_failure'] as const;
const SCOPE_VALUES = ['', 'model', 'key', 'channel'] as const;

export function parseRulesJson(raw: string): ErrorRule[] {
    try {
        const parsed = JSON.parse(raw || '[]');
        return Array.isArray(parsed) ? parsed : [];
    } catch {
        return [];
    }
}

// 前端校验与后端 ParseCircuitErrorRules 同口径，返回 null=通过。
export function validateRule(rule: ErrorRule): string | null {
    const codes = rule.status_codes ?? [];
    const kws = (rule.keywords ?? []).map((k) => k.trim()).filter(Boolean);
    if (codes.length === 0 && kws.length === 0) return 'rule needs status_codes or keywords';
    if (codes.length > 20) return 'at most 20 status codes';
    for (const c of codes) {
        if (!Number.isInteger(c) || c < 100 || c > 599) return 'status code must be 100..599';
    }
    if (kws.length > 20) return 'at most 20 keywords';
    const legacy = !rule.retry && !rule.effect;
    if (legacy && !rule.scope) return 'legacy hold rules need scope';
    const needsCooldown = legacy || ['cooldown_key', 'cooldown_credential', 'cooldown_model', 'cooldown_channel', 'skip_group'].includes(rule.effect ?? '');
    const cd = rule.cooldown_seconds ?? 0;
    if (needsCooldown && (cd < 1 || cd > 86400)) return 'cooldown_seconds must be 1..86400';
    return null;
}

export function rulesToJson(rules: ErrorRule[]): string {
    return JSON.stringify(rules.map((r) => {
        const out: ErrorRule = { name: (r.name || '').trim() };
        if (!out.name) out.name = 'rule';
        if (r.status_codes?.length) out.status_codes = r.status_codes;
        const kws = (r.keywords ?? []).map((k) => k.trim().toLowerCase()).filter(Boolean);
        if (kws.length) out.keywords = kws;
        if (r.scope) out.scope = r.scope;
        if (r.cooldown_seconds) out.cooldown_seconds = r.cooldown_seconds;
        if (r.retry) out.retry = r.retry;
        if (r.effect) out.effect = r.effect;
        if (r.disabled) out.disabled = true;
        return out;
    }));
}

function csvToInts(raw: string): number[] {
    return raw.split(',').map((s) => Number.parseInt(s.trim(), 10)).filter((n) => Number.isFinite(n));
}
function csvToStrings(raw: string): string[] {
    return raw.split(',').map((s) => s.trim()).filter(Boolean);
}

export function ErrorRulesEditor({ value, onChange, disabled }: {
    value: string; // JSON 原文（与设置值同形）
    onChange: (json: string) => void;
    disabled?: boolean;
}) {
    const t = useTranslations('setting');
    const [rules, setRules] = useState<ErrorRule[]>(() => parseRulesJson(value));

    // 外部值变更（初次加载/保存回滚）时重同步；编辑期间不打扰。
    useEffect(() => {
        setRules((current) => {
            return rulesToJson(parseRulesJson(value)) !== rulesToJson(current) ? parseRulesJson(value) : current;
        });
    }, [value]);

    const update = (next: ErrorRule[]) => {
        setRules(next);
        onChange(rulesToJson(next));
    };
    const patch = (index: number, part: Partial<ErrorRule>) => {
        update(rules.map((r, i) => (i === index ? { ...r, ...part } : r)));
    };

    const selectClass = 'rounded-xl border border-border bg-background px-2 py-1 text-xs';

    return (
        <div className="space-y-2" data-error-rules-editor>
            {rules.length >= 100 && (
                <p className="text-xs text-red-600">{t('circuitBreaker.rules.tooMany')}</p>
            )}
            {rules.map((rule, index) => {
                const error = validateRule(rule);
                return (
                    <div key={index} className="space-y-1 rounded-xl border border-border bg-background/60 p-2" data-rule-row>
                        <div className="flex flex-wrap items-center gap-1.5">
                            <Input
                                value={rule.name}
                                onChange={(e) => patch(index, { name: e.target.value })}
                                placeholder={t('circuitBreaker.rules.namePh')}
                                aria-label={t('circuitBreaker.rules.namePh')}
                                className="h-8 w-28 rounded-xl text-xs"
                                disabled={disabled}
                            />
                            <Input
                                defaultValue={(rule.status_codes ?? []).join(',')}
                                onBlur={(e) => patch(index, { status_codes: csvToInts(e.target.value) })}
                                placeholder={t('circuitBreaker.rules.codesPh')}
                                aria-label={t('circuitBreaker.rules.codesPh')}
                                className="h-8 w-24 rounded-xl font-mono text-xs"
                                disabled={disabled}
                            />
                            <Input
                                defaultValue={(rule.keywords ?? []).join(',')}
                                onBlur={(e) => patch(index, { keywords: csvToStrings(e.target.value) })}
                                placeholder={t('circuitBreaker.rules.keywordsPh')}
                                aria-label={t('circuitBreaker.rules.keywordsPh')}
                                className="h-8 min-w-28 flex-1 rounded-xl font-mono text-xs"
                                disabled={disabled}
                            />
                            <Button
                                size="sm" variant="ghost" className="h-8 w-8 rounded-xl p-0"
                                onClick={() => update(rules.filter((_, i) => i !== index))}
                                disabled={disabled}
                                aria-label={t('circuitBreaker.rules.remove')}
                            >
                                <Trash2 className="size-3.5" />
                            </Button>
                        </div>
                        <div className="flex flex-wrap items-center gap-1.5">
                            <select
                                value={rule.retry ?? ''}
                                onChange={(e) => patch(index, { retry: e.target.value || undefined })}
                                className={selectClass}
                                aria-label={t('circuitBreaker.rules.retryLabel')}
                                disabled={disabled}
                            >
                                {RETRY_VALUES.map((v) => <option key={v || 'empty'} value={v}>{v === '' ? t('circuitBreaker.rules.retryDefault') : v}</option>)}
                            </select>
                            <select
                                value={rule.effect ?? ''}
                                onChange={(e) => patch(index, { effect: e.target.value || undefined })}
                                className={selectClass}
                                aria-label={t('circuitBreaker.rules.effectLabel')}
                                disabled={disabled}
                            >
                                {EFFECT_VALUES.map((v) => <option key={v || 'empty'} value={v}>{v === '' ? t('circuitBreaker.rules.effectDefault') : v}</option>)}
                            </select>
                            <select
                                value={rule.scope ?? ''}
                                onChange={(e) => patch(index, { scope: e.target.value || undefined })}
                                className={selectClass}
                                aria-label={t('circuitBreaker.rules.scopeLabel')}
                                disabled={disabled}
                            >
                                {SCOPE_VALUES.map((v) => <option key={v || 'empty'} value={v}>{v === '' ? t('circuitBreaker.rules.scopeNone') : v}</option>)}
                            </select>
                            <Input
                                type="number" min={0}
                                defaultValue={rule.cooldown_seconds ?? ''}
                                onBlur={(e) => patch(index, { cooldown_seconds: Number.parseInt(e.target.value, 10) || 0 })}
                                placeholder={t('circuitBreaker.rules.cooldownPh')}
                                aria-label={t('circuitBreaker.rules.cooldownPh')}
                                className="h-8 w-24 rounded-xl text-xs"
                                disabled={disabled}
                            />
                            <label className="flex items-center gap-1 text-xs text-muted-foreground">
                                <input
                                    type="checkbox"
                                    checked={rule.disabled ?? false}
                                    onChange={(e) => patch(index, { disabled: e.target.checked || undefined })}
                                    disabled={disabled}
                                />
                                {t('circuitBreaker.rules.disabledLabel')}
                            </label>
                        </div>
                        {error && <p className="text-xs text-red-600" data-rule-error>{error}</p>}
                    </div>
                );
            })}
            <Button
                size="sm" variant="outline" className="rounded-xl"
                onClick={() => update([...rules, { name: '', keywords: [], status_codes: [], effect: 'cooldown_key', cooldown_seconds: 600 }])}
                disabled={disabled || rules.length >= 100}
                data-add-rule
            >
                <Plus className="mr-1 size-3.5" />
                {t('circuitBreaker.rules.add')}
            </Button>
        </div>
    );
}
