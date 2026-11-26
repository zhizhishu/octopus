'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { Loader2 } from 'lucide-react';
import type { AccessPlan } from '@/api/endpoints/access-plan';
import type { Channel } from '@/api/endpoints/channel';
import { normalizePromptRulePreview, usePreviewPromptRules } from '@/api/endpoints/prompt-rule';
import type { ApiError } from '@/api/types';
import { cn } from '@/lib/utils';
import {
    PROMPT_RULE_CLIENT_EMPTY_HINT,
    promptRuleClientSegmentEmpty,
    promptRulePreviewView,
    promptRuleReasonText,
    promptRuleScopeTags,
    type PromptRulePreviewView,
} from './rule-model';

// 合并预览页：选 方案 / 模型 / 最后走的渠道 / 谁在调，看这次会加哪几条、模型最后看到什么。
// 命中和合并文本都由后端 /preview 在本地算（不发上游请求），前端只展示，不自己再拼一遍。

const CLIENT_SYSTEM_PLACEHOLDER = '贴一段客户端自带的 system / developer 指令（可选）';

/** 参数变了先等一小会儿再问后端，免得每敲一个字发一次。 */
const PREVIEW_DEBOUNCE_MS = 250;

/** 模型下拉的候选：各方案路由的请求模型 + 渠道勾选/发现的模型，去重按小写。 */
export function collectModelSuggestions(plans: AccessPlan[], channels: Channel[]): string[] {
    const seen = new Set<string>();
    const list: string[] = [];
    const push = (name: string) => {
        const key = name.trim().toLowerCase();
        if (!key || seen.has(key)) return;
        seen.add(key);
        list.push(name.trim());
    };
    for (const plan of plans) {
        for (const target of plan.route_targets ?? []) {
            const model = (target.request_model ?? '').trim();
            if (model) push(model);
        }
    }
    for (const channel of channels) {
        for (const model of [...(channel.selected_models ?? []), ...(channel.discovered_models ?? [])]) {
            const name = (model ?? '').trim();
            if (name) push(name);
        }
    }
    return list.sort((a, b) => a.localeCompare(b));
}

export function MergePreview({
    plans,
    channels,
    loading,
    rulesVersion,
}: {
    plans: AccessPlan[];
    channels: Channel[];
    loading: boolean;
    /** 规则有任何改动（保存/开关/调顺序）都会变：变了就重新问一次 /preview。 */
    rulesVersion: string;
}) {
    const [planId, setPlanId] = useState(0);
    const [model, setModel] = useState('');
    const [channelId, setChannelId] = useState<number | null>(null);
    const [isAdmin, setIsAdmin] = useState(false);
    const [clientSystem, setClientSystem] = useState('');
    const [view, setView] = useState<PromptRulePreviewView | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [pending, setPending] = useState(false);

    const planNames = useMemo(() => new Map(plans.map((plan) => [plan.id, plan.display_name || plan.slug])), [plans]);
    const channelNames = useMemo(() => new Map(channels.map((channel) => [channel.id, channel.name])), [channels]);
    const modelOptions = useMemo(() => collectModelSuggestions(plans, channels), [plans, channels]);
    const effectiveChannel = channelId ?? channels[0]?.id ?? 0;
    const scope = { plan_id: planId, request_model: model, channel_id: effectiveChannel, is_admin: isAdmin };

    const previewMutation = usePreviewPromptRules();
    const previewRef = useRef(previewMutation);
    useEffect(() => {
        previewRef.current = previewMutation;
    });

    // 每次换参数或规则改动就换一个请求键：旧请求的结果回来时键已经变了，直接丢掉，
    // 不能拿上一次参数的结果冒充这一次的预览。
    const requestKey = `${rulesVersion}\u0000${planId}\u0000${model}\u0000${effectiveChannel}\u0000${isAdmin}\u0000${clientSystem}`;
    const activeKey = useRef('');

    useEffect(() => {
        if (loading) return;
        const key = requestKey;
        activeKey.current = key;
        setPending(true);
        const timer = setTimeout(() => {
            previewRef.current.mutateAsync({
                plan_id: planId,
                request_model: model,
                channel_id: effectiveChannel,
                is_admin: isAdmin,
                client_system: clientSystem,
            })
                .then((response) => {
                    if (activeKey.current !== key) return;
                    setView(promptRulePreviewView(normalizePromptRulePreview(response)));
                    setError(null);
                    setPending(false);
                })
                .catch((cause: unknown) => {
                    if (activeKey.current !== key) return;
                    // 出错就把上一次的结果清掉：宁可空着说失败，也不拿旧结果当当前预览。
                    setView(null);
                    setError((cause as ApiError | undefined)?.message || '预览没算出来，稍后再试');
                    setPending(false);
                });
        }, PREVIEW_DEBOUNCE_MS);
        return () => clearTimeout(timer);
    }, [loading, requestKey, planId, model, effectiveChannel, isAdmin, clientSystem]);

    const selectClass = 'min-h-11 max-w-full rounded-xl border border-input bg-background px-3 text-sm text-foreground';
    const stale = pending && view !== null;

    return (
        <div className="grid content-start gap-3">
            <section aria-label="预览条件" className="rounded-3xl border border-border bg-card p-4 sm:p-5">
                <div className="flex flex-wrap items-end gap-3">
                    <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                        方案
                        <select value={planId} onChange={(event) => setPlanId(Number(event.target.value))} className={selectClass} aria-label="预览用的方案">
                            <option value={0}>没选方案的 Key</option>
                            {plans.map((plan) => (
                                <option key={plan.id} value={plan.id}>{plan.display_name || plan.slug}</option>
                            ))}
                        </select>
                    </label>
                    <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                        模型
                        <input
                            value={model}
                            onChange={(event) => setModel(event.target.value)}
                            list="prompt-preview-models"
                            placeholder="请求模型名；留空 = 不带模型"
                            aria-label="预览用的模型"
                            className={selectClass}
                        />
                        <datalist id="prompt-preview-models">
                            {modelOptions.slice(0, 100).map((name) => <option key={name} value={name} />)}
                        </datalist>
                    </label>
                    <label className="grid min-w-0 gap-1 text-xs text-muted-foreground">
                        最后走的渠道
                        <select value={effectiveChannel} onChange={(event) => setChannelId(Number(event.target.value))} className={selectClass} aria-label="预览用的渠道">
                            {channels.length === 0 ? <option value={0}>（还没有渠道）</option> : null}
                            {channels.map((channel) => (
                                <option key={channel.id} value={channel.id}>#{channel.id} {channel.name}</option>
                            ))}
                        </select>
                    </label>
                    <div className="grid gap-1 text-xs text-muted-foreground">
                        谁在调
                        <div role="radiogroup" aria-label="谁在调用" className="inline-flex min-h-11 items-center rounded-xl border border-input bg-muted/40 p-0.5 text-sm">
                            {([
                                { value: false, label: '普通用户' },
                                { value: true, label: '管理员' },
                            ]).map((option) => (
                                <button
                                    key={String(option.value)}
                                    type="button"
                                    role="radio"
                                    aria-checked={isAdmin === option.value}
                                    onClick={() => setIsAdmin(option.value)}
                                    className={cn(
                                        'min-h-11 shrink-0 rounded-lg px-3 transition-colors',
                                        isAdmin === option.value ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                                    )}
                                >
                                    {option.label}
                                </button>
                            ))}
                        </div>
                    </div>
                    <span className="text-xs text-muted-foreground sm:ml-auto">在这里查看合并效果，不会调用模型</span>
                </div>
                <label className="mt-3 grid gap-1 text-xs text-muted-foreground">
                    客户端示例（可选）
                    <textarea
                        value={clientSystem}
                        onChange={(event) => setClientSystem(event.target.value)}
                        placeholder={CLIENT_SYSTEM_PLACEHOLDER}
                        aria-label="客户端自带的系统提示词示例"
                        className="min-h-16 w-full rounded-xl border border-input bg-background px-3 py-2 text-sm leading-6 text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    />
                </label>
            </section>

            {loading ? (
                <section className="flex justify-center rounded-3xl border border-border bg-card py-10 text-muted-foreground">
                    <Loader2 className="size-5 animate-spin" />
                </section>
            ) : error ? (
                <section role="alert" className="rounded-3xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive sm:p-5">
                    预览没算出来：{error}
                </section>
            ) : view === null ? (
                <section className="flex items-center justify-center gap-2 rounded-3xl border border-border bg-card py-10 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    正在按这些条件算…
                </section>
            ) : (
                <div className={cn('grid items-start gap-3 transition-opacity lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]', stale && 'opacity-60')}>
                    <section aria-label="命中结论" className="min-w-0 rounded-3xl border border-border bg-card p-4 sm:p-5">
                        <div className="flex items-center justify-between gap-2">
                            <h3 className="text-sm font-semibold">这次会加哪几条</h3>
                            {stale ? <span className="text-xs text-muted-foreground">正在重算…</span> : null}
                        </div>
                        <p className="mt-1 text-xs text-muted-foreground">{view.matchedCount} 条会加上。左栏从上到下是加进去的先后（先全局、后更具体的），越具体的规则在最终提示词里越靠外；灰的是这次不命中的。</p>
                        <ul className="mt-3 grid content-start gap-2">
                            {view.matches.map(({ rule, matched, reason }) => {
                                const tags = promptRuleScopeTags(rule, planNames, channelNames);
                                const reasonText = promptRuleReasonText(reason, scope);
                                return (
                                    <li
                                        key={rule.id}
                                        className={cn(
                                            'rounded-xl border-l-4 px-3 py-2',
                                            matched ? 'border-l-primary bg-primary/5' : 'border-l-border bg-muted/30 opacity-70',
                                        )}
                                    >
                                        <div className="flex min-w-0 flex-wrap items-center gap-x-2">
                                            <span className="min-w-0 truncate text-sm font-medium">{matched ? '✓' : '—'} {rule.name}</span>
                                            <span className="shrink-0 text-xs text-muted-foreground">{rule.position === 'after' ? '放在后面' : '放在前面'}</span>
                                        </div>
                                        <p className="mt-0.5 break-words text-xs leading-5 text-muted-foreground">
                                            {matched ? tags.map((tag) => tag.label).join(' · ') : reasonText}
                                        </p>
                                    </li>
                                );
                            })}
                            {view.matches.length === 0 ? <li className="rounded-xl border border-dashed border-border px-3 py-6 text-center text-sm text-muted-foreground">还没有规则</li> : null}
                        </ul>
                    </section>

                    <section aria-label="最终系统提示词" className="min-w-0 rounded-3xl border border-border bg-card p-4 sm:p-5">
                        <h3 className="text-sm font-semibold">模型最后看到的系统提示词</h3>
                        <div className="mt-3 grid content-start gap-2">
                            {view.segments.map((seg, index) => {
                                const isClient = seg.position === 'client';
                                const emptyClient = promptRuleClientSegmentEmpty(seg);
                                return (
                                    <div
                                        key={`${seg.rule_id}-${index}`}
                                        className={cn(
                                            'rounded-xl border px-3 py-2',
                                            isClient ? 'border-dashed border-primary/40 bg-primary/5' : 'border-border bg-background',
                                        )}
                                    >
                                        <p className={cn('text-xs font-semibold', isClient ? 'text-primary' : 'text-muted-foreground')}>
                                            {isClient ? '客户端（用户自带的系统提示词，原样保留）' : `${seg.name}（${seg.position === 'after' ? '后面' : '前面'}）`}
                                        </p>
                                        {emptyClient ? (
                                            <p className="mt-1 text-sm text-muted-foreground">{PROMPT_RULE_CLIENT_EMPTY_HINT}</p>
                                        ) : (
                                            <p className="mt-1 whitespace-pre-wrap break-words text-sm leading-6">{seg.text}</p>
                                        )}
                                    </div>
                                );
                            })}
                        </div>
                        <p className="mt-3 text-xs leading-5 text-muted-foreground">
                            Claude Code 自带的计费头和身份块在真实请求里仍排第一，规则插在它后面。这里是纯文字示意，各协议的真实分块以实际转发为准。
                        </p>
                    </section>
                </div>
            )}
        </div>
    );
}
