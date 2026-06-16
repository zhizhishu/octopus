'use client';

import { useMemo, useState } from 'react';
import {
    ArrowRight,
    GitBranch,
    KeyRound,
    Layers3,
    Loader2,
    MessageSquareText,
    Radio,
    Save,
    ShieldCheck,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import type { ApiError } from '@/api/types';
import {
    type AccessPlan,
    type PromptOverrideMode,
    useAccessPlanList,
    useUpdateAccessPlan,
} from '@/api/endpoints/access-plan';
import { type Channel, useChannelList, useUpdateChannel } from '@/api/endpoints/channel';
import { SettingKey, useSetSetting, useSettingList } from '@/api/endpoints/setting';
import { useAuthStore } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { useNavStore } from '../navbar/nav-store';
import { cn } from '@/lib/utils';

const modeLabels: Record<PromptOverrideMode, string> = {
    append_system: '追加 system',
    replace_system: '替换 system',
};

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function PromptModeSelect({
    value,
    onChange,
    label,
}: {
    value: PromptOverrideMode;
    onChange: (value: PromptOverrideMode) => void;
    label: string;
}) {
    return (
        <select
            value={value}
            onChange={(event) => onChange(event.target.value as PromptOverrideMode)}
            aria-label={label}
            className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
        >
            <option value="append_system">{modeLabels.append_system}</option>
            <option value="replace_system">{modeLabels.replace_system}</option>
        </select>
    );
}

function LayerCard({
    icon: Icon,
    title,
    description,
    enabled,
}: {
    icon: LucideIcon;
    title: string;
    description: string;
    enabled: boolean;
}) {
    return (
        <div className="rounded-3xl border border-border bg-card p-4">
            <div className="flex items-start gap-3">
                <div className={cn(
                    'flex size-10 items-center justify-center rounded-2xl border',
                    enabled ? 'border-primary/30 bg-primary/10 text-primary' : 'border-border bg-muted/30 text-muted-foreground'
                )}>
                    <Icon className="size-5" />
                </div>
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="text-sm font-semibold">{title}</h3>
                        <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? '已配置' : '未配置'}</Badge>
                    </div>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{description}</p>
                </div>
            </div>
        </div>
    );
}

function UserPromptView() {
    const setActiveItem = useNavStore((state) => state.setActiveItem);

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div className="max-w-2xl">
                        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                            <MessageSquareText className="size-3.5" />
                            个人侧提示词
                        </div>
                        <h2 className="text-2xl font-bold">提示词由管理员统一管理</h2>
                        <p className="mt-2 text-sm leading-6 text-muted-foreground">
                            你的请求仍然可以自带 system / developer 提示词；Octopus 会在管理员配置的全局、方案、模型映射、渠道规则之后统一处理。为避免泄露管理策略，普通用户不展示完整覆盖内容。
                        </p>
                    </div>
                    <Button type="button" className="rounded-xl" onClick={() => setActiveItem('key')}>
                        <KeyRound className="size-4" />
                        查看 API Key
                    </Button>
                </div>
            </section>

            <div className="grid gap-4 md:grid-cols-3">
                <LayerCard
                    icon={ShieldCheck}
                    title="管理员策略"
                    description="管理员可按全局、方案、模型映射、渠道四层追加或替换 system 提示词。"
                    enabled
                />
                <LayerCard
                    icon={GitBranch}
                    title="模型映射"
                    description="如果你的 API Key 绑定了不同方案，同一个请求模型可能应用不同的提示词策略。"
                    enabled
                />
                <LayerCard
                    icon={Radio}
                    title="渠道约束"
                    description="最终转发到不同上游渠道时，渠道级提示词会作为最后一层约束。"
                    enabled
                />
            </div>
        </PageWrapper>
    );
}

function GlobalPromptEditor({
    settings,
}: {
    settings: Array<{ key: string; value: string }> | undefined;
}) {
    const setSetting = useSetSetting();
    const initialPrompt = settings?.find((item) => item.key === SettingKey.PromptOverrideSystem)?.value ?? '';
    const initialMode = (settings?.find((item) => item.key === SettingKey.PromptOverrideMode)?.value as PromptOverrideMode | undefined) ?? 'append_system';
    const [prompt, setPrompt] = useState(initialPrompt);
    const [mode, setMode] = useState<PromptOverrideMode>(initialMode);

    const save = async () => {
        try {
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideMode, value: mode });
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideSystem, value: prompt.trim() });
            toast.success('全局提示词已保存');
        } catch (error) {
            toast.error('全局提示词保存失败', { description: apiErrorMessage(error) });
        }
    };

    return (
        <section className="rounded-3xl border border-border bg-card p-5">
            <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                <div>
                    <h3 className="flex items-center gap-2 text-base font-semibold">
                        <ShieldCheck className="size-5" />
                        全局默认提示词
                    </h3>
                    <p className="mt-1 text-xs text-muted-foreground">最先应用，全站默认；留空表示不注入全局提示词。</p>
                </div>
                <div className="flex items-center gap-2">
                    <PromptModeSelect value={mode} onChange={setMode} label="全局提示词模式" />
                    <Button type="button" className="rounded-xl" disabled={setSetting.isPending} onClick={save}>
                        {setSetting.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        保存
                    </Button>
                </div>
            </div>
            <textarea
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                placeholder="可选：全站默认 system 提示词"
                className="min-h-36 w-full rounded-2xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
        </section>
    );
}

function PlanPromptRow({ plan }: { plan: AccessPlan }) {
    const updatePlan = useUpdateAccessPlan();
    const [prompt, setPrompt] = useState(plan.system_prompt_override ?? '');
    const [mode, setMode] = useState<PromptOverrideMode>(plan.prompt_override_mode ?? 'append_system');

    const save = () => {
        updatePlan.mutate(
            {
                id: plan.id,
                slug: plan.slug,
                display_name: plan.display_name,
                enabled: plan.enabled,
                is_default: plan.is_default,
                sort: plan.sort,
                system_prompt_override: prompt.trim(),
                prompt_override_mode: mode,
            },
            {
                onSuccess: () => toast.success(`${plan.display_name || plan.slug} 提示词已保存`),
                onError: (error) => toast.error('方案提示词保存失败', { description: apiErrorMessage(error) }),
            }
        );
    };

    return (
        <div className="grid gap-3 rounded-2xl border border-border bg-background/50 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="min-w-0">
                    <div className="flex items-center gap-2">
                        <span className="truncate text-sm font-semibold">{plan.display_name || plan.slug}</span>
                        {plan.is_default && <Badge>默认</Badge>}
                        <Badge variant={prompt.trim() ? 'secondary' : 'outline'}>
                            {prompt.trim() ? '已配置' : '未配置'}
                        </Badge>
                    </div>
                    <p className="mt-1 font-mono text-xs text-muted-foreground">{plan.slug}</p>
                </div>
                <div className="flex items-center gap-2">
                    <PromptModeSelect value={mode} onChange={setMode} label={`${plan.slug} 提示词模式`} />
                    <Button type="button" size="sm" className="rounded-xl" disabled={updatePlan.isPending} onClick={save}>
                        {updatePlan.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        保存
                    </Button>
                </div>
            </div>
            <textarea
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                placeholder="可选：该方案默认 system 提示词"
                className="min-h-24 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
        </div>
    );
}

function ChannelPromptRow({ channel }: { channel: Channel }) {
    const updateChannel = useUpdateChannel();
    const [prompt, setPrompt] = useState(channel.system_prompt_override ?? '');
    const [mode, setMode] = useState<PromptOverrideMode>(channel.prompt_override_mode ?? 'append_system');

    const save = () => {
        updateChannel.mutate(
            {
                id: channel.id,
                system_prompt_override: prompt.trim(),
                prompt_override_mode: mode,
            },
            {
                onSuccess: () => toast.success(`${channel.name} 渠道提示词已保存`),
                onError: (error) => toast.error('渠道提示词保存失败', { description: apiErrorMessage(error) }),
            }
        );
    };

    return (
        <div className="grid gap-3 rounded-2xl border border-border bg-background/50 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="min-w-0">
                    <div className="flex items-center gap-2">
                        <span className="truncate text-sm font-semibold">#{channel.id} {channel.name}</span>
                        <Badge variant={channel.enabled ? 'secondary' : 'outline'}>{channel.enabled ? '启用' : '停用'}</Badge>
                        <Badge variant={prompt.trim() ? 'secondary' : 'outline'}>
                            {prompt.trim() ? '已配置' : '未配置'}
                        </Badge>
                    </div>
                    <p className="mt-1 text-xs text-muted-foreground">最终上游渠道层，优先级最高。</p>
                </div>
                <div className="flex items-center gap-2">
                    <PromptModeSelect value={mode} onChange={setMode} label={`${channel.name} 提示词模式`} />
                    <Button type="button" size="sm" className="rounded-xl" disabled={updateChannel.isPending} onClick={save}>
                        {updateChannel.isPending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                        保存
                    </Button>
                </div>
            </div>
            <textarea
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                placeholder="可选：该渠道最终注入的 system 提示词"
                className="min-h-24 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
        </div>
    );
}

function AdminPromptView() {
    const setActiveItem = useNavStore((state) => state.setActiveItem);
    const { data: settings, isLoading: settingsLoading } = useSettingList();
    const { data: plans = [], isLoading: plansLoading } = useAccessPlanList();
    const { data: channelItems = [], isLoading: channelsLoading } = useChannelList();
    const channels = channelItems.map((item) => item.raw);

    const globalPrompt = settings?.find((item) => item.key === SettingKey.PromptOverrideSystem)?.value ?? '';
    const planPromptCount = plans.filter((plan) => (plan.system_prompt_override ?? '').trim()).length;
    const routePromptRows = useMemo(() => (
        plans.flatMap((plan) => (
            plan.route_targets
                .filter((target) => (target.system_prompt_override ?? '').trim())
                .map((target) => ({
                    plan,
                    target,
                }))
        ))
    ), [plans]);
    const channelPromptCount = channels.filter((channel) => (channel.system_prompt_override ?? '').trim()).length;

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div>
                        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                            <MessageSquareText className="size-3.5" />
                            设置 / 提示词管理
                        </div>
                        <h2 className="text-2xl font-bold">提示词管理</h2>
                        <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">
                            当前覆盖顺序为：全局默认、方案、模型映射、渠道。默认追加 system，只有显式选择替换时才替换用户原始 system。
                        </p>
                    </div>
                    <Button type="button" variant="outline" className="rounded-xl" onClick={() => setActiveItem('access-plan')}>
                        打开模型映射
                        <ArrowRight className="size-4" />
                    </Button>
                </div>
            </section>

            <div className="grid gap-4 md:grid-cols-4">
                <LayerCard icon={ShieldCheck} title="全局默认" description="全站第一层规则。" enabled={globalPrompt.trim().length > 0} />
                <LayerCard icon={Layers3} title="方案" description={`${planPromptCount} 个方案已配置。`} enabled={planPromptCount > 0} />
                <LayerCard icon={GitBranch} title="模型映射" description={`${routePromptRows.length} 条映射已配置。`} enabled={routePromptRows.length > 0} />
                <LayerCard icon={Radio} title="渠道" description={`${channelPromptCount} 个渠道已配置。`} enabled={channelPromptCount > 0} />
            </div>

            {settingsLoading ? (
                <div className="flex justify-center rounded-3xl border border-border bg-card py-10 text-muted-foreground">
                    <Loader2 className="size-5 animate-spin" />
                </div>
            ) : (
                <GlobalPromptEditor
                    key={`${globalPrompt}-${settings?.find((item) => item.key === SettingKey.PromptOverrideMode)?.value ?? 'append_system'}`}
                    settings={settings}
                />
            )}

            <section className="rounded-3xl border border-border bg-card p-5">
                <div className="mb-4 flex items-center justify-between gap-3">
                    <div>
                        <h3 className="flex items-center gap-2 text-base font-semibold">
                            <Layers3 className="size-5" />
                            方案提示词
                        </h3>
                        <p className="mt-1 text-xs text-muted-foreground">适合 VIP / SVIP / SSVIP 等方案统一约束。</p>
                    </div>
                    <Badge variant="outline">{plans.length}</Badge>
                </div>
                {plansLoading ? (
                    <div className="flex justify-center py-8 text-muted-foreground">
                        <Loader2 className="size-5 animate-spin" />
                    </div>
                ) : plans.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        暂无方案
                    </div>
                ) : (
                    <div className="grid gap-3">
                        {plans.map((plan) => (
                            <PlanPromptRow
                                key={`${plan.id}-${plan.system_prompt_override ?? ''}-${plan.prompt_override_mode ?? 'append_system'}`}
                                plan={plan}
                            />
                        ))}
                    </div>
                )}
            </section>

            <section className="rounded-3xl border border-border bg-card p-5">
                <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                    <div>
                        <h3 className="flex items-center gap-2 text-base font-semibold">
                            <GitBranch className="size-5" />
                            模型映射提示词
                        </h3>
                        <p className="mt-1 text-xs text-muted-foreground">模型映射级提示词仍在「方案」页面的矩阵里编辑，这里集中展示已有覆盖。</p>
                    </div>
                    <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={() => setActiveItem('access-plan')}>
                        去映射矩阵编辑
                    </Button>
                </div>
                {routePromptRows.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        暂无模型映射提示词
                    </div>
                ) : (
                    <div className="grid gap-2">
                        {routePromptRows.slice(0, 8).map(({ plan, target }) => (
                            <div key={`${plan.id}-${target.request_model}-${target.channel_id}-${target.upstream_model}`} className="flex flex-wrap items-center gap-2 rounded-2xl border border-border bg-background/50 px-3 py-2 text-sm">
                                <Badge>{plan.display_name || plan.slug}</Badge>
                                <span className="font-mono">{target.request_model}</span>
                                <ArrowRight className="size-3 text-muted-foreground" />
                                <span className="font-mono">{target.upstream_model}</span>
                                <Badge variant="outline">{modeLabels[target.prompt_override_mode ?? 'append_system']}</Badge>
                            </div>
                        ))}
                        {routePromptRows.length > 8 && (
                            <p className="text-xs text-muted-foreground">还有 {routePromptRows.length - 8} 条，请进入模型映射查看。</p>
                        )}
                    </div>
                )}
            </section>

            <section className="rounded-3xl border border-border bg-card p-5">
                <div className="mb-4 flex items-center justify-between gap-3">
                    <div>
                        <h3 className="flex items-center gap-2 text-base font-semibold">
                            <Radio className="size-5" />
                            渠道提示词
                        </h3>
                        <p className="mt-1 text-xs text-muted-foreground">最终转发到某个上游渠道时生效，适合安全边界或供应商特定约束。</p>
                    </div>
                    <Badge variant="outline">{channels.length}</Badge>
                </div>
                {channelsLoading ? (
                    <div className="flex justify-center py-8 text-muted-foreground">
                        <Loader2 className="size-5 animate-spin" />
                    </div>
                ) : channels.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        暂无渠道
                    </div>
                ) : (
                    <div className="grid gap-3">
                        {channels.map((channel) => (
                            <ChannelPromptRow
                                key={`${channel.id}-${channel.system_prompt_override ?? ''}-${channel.prompt_override_mode ?? 'append_system'}`}
                                channel={channel}
                            />
                        ))}
                    </div>
                )}
            </section>
        </PageWrapper>
    );
}

export function PromptManagement() {
    const role = useAuthStore((state) => state.user?.role);
    return role === 'admin' ? <AdminPromptView /> : <UserPromptView />;
}
