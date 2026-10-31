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
import { routePromptRows as buildRoutePromptRows, sortChannelsStable } from './prompt-rows';
import { normalizePromptMode, promptEnabledFromSaved, promptLayerActive, promptStatus } from './prompt-mode';
import { PromptLayerControls } from './PromptLayerControls';

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function PromptStatusBadge({ enabled, mode, text }: { enabled: boolean; mode: PromptOverrideMode; text: string }) {
    const status = promptStatus(enabled, mode, text);
    return (
        <Badge variant={status.tone === 'on' ? 'secondary' : 'outline'} title={status.hint}>
            {status.label}
        </Badge>
    );
}

function promptTextareaClass(enabled: boolean, size: string) {
    return cn(
        size,
        'w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground transition-opacity focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
        !enabled && 'bg-muted/40 text-muted-foreground opacity-60'
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
                        <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? '已启用' : '未启用'}</Badge>
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
                            你的请求仍然可以自带 system / developer 提示词；管理员可以在全局、方案、模型路由、渠道四层把提示词放在你的提示词前面或后面，你自带的不会被删。为避免泄露管理策略，普通用户不展示完整覆盖内容。
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
                    description="管理员可在全局、方案、模型路由、渠道四层给请求加系统提示词，不会删掉你自带的。"
                    enabled
                />
                <LayerCard
                    icon={GitBranch}
                    title="模型路由"
                    description="如果你的 API Key 绑定了不同方案，同一个请求模型可能应用不同的提示词策略。"
                    enabled
                />
                <LayerCard
                    icon={Radio}
                    title="渠道约束"
                    description="最终转发到不同上游渠道时，渠道级提示词排在最外面。"
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
    const savedMode = settings?.find((item) => item.key === SettingKey.PromptOverrideMode)?.value;
    const savedEnabled = settings?.find((item) => item.key === SettingKey.PromptOverrideEnabled)?.value === 'true';
    const [prompt, setPrompt] = useState(initialPrompt);
    const [mode, setMode] = useState<PromptOverrideMode>(normalizePromptMode(savedMode));
    const [enabled, setEnabled] = useState(promptEnabledFromSaved(savedEnabled, savedMode, initialPrompt));

    const save = async () => {
        try {
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideMode, value: mode });
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideSystem, value: prompt.trim() });
            await setSetting.mutateAsync({ key: SettingKey.PromptOverrideEnabled, value: enabled ? 'true' : 'false' });
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
                        <PromptStatusBadge enabled={enabled} mode={mode} text={prompt} />
                    </h3>
                    <p className="mt-1 text-xs text-muted-foreground">全站所有请求都会带上，离请求自带的提示词最近。</p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <PromptLayerControls enabled={enabled} onEnabledChange={setEnabled} mode={mode} onModeChange={setMode} label="全局提示词" />
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
                disabled={!enabled}
                title={enabled ? undefined : '已关闭，文字保留；打开「启用」后才能改、才会生效。'}
                className={promptTextareaClass(enabled, 'min-h-36 rounded-2xl')}
            />
        </section>
    );
}

function PlanPromptRow({ plan }: { plan: AccessPlan }) {
    const updatePlan = useUpdateAccessPlan();
    const [prompt, setPrompt] = useState(plan.system_prompt_override ?? '');
    const [mode, setMode] = useState<PromptOverrideMode>(normalizePromptMode(plan.prompt_override_mode));
    const [enabled, setEnabled] = useState(
        promptEnabledFromSaved(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override)
    );

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
                prompt_override_enabled: enabled,
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
                        <PromptStatusBadge enabled={enabled} mode={mode} text={prompt} />
                    </div>
                    <p className="mt-1 font-mono text-xs text-muted-foreground">{plan.slug}</p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <PromptLayerControls enabled={enabled} onEnabledChange={setEnabled} mode={mode} onModeChange={setMode} label={`${plan.slug} 提示词`} />
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
                disabled={!enabled}
                title={enabled ? undefined : '已关闭，文字保留；打开「启用」后才能改、才会生效。'}
                className={promptTextareaClass(enabled, 'min-h-24')}
            />
        </div>
    );
}

function ChannelPromptRow({ channel }: { channel: Channel }) {
    const updateChannel = useUpdateChannel();
    const [prompt, setPrompt] = useState(channel.system_prompt_override ?? '');
    const [mode, setMode] = useState<PromptOverrideMode>(normalizePromptMode(channel.prompt_override_mode));
    const [enabled, setEnabled] = useState(
        promptEnabledFromSaved(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override)
    );

    const save = () => {
        updateChannel.mutate(
            {
                id: channel.id,
                system_prompt_override: prompt.trim(),
                prompt_override_mode: mode,
                prompt_override_enabled: enabled,
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
                        <PromptStatusBadge enabled={enabled} mode={mode} text={prompt} />
                    </div>
                    <p className="mt-1 text-xs text-muted-foreground">最终上游渠道层，排在最外面。</p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <PromptLayerControls enabled={enabled} onEnabledChange={setEnabled} mode={mode} onModeChange={setMode} label={`${channel.name} 提示词`} />
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
                disabled={!enabled}
                title={enabled ? undefined : '已关闭，文字保留；打开「启用」后才能改、才会生效。'}
                className={promptTextareaClass(enabled, 'min-h-24')}
            />
        </div>
    );
}

function AdminPromptView() {
    const setActiveItem = useNavStore((state) => state.setActiveItem);
    const { data: settings, isLoading: settingsLoading } = useSettingList();
    const { data: plans = [], isLoading: plansLoading } = useAccessPlanList();
    const { data: channelItems = [], isLoading: channelsLoading } = useChannelList();
    const channels = useMemo(() => sortChannelsStable(channelItems.map((item) => item.raw)), [channelItems]);

    const globalPrompt = settings?.find((item) => item.key === SettingKey.PromptOverrideSystem)?.value ?? '';
    const globalMode = settings?.find((item) => item.key === SettingKey.PromptOverrideMode)?.value ?? '';
    const globalEnabled = settings?.find((item) => item.key === SettingKey.PromptOverrideEnabled)?.value ?? '';
    const globalActive = promptLayerActive(globalEnabled === 'true', globalMode, globalPrompt);
    const planPromptCount = plans.filter((plan) =>
        promptLayerActive(plan.prompt_override_enabled, plan.prompt_override_mode, plan.system_prompt_override)
    ).length;
    const routePromptRows = useMemo(() => buildRoutePromptRows(plans), [plans]);
    const routeActiveCount = routePromptRows.filter((row) => row.enabled).length;
    const channelPromptCount = channels.filter((channel) =>
        promptLayerActive(channel.prompt_override_enabled, channel.prompt_override_mode, channel.system_prompt_override)
    ).length;

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div>
                        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                            <MessageSquareText className="size-3.5" />
                            提示词
                        </div>
                        <h2 className="text-2xl font-bold">提示词管理</h2>
                        <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">
                            每层都能单独开关，并选「放在前面」或「放在后面」——放在请求自带的系统提示词前面或后面，原内容不删；请求没带就直接用这段。几层叠加时越具体的层排得越靠外：渠道 › 路由 › 方案 › 全局 › 请求自带 › 全局 › 方案 › 路由 › 渠道。
                        </p>
                    </div>
                    <Button type="button" variant="outline" className="rounded-xl" onClick={() => setActiveItem('access-plan')}>
                        打开模型路由
                        <ArrowRight className="size-4" />
                    </Button>
                </div>
            </section>

            <div className="grid gap-4 md:grid-cols-4">
                <LayerCard icon={ShieldCheck} title="全局默认" description="全站所有请求。" enabled={globalActive} />
                <LayerCard icon={Layers3} title="方案" description={`${planPromptCount} 个方案已启用。`} enabled={planPromptCount > 0} />
                <LayerCard icon={GitBranch} title="模型路由" description={`${routeActiveCount} 条路由已启用。`} enabled={routeActiveCount > 0} />
                <LayerCard icon={Radio} title="渠道" description={`${channelPromptCount} 个渠道已启用。`} enabled={channelPromptCount > 0} />
            </div>

            {settingsLoading ? (
                <div className="flex justify-center rounded-3xl border border-border bg-card py-10 text-muted-foreground">
                    <Loader2 className="size-5 animate-spin" />
                </div>
            ) : (
                <GlobalPromptEditor
                    key={`${globalPrompt}-${globalMode}-${globalEnabled}`}
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
                                key={`${plan.id}-${plan.system_prompt_override ?? ''}-${plan.prompt_override_mode ?? ''}-${plan.prompt_override_enabled ? 1 : 0}`}
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
                            模型路由提示词
                        </h3>
                        <p className="mt-1 text-xs text-muted-foreground">模型路由的提示词在「方案」页点模型的「编辑」里改，这里集中展示已有的。</p>
                    </div>
                    <Button type="button" variant="outline" size="sm" className="rounded-xl" onClick={() => setActiveItem('access-plan')}>
                        去映射矩阵编辑
                    </Button>
                </div>
                {routePromptRows.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        暂无模型路由提示词
                    </div>
                ) : (
                    <div className="grid gap-2">
                        {routePromptRows.slice(0, 8).map(({ plan, requestModel, upstreamModels, targetCount, mode, enabled, text }) => (
                            <div key={`${plan.id}-${requestModel}`} className="flex flex-wrap items-center gap-2 rounded-2xl border border-border bg-background/50 px-3 py-2 text-sm">
                                <Badge>{plan.display_name || plan.slug}</Badge>
                                <span className="font-mono">{requestModel}</span>
                                <ArrowRight className="size-3 text-muted-foreground" />
                                <span className="font-mono">{upstreamModels.join(' / ') || '-'}</span>
                                {targetCount > 1 && <span className="text-xs text-muted-foreground">{targetCount} 个目标</span>}
                                <PromptStatusBadge enabled={enabled} mode={mode} text={text} />
                            </div>
                        ))}
                        {routePromptRows.length > 8 && (
                            <p className="text-xs text-muted-foreground">还有 {routePromptRows.length - 8} 条，请到「方案」页查看。</p>
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
                                key={`${channel.id}-${channel.system_prompt_override ?? ''}-${channel.prompt_override_mode ?? ''}-${channel.prompt_override_enabled ? 1 : 0}`}
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
