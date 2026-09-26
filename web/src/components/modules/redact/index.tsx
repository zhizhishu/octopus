'use client';

/**
 * 凭据脱敏管理页 (CosyRedactGateway 接入)。
 *
 * 三块:
 * 1. 全局设置卡 — 总闸 (redact_enabled)、默认检测器 (redact_default_flags)、
 *    脱敏提示注入 (redact_notice_enabled)。
 * 2. 渠道卡 — 每渠道开关脱敏 (redact_enabled) + 检测器定制 (redact_flags,
 *    留空走全局默认)。指纹安全: 脱敏只改应用层 body 文本, TLS/header 指纹零改动,
 *    claude/codex CLI shape 与普通渠道 UA 全部保持原样。
 * 3. 测试卡 — 合成样本跑一遍 脱敏→还原 往返, 上线前先看效果。
 */

import { useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { ShieldCheck, FlaskConical, Eye, EyeOff } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { useChannelList, useUpdateChannel } from '@/api/endpoints/channel';
import { apiClient } from '@/api/client';
import { cn } from '@/lib/utils';

/** 检测器字母 → 名称 (与后端 model.RedactFlagLetters 一致) */
const DETECTORS: Array<{ letter: string; name: string; hint: string }> = [
    { letter: 'H', name: '高熵密钥', hint: 'High-entropy secrets (API keys, tokens)' },
    { letter: 'P', name: '手机号', hint: 'Phone numbers' },
    { letter: 'S', name: 'sk- 密钥', hint: 'OpenAI-style sk- keys' },
    { letter: 'I', name: '身份证', hint: 'ID card numbers' },
    { letter: 'B', name: '银行卡', hint: 'Bank card numbers' },
    { letter: 'E', name: '邮箱', hint: 'Email addresses' },
    { letter: 'G', name: 'Git 泄漏', hint: 'Gitleaks patterns' },
];

function normalizeFlags(raw: string): string {
    const seen = new Set<string>();
    for (const ch of raw.toUpperCase()) {
        if (DETECTORS.some((d) => d.letter === ch)) seen.add(ch);
    }
    return DETECTORS.filter((d) => seen.has(d.letter)).map((d) => d.letter).join('');
}

/** 全局设置卡: 总闸 + 默认检测器 + 提示注入 */
function RedactGlobalCard() {
    const t = useTranslations('redact');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const valueOf = (key: string, fallback: string) =>
        settings?.find((s) => s.key === key)?.value ?? fallback;

    const masterEnabled = valueOf(SettingKey.RedactEnabled, 'false') === 'true';
    const noticeEnabled = valueOf(SettingKey.RedactNoticeEnabled, 'true') === 'true';
    const defaultFlags = valueOf(SettingKey.RedactDefaultFlags, 'HPSIBEG');

    const update = (key: string, value: string) => setSetting.mutate({ key, value });

    return (
        <div className="rounded-3xl border border-border bg-card p-6">
            <div className="flex items-center gap-3">
                <ShieldCheck className="h-5 w-5 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                    <p className="text-sm font-semibold text-foreground">{t('global.title')}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">{t('global.subtitle')}</p>
                </div>
            </div>

            <div className="mt-4 rounded-xl border border-border bg-muted/40 p-3.5">
                <p className="text-xs font-semibold text-foreground">{t('global.scopeTitle')}</p>
                <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
                    <li>{t('global.scopeProtected')}</li>
                    <li>{t('global.scopeUnprotected')}</li>
                    <li>{t('global.scopeBypass')}</li>
                </ul>
            </div>

            <div className="mt-5 space-y-4">
                <div className="flex items-center justify-between gap-4 rounded-xl border border-border bg-background px-4 py-3">
                    <div className="min-w-0">
                        <p className="text-sm font-medium text-foreground">{t('global.master')}</p>
                        <p className="mt-0.5 text-xs text-muted-foreground">{t('global.masterHint')}</p>
                    </div>
                    <Switch
                        checked={masterEnabled}
                        onCheckedChange={(v) => update(SettingKey.RedactEnabled, String(v))}
                        disabled={setSetting.isPending}
                    />
                </div>

                <div className="rounded-xl border border-border bg-background px-4 py-3">
                    <div className="flex items-center justify-between gap-4">
                        <div className="min-w-0">
                            <p className="text-sm font-medium text-foreground">{t('global.notice')}</p>
                            <p className="mt-0.5 text-xs text-muted-foreground">{t('global.noticeHint')}</p>
                        </div>
                        <Switch
                            checked={noticeEnabled}
                            onCheckedChange={(v) => update(SettingKey.RedactNoticeEnabled, String(v))}
                            disabled={setSetting.isPending}
                        />
                    </div>
                </div>

                <div className="rounded-xl border border-border bg-background px-4 py-3">
                    <p className="text-sm font-medium text-foreground">{t('global.defaultFlags')}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">{t('global.defaultFlagsHint')}</p>
                    <div className="mt-3 flex flex-wrap gap-1.5">
                        {DETECTORS.map((d) => {
                            const active = defaultFlags.includes(d.letter);
                            return (
                                <button
                                    key={d.letter}
                                    type="button"
                                    title={`${d.name} — ${d.hint}`}
                                    onClick={() => {
                                        const next = active
                                            ? defaultFlags.split('').filter((c) => c !== d.letter).join('')
                                            : normalizeFlags(defaultFlags + d.letter);
                                        update(SettingKey.RedactDefaultFlags, next);
                                    }}
                                    className={cn(
                                        'h-7 rounded-md border px-2.5 text-xs font-medium transition-colors',
                                        active
                                            ? 'border-primary bg-primary text-primary-foreground'
                                            : 'border-border text-muted-foreground hover:bg-muted/70'
                                    )}
                                >
                                    {d.letter} · {d.name}
                                </button>
                            );
                        })}
                    </div>
                </div>
            </div>
        </div>
    );
}

/** 渠道卡: 每渠道开关 + 检测器定制 */
function RedactChannelCard() {
    const t = useTranslations('redact');
    const { data: channels } = useChannelList();
    const { data: settings } = useSettingList();
    const updateChannel = useUpdateChannel();
    const [expanded, setExpanded] = useState<number | null>(null);

    const valueOf = (key: string, fallback: string) =>
        settings?.find((s) => s.key === key)?.value ?? fallback;

    const masterEnabled = valueOf(SettingKey.RedactEnabled, 'false') === 'true';
    const globalDefaultFlags = normalizeFlags(valueOf(SettingKey.RedactDefaultFlags, 'HPSIBEG'));

    const sorted = useMemo(() => {
        if (!channels) return [];
        return [...channels].sort((a, b) => {
            const diff = Number(b.raw.redact_enabled) - Number(a.raw.redact_enabled);
            if (diff !== 0) return diff;
            return a.raw.id - b.raw.id;
        });
    }, [channels]);

    return (
        <div className="rounded-3xl border border-border bg-card p-6">
            <div className="flex items-center gap-3">
                <EyeOff className="h-5 w-5 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                    <p className="text-sm font-semibold text-foreground">{t('channel.title')}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">{t('channel.subtitle')}</p>
                </div>
            </div>

            <div className="mt-5 space-y-2">
                {sorted.length === 0 && (
                    <p className="py-6 text-center text-sm text-muted-foreground">{t('channel.empty')}</p>
                )}
                {sorted.map(({ raw }) => {
                    const channelFlags = raw.redact_flags ?? '';
                    // 空串 = 继承全局默认 (后端语义), 非空 = 该渠道自定义
                    const inherits = channelFlags === '';
                    // 实际生效 = 渠道自定义值, 否则全局默认值
                    const effectiveFlags = inherits ? globalDefaultFlags : channelFlags;
                    // 三态徽章 (+ 总闸态): 未启用 / 总闸关闭 / 继承默认 / 自定义
                    const badge = !raw.redact_enabled
                        ? { text: t('channel.statusDisabled'), cls: 'bg-muted text-muted-foreground' }
                        : !masterEnabled
                          ? {
                                text: t('channel.statusMasterDisabled'),
                                cls: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
                            }
                          : inherits
                            ? {
                                  text: t('channel.statusInherited', { flags: effectiveFlags || '—' }),
                                  cls: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
                              }
                            : {
                                  text: t('channel.statusCustom'),
                                  cls: 'bg-sky-500/10 text-sky-600 dark:text-sky-400',
                              };
                    const dotCls = !raw.redact_enabled
                        ? 'bg-muted-foreground/30'
                        : !masterEnabled
                          ? 'bg-amber-500'
                          : 'bg-emerald-500';
                    return (
                        <div key={raw.id} className="rounded-xl border border-border bg-background">
                            <div className="flex items-center justify-between gap-3 px-4 py-3">
                                <button
                                    type="button"
                                    className="flex min-w-0 items-center gap-2 text-left"
                                    onClick={() => setExpanded(expanded === raw.id ? null : raw.id)}
                                >
                                    <span className={cn('h-2 w-2 shrink-0 rounded-full', dotCls)} />
                                    <span className="min-w-0 truncate text-sm font-medium text-foreground">
                                        {raw.name}
                                    </span>
                                    <span className="shrink-0 text-xs text-muted-foreground">#{raw.id}</span>
                                </button>
                                <div className="flex shrink-0 items-center gap-2">
                                    <span
                                        className={cn(
                                            'rounded-full px-2 py-0.5 text-[10px] font-medium',
                                            badge.cls
                                        )}
                                    >
                                        {badge.text}
                                    </span>
                                    <Switch
                                        checked={raw.redact_enabled}
                                        onCheckedChange={(v) =>
                                            updateChannel.mutate({ id: raw.id, redact_enabled: v })
                                        }
                                        disabled={updateChannel.isPending}
                                    />
                                </div>
                            </div>
                            {expanded === raw.id && (
                                <div className="border-t border-border px-4 py-3">
                                    <p className="text-xs text-muted-foreground">{t('channel.flagsHint')}</p>
                                    <div className="mt-2 flex flex-wrap gap-1.5">
                                        {DETECTORS.map((d) => {
                                            const active = effectiveFlags.includes(d.letter);
                                            const inheritedActive = inherits && active;
                                            return (
                                                <button
                                                    key={d.letter}
                                                    type="button"
                                                    title={`${d.name} — ${d.hint}`}
                                                    onClick={() => {
                                                        // 基于实际生效组合计算下一组合并写回渠道值;
                                                        // 取消全部字母得到空串 = 恢复继承全局默认 (后端语义)。
                                                        const next = active
                                                            ? normalizeFlags(
                                                                  effectiveFlags
                                                                      .split('')
                                                                      .filter((c) => c !== d.letter)
                                                                      .join('')
                                                              )
                                                            : normalizeFlags(effectiveFlags + d.letter);
                                                        updateChannel.mutate({ id: raw.id, redact_flags: next });
                                                    }}
                                                    className={cn(
                                                        'h-7 rounded-md border px-2.5 text-xs font-medium transition-colors',
                                                        active
                                                            ? 'border-primary bg-primary text-primary-foreground'
                                                            : 'border-border text-muted-foreground hover:bg-muted/70',
                                                        inheritedActive && 'border-dashed'
                                                    )}
                                                >
                                                    {d.letter}
                                                    {inheritedActive && <span className="ml-1 opacity-60">·</span>}
                                                </button>
                                            );
                                        })}
                                    </div>
                                    <p className="mt-2 text-xs text-muted-foreground">
                                        {t('channel.effective', { flags: effectiveFlags || '—' })}
                                    </p>
                                    {inherits ? (
                                        <p className="mt-1 text-xs text-muted-foreground">
                                            {t('channel.inheritedNotice', { flags: effectiveFlags || '—' })}
                                        </p>
                                    ) : (
                                        <div className="mt-1 flex flex-wrap items-center gap-2">
                                            <p className="text-xs text-muted-foreground">
                                                {t('channel.customNotice')}
                                            </p>
                                            <button
                                                type="button"
                                                onClick={() =>
                                                    updateChannel.mutate({ id: raw.id, redact_flags: '' })
                                                }
                                                disabled={updateChannel.isPending}
                                                className="h-7 rounded-md border border-dashed border-border px-2.5 text-xs text-muted-foreground hover:bg-muted/70"
                                            >
                                                {t('channel.restoreInherit')}
                                            </button>
                                        </div>
                                    )}
                                </div>
                            )}
                        </div>
                    );
                })}
            </div>
        </div>
    );
}

interface RedactTestResult {
    redacted: string;
    restored: string;
    count: number;
}

/** 测试卡: 合成样本往返验证 */
function RedactTestCard() {
    const t = useTranslations('redact');
    const [text, setText] = useState('我的邮箱是 a@example.com，手机 13812345678，密钥 sk-proj-abc123def456ghi789');
    const [flags, setFlags] = useState('');
    const [result, setResult] = useState<RedactTestResult | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [testing, setTesting] = useState(false);

    const runTest = async () => {
        setTesting(true);
        setError(null);
        setResult(null);
        try {
            const res = await apiClient.post<RedactTestResult>('/api/v1/redact/test', {
                text,
                flags,
            });
            setResult(res);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setTesting(false);
        }
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6">
            <div className="flex items-center gap-3">
                <FlaskConical className="h-5 w-5 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                    <p className="text-sm font-semibold text-foreground">{t('test.title')}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">{t('test.subtitle')}</p>
                </div>
            </div>

            <div className="mt-5 space-y-3">
                <textarea
                    value={text}
                    onChange={(e) => setText(e.target.value)}
                    rows={3}
                    className="w-full resize-none rounded-xl border border-input bg-background px-3 py-2 text-sm text-foreground placeholder:text-muted-foreground"
                    placeholder={t('test.placeholder')}
                />
                <div className="flex items-center gap-2">
                    <input
                        value={flags}
                        onChange={(e) => setFlags(normalizeFlags(e.target.value))}
                        className="h-9 w-32 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                        placeholder={t('test.flagsPlaceholder')}
                    />
                    <span className="text-xs text-muted-foreground">{t('test.flagsHint')}</span>
                </div>
                <Button onClick={runTest} disabled={testing || text.trim() === ''}>
                    {testing ? t('test.running') : t('test.run')}
                </Button>

                {error && (
                    <p className="rounded-xl border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
                        {error}
                    </p>
                )}
                {result && (
                    <div className="space-y-2">
                        <div className="rounded-xl border border-border bg-background px-3 py-2">
                            <p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                                <EyeOff className="h-3.5 w-3.5" />
                                {t('test.upstreamSees')} ({t('test.count', { count: result.count })})
                            </p>
                            <p className="mt-1 break-all font-mono text-xs text-foreground">{result.redacted}</p>
                        </div>
                        <div className="rounded-xl border border-border bg-background px-3 py-2">
                            <p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                                <Eye className="h-3.5 w-3.5" />
                                {t('test.clientSees')}
                            </p>
                            <p className="mt-1 break-all font-mono text-xs text-foreground">{result.restored}</p>
                        </div>
                    </div>
                )}
            </div>
        </div>
    );
}

export function Redact() {
    return (
        <div className="h-full min-h-0 overflow-y-auto overscroll-contain rounded-t-3xl">
            <div className="space-y-4 pb-24 md:pb-4">
                <PageWrapper className="grid grid-cols-1 items-start gap-4 md:grid-cols-2">
                    <RedactGlobalCard />
                    <RedactTestCard />
                </PageWrapper>
                <div className="px-4 md:px-0">
                    <RedactChannelCard />
                </div>
            </div>
        </div>
    );
}
