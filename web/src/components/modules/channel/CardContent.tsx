import { useState } from 'react';
import {
    Trash2,
    CheckCircle2,
    XCircle,
    FileText,
    DollarSign,
    Clock,
    Activity,
    TrendingUp,
    Globe,
    Key,
    SlidersHorizontal
} from 'lucide-react';
import { useUpdateChannel, useDeleteChannel, type Channel, type UpdateChannelRequest } from '@/api/endpoints/channel';
import {
    MorphingDialogTitle,
    MorphingDialogDescription,
    MorphingDialogClose,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';
import { Tabs, TabsContents, TabsContent } from '@/components/animate-ui/primitives/animate/tabs';
import { type StatsMetricsFormatted } from '@/api/endpoints/stats';
import { useTranslations } from 'next-intl';
import { Button } from '@/components/ui/button';
import { ChannelForm, normalizeBaseUrlForChannelType, type ChannelFormData } from './Form';
import { formatMoney } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { MonoSafeText, SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';

export function CardContent({ channel, stats }: { channel: Channel; stats: StatsMetricsFormatted }) {
    const { setIsOpen } = useMorphingDialog();
    const updateChannel = useUpdateChannel();
    const deleteChannel = useDeleteChannel();
    const [isEditing, setIsEditing] = useState(false);
    const [advancedOpen, setAdvancedOpen] = useState(false);
    const [isConfirmingDelete, setIsConfirmingDelete] = useState(false);
    const [formData, setFormData] = useState<ChannelFormData>({
        name: channel.name,
        type: channel.type,
        priority: channel.priority ?? 0,
        max_concurrent: channel.max_concurrent ?? 0,
        rpm_limit: channel.rpm_limit ?? 0,
        key_select_strategy: channel.key_select_strategy ?? 0,
        disable_circuit_breaker: channel.disable_circuit_breaker ?? false,
        enabled: channel.enabled,
        base_urls: channel.base_urls?.length ? channel.base_urls : [{ url: '', delay: 0 }],
        custom_header: channel.custom_header ?? [],
        cloak_mode: channel.cloak?.mode || 'auto',
        cloak_profile_id: channel.cloak?.profile_id ?? 0,
        channel_proxy: channel.channel_proxy ?? '',
        param_override: channel.param_override ?? '',
        system_prompt_override: channel.system_prompt_override ?? '',
        prompt_override_mode: channel.prompt_override_mode ?? 'append_system',
        keys: channel.keys.length > 0
            ? channel.keys.map((k) => ({
                id: k.id,
                enabled: k.enabled,
                channel_key: k.channel_key,
                status_code: k.status_code,
                last_use_time_stamp: k.last_use_time_stamp,
                total_cost: k.total_cost,
                remark: k.remark,
            }))
            : [{ enabled: true, channel_key: '', remark: '' }],
        model: channel.model,
        custom_model: channel.custom_model,
        discovered_models: channel.discovered_models ?? [],
        selected_models: channel.selected_models ?? [],
        anthropic_context_1m: channel.anthropic_context_1m ?? false,
        thinking_to_content: channel.thinking_to_content ?? false,
        proxy: channel.proxy,
        auto_sync: channel.auto_sync,
        match_regex: channel.match_regex ?? '',
        openai_chat_path: channel.openai_chat_path ?? '',
        openai_models_path: channel.openai_models_path ?? '',
        // Load the saved mapping into the edit form so reopening a channel SHOWS the
        // mapping the user saved (previously omitted here, so the rows always rendered
        // empty on open — "点了保存 点开看不到").
        model_mapping: channel.model_mapping,
    });
    const t = useTranslations('channel.detail');

    const currentView = isEditing ? 'editing' : 'viewing';

    const baseUrlsEqual = (a: Channel['base_urls'] | undefined, b: Channel['base_urls'] | undefined) =>
        JSON.stringify(a ?? []) === JSON.stringify(b ?? []);
    const headersEqual = (a: Channel['custom_header'] | undefined, b: Channel['custom_header'] | undefined) =>
        JSON.stringify(a ?? []) === JSON.stringify(b ?? []);

    const handleUpdate = (event: React.FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const req: UpdateChannelRequest = { id: channel.id };

        // only send changed fields to avoid accidental clears
        if (formData.name !== channel.name) req.name = formData.name;
        if (formData.type !== channel.type) req.type = formData.type;
        if ((formData.priority ?? 0) !== (channel.priority ?? 0)) req.priority = formData.priority ?? 0;
        if ((formData.max_concurrent ?? 0) !== (channel.max_concurrent ?? 0)) req.max_concurrent = formData.max_concurrent ?? 0;
        if ((formData.rpm_limit ?? 0) !== (channel.rpm_limit ?? 0)) req.rpm_limit = formData.rpm_limit ?? 0;
        if ((formData.key_select_strategy ?? 0) !== (channel.key_select_strategy ?? 0)) req.key_select_strategy = formData.key_select_strategy ?? 0;
        if ((formData.disable_circuit_breaker ?? false) !== (channel.disable_circuit_breaker ?? false)) req.disable_circuit_breaker = formData.disable_circuit_breaker ?? false;
        if (formData.enabled !== channel.enabled) req.enabled = formData.enabled;
        const normalizedBaseUrls = (formData.base_urls ?? []).filter((u) => u.url.trim()).map((u) => ({
            url: normalizeBaseUrlForChannelType(formData.type, u.url),
            delay: Number(u.delay || 0),
        }));
        if (formData.type !== channel.type || !baseUrlsEqual(normalizedBaseUrls, channel.base_urls)) {
            req.base_urls = normalizedBaseUrls;
        }
        if (formData.model !== channel.model) req.model = formData.model;
        if (formData.custom_model !== channel.custom_model) req.custom_model = formData.custom_model;
        if (JSON.stringify(formData.selected_models ?? []) !== JSON.stringify(channel.selected_models ?? [])) req.selected_models = formData.selected_models;
        if (JSON.stringify(formData.discovered_models ?? []) !== JSON.stringify(channel.discovered_models ?? [])) req.discovered_models = formData.discovered_models;
        if (formData.anthropic_context_1m !== (channel.anthropic_context_1m ?? false)) req.anthropic_context_1m = formData.anthropic_context_1m;
        if (formData.thinking_to_content !== (channel.thinking_to_content ?? false)) req.thinking_to_content = formData.thinking_to_content;
        if (formData.proxy !== channel.proxy) req.proxy = formData.proxy;
        if (formData.auto_sync !== channel.auto_sync) req.auto_sync = formData.auto_sync;

        if (!headersEqual(formData.custom_header, channel.custom_header)) {
            req.custom_header = (formData.custom_header ?? [])
                .map((h) => ({ header_key: h.header_key.trim(), header_value: h.header_value }))
                .filter((h) => h.header_key && h.header_value !== '');
        }

        const nextCloakMode = formData.cloak_mode || 'auto';
        const curCloakMode = channel.cloak?.mode || 'auto';
        const nextCloakProfileID = formData.cloak_profile_id ?? 0;
        const curCloakProfileID = channel.cloak?.profile_id ?? 0;
        if (nextCloakMode !== curCloakMode || nextCloakProfileID !== curCloakProfileID) {
            req.cloak = { mode: nextCloakMode, profile_id: nextCloakProfileID };
        }

        const nextChannelProxy = formData.channel_proxy.trim();
        const curChannelProxy = channel.channel_proxy ?? '';
        if (nextChannelProxy !== curChannelProxy) {
            // Empty string means "clear" for patch semantics; backend maps it to NULL.
            req.channel_proxy = nextChannelProxy;
        }

        const nextParamOverride = formData.param_override.trim();
        const curParamOverride = channel.param_override ?? '';
        if (nextParamOverride !== curParamOverride) {
            // Empty string means "clear" for patch semantics; backend maps it to NULL.
            req.param_override = nextParamOverride;
        }

        const nextSystemPromptOverride = formData.system_prompt_override.trim();
        const curSystemPromptOverride = channel.system_prompt_override ?? '';
        if (nextSystemPromptOverride !== curSystemPromptOverride) {
            req.system_prompt_override = nextSystemPromptOverride;
        }
        if (formData.prompt_override_mode !== (channel.prompt_override_mode ?? 'append_system')) {
            req.prompt_override_mode = formData.prompt_override_mode;
        }

        const nextMatchRegex = formData.match_regex.trim();
        const curMatchRegex = channel.match_regex ?? '';
        if (nextMatchRegex !== curMatchRegex) {
            // Empty string means "clear" for patch semantics; backend maps it to NULL.
            req.match_regex = nextMatchRegex;
        }

        const nextOpenAIChatPath = formData.openai_chat_path.trim();
        const curOpenAIChatPath = channel.openai_chat_path ?? '';
        if (nextOpenAIChatPath !== curOpenAIChatPath) {
            req.openai_chat_path = nextOpenAIChatPath;
        }

        const nextOpenAIModelsPath = formData.openai_models_path.trim();
        const curOpenAIModelsPath = channel.openai_models_path ?? '';
        if (nextOpenAIModelsPath !== curOpenAIModelsPath) {
            req.openai_models_path = nextOpenAIModelsPath;
        }

        // Send the model-mapping table when it changed (add / edit / clear). An empty
        // object is the explicit "clear" signal the backend maps to no mapping. Without
        // this the edit form never persisted mapping changes.
        if (JSON.stringify(formData.model_mapping ?? {}) !== JSON.stringify(channel.model_mapping ?? {})) {
            req.model_mapping = formData.model_mapping ?? {};
        }

        const originalKeys = channel.keys;
        const originalByID = new Map(originalKeys.map((k) => [k.id, k]));
        const nextKeys = formData.keys ?? [];

        const nextIDs = new Set(nextKeys.filter((k) => typeof k.id === 'number').map((k) => k.id as number));
        const keys_to_delete = originalKeys.filter((k) => !nextIDs.has(k.id)).map((k) => k.id);

        const keys_to_add = nextKeys
            .filter((k) => !k.id && k.channel_key.trim())
            .map((k) => ({ enabled: k.enabled, channel_key: k.channel_key, remark: k.remark ?? '' }));

        const keys_to_update = nextKeys
            .filter((k) => typeof k.id === 'number' && originalByID.has(k.id as number))
            .map((k) => {
                const orig = originalByID.get(k.id as number)!;
                const u: { id: number; enabled?: boolean; channel_key?: string; remark?: string } = { id: k.id as number };
                if (k.enabled !== orig.enabled) u.enabled = k.enabled;
                if (k.channel_key !== orig.channel_key) u.channel_key = k.channel_key;
                if ((k.remark ?? '') !== orig.remark) u.remark = k.remark ?? '';
                return Object.keys(u).length > 1 ? u : null;
            })
            .filter((u) => u !== null) as Array<{ id: number; enabled?: boolean; channel_key?: string; remark?: string }>;

        if (keys_to_add.length > 0) req.keys_to_add = keys_to_add;
        if (keys_to_update.length > 0) req.keys_to_update = keys_to_update;
        if (keys_to_delete.length > 0) req.keys_to_delete = keys_to_delete;

        updateChannel.mutate(req, {
            onSuccess: () => {
                setIsEditing(false);
                setAdvancedOpen(false);
                setIsOpen(false);
            }
        });
    };

    const handleDeleteClick = () => {
        if (!isConfirmingDelete) {
            setIsConfirmingDelete(true);
            return;
        }

        setIsOpen(false);
        setTimeout(() => {
            deleteChannel.mutate(channel.id);
        }, 300);
    };

    return (
        <div className="flex h-full min-h-0 flex-1 flex-col overflow-hidden">
            <MorphingDialogTitle className="shrink-0">
                <header className="mb-4 flex items-center justify-between gap-2 border-b border-border/40 pb-3">
                    <h2 className="whitespace-nowrap text-xl font-bold tabular-nums text-card-foreground sm:text-2xl">
                        {isEditing ? t('title.edit') : t('title.view')}
                    </h2>
                    <MorphingDialogClose
                        className="relative right-0 top-0 text-muted-foreground transition-colors hover:text-foreground"
                        variants={{
                            initial: { opacity: 0, scale: 0.8 },
                            animate: { opacity: 1, scale: 1 },
                            exit: { opacity: 0, scale: 0.8 }
                        }}
                    />
                </header>
            </MorphingDialogTitle>

            <MorphingDialogDescription className="flex min-h-0 flex-1 flex-col overflow-hidden">
                <Tabs value={currentView} className="flex min-h-0 flex-1 flex-col overflow-hidden">
                    <TabsContents className="flex min-h-0 flex-1 flex-col overflow-hidden">
                        <TabsContent value="viewing" className="flex min-h-0 flex-1 flex-col overflow-hidden">
                            <div className="min-h-0 flex-1 overflow-y-auto pr-1">
                                <div className="space-y-4 pb-4 sm:space-y-5">
                                    <dl className="grid gap-3 grid-cols-1 sm:grid-cols-3">
                                        <div className="rounded-lg border border-border bg-muted/20 p-3 sm:p-4">
                                            <dt className="flex items-center gap-2 mb-2 text-xs font-medium text-muted-foreground">
                                                <Activity className="size-4 text-muted-foreground" />
                                                {t('metrics.totalRequests')}
                                            </dt>
                                            <dd className="text-xl sm:text-2xl font-bold text-foreground">
                                                {stats.request_count.formatted.value}
                                                <span className="text-xs font-normal ml-1 text-muted-foreground">{stats.request_count.formatted.unit}</span>
                                            </dd>
                                        </div>

                                        <div className="rounded-lg border border-border bg-muted/20 p-3 sm:p-4">
                                            <dt className="flex items-center gap-2 mb-2 text-xs font-medium text-muted-foreground">
                                                <FileText className="size-4 text-muted-foreground" />
                                                {t('metrics.totalToken')}
                                            </dt>
                                            <dd className="text-xl sm:text-2xl font-bold text-foreground">
                                                {stats.total_token.formatted.value}
                                                <span className="text-xs font-normal ml-1 text-muted-foreground">{stats.total_token.formatted.unit}</span>
                                            </dd>
                                        </div>

                                        <div className="rounded-lg border border-border bg-muted/20 p-3 sm:p-4">
                                            <dt className="flex items-center gap-2 mb-2 text-xs font-medium text-muted-foreground">
                                                <DollarSign className="size-4 text-muted-foreground" />
                                                {t('metrics.totalCost')}
                                            </dt>
                                            <dd className="whitespace-nowrap text-xl font-bold tabular-nums text-foreground sm:text-2xl">
                                                {stats.total_cost.formatted.value}
                                                <span className="text-xs font-normal ml-1 text-muted-foreground">{stats.total_cost.formatted.unit}</span>
                                            </dd>
                                        </div>
                                    </dl>

                                    {/* 请求详情 */}
                                    <section className="space-y-3">
                                        <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                                            <TrendingUp className="size-3.5" />
                                            {t('sections.requests')}
                                        </h4>
                                        <dl className="grid gap-3 grid-cols-1 sm:grid-cols-2">
                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <CheckCircle2 className="size-4 text-accent" />
                                                    {t('metrics.successRequests')}
                                                </dt>
                                                <dd className="text-2xl font-bold text-accent">
                                                    {stats.request_success.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.request_success.formatted.unit}</span>
                                                </dd>
                                            </div>

                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <XCircle className="size-4 text-destructive" />
                                                    {t('metrics.failedRequests')}
                                                </dt>
                                                <dd className="text-2xl font-bold text-destructive">
                                                    {stats.request_failed.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.request_failed.formatted.unit}</span>
                                                </dd>
                                            </div>
                                        </dl>
                                    </section>

                                    {/* Token 使用 */}
                                    <section className="space-y-3">
                                        <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                                            <FileText className="size-3.5" />
                                            {t('sections.tokens')}
                                        </h4>
                                        <dl className="grid gap-3 grid-cols-1 sm:grid-cols-2">
                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <div className="size-2 rounded-full bg-chart-1" />
                                                    {t('metrics.inputToken')}
                                                </dt>
                                                <dd className="whitespace-nowrap text-2xl font-bold tabular-nums text-card-foreground">
                                                    {stats.input_token.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.input_token.formatted.unit}</span>
                                                </dd>
                                            </div>

                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <div className="size-2 rounded-full bg-chart-3" />
                                                    {t('metrics.outputToken')}
                                                </dt>
                                                <dd className="whitespace-nowrap text-2xl font-bold tabular-nums text-card-foreground">
                                                    {stats.output_token.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.output_token.formatted.unit}</span>
                                                </dd>
                                            </div>
                                        </dl>
                                    </section>

                                    {/* 成本详情 */}
                                    <section className="space-y-3">
                                        <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                                            <DollarSign className="size-3.5" />
                                            {t('sections.costs')}
                                        </h4>
                                        <dl className="grid gap-3 grid-cols-1 sm:grid-cols-2">
                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <div className="size-2 rounded-full bg-chart-2" />
                                                    {t('metrics.inputCost')}
                                                </dt>
                                                <dd className="whitespace-nowrap text-2xl font-bold tabular-nums text-card-foreground">
                                                    {stats.input_cost.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.input_cost.formatted.unit}</span>
                                                </dd>
                                            </div>

                                            <div className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                                <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                                    <div className="size-2 rounded-full bg-chart-5" />
                                                    {t('metrics.outputCost')}
                                                </dt>
                                                <dd className="whitespace-nowrap text-2xl font-bold tabular-nums text-card-foreground">
                                                    {stats.output_cost.formatted.value}
                                                    <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.output_cost.formatted.unit}</span>
                                                </dd>
                                            </div>
                                        </dl>
                                    </section>

                                    {/* Base URLs */}
                                    <section className="space-y-3">
                                        <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                                            <Globe className="size-3.5" />
                                            {t('sections.baseUrls')}
                                        </h4>
                                        <div className="rounded-2xl border bg-card overflow-hidden">
                                            {channel.base_urls?.map((url, i) => (
                                                <div key={i} className="flex items-center justify-between p-3 sm:p-4 border-b last:border-0 hover:bg-accent/5 transition-colors">
                                                    <div className="flex flex-col gap-1 min-w-0">
                                                        <MonoSafeText value={url.url} className="text-sm select-all" />
                                                    </div>
                                                    <Badge
                                                        variant="secondary"
                                                        className={cn(
                                                            "h-5 px-1.5 text-xs",
                                                            url.delay < 300
                                                                ? "bg-green-500/15 text-green-700 dark:text-green-400"
                                                                : url.delay < 1000
                                                                    ? "bg-orange-500/15 text-orange-700 dark:text-orange-400"
                                                                    : "bg-red-500/15 text-red-700 dark:text-red-400"
                                                        )}
                                                    >
                                                        {url.delay}ms
                                                    </Badge>
                                                </div>
                                            ))}
                                            {(!channel.base_urls || channel.base_urls.length === 0) && (
                                                <div className="p-4 text-sm text-muted-foreground text-center">{t('noBaseUrls')}</div>
                                            )}
                                        </div>
                                    </section>

                                    {/* Keys */}
                                    <section className="space-y-3">
                                        <h4 className="flex items-center gap-2 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                                            <Key className="size-3.5" />
                                            {t('sections.keys')}
                                        </h4>
                                        <div className="rounded-2xl border bg-card overflow-hidden">
                                            {channel.keys?.map((key) => (
                                                <div key={key.id} className="flex items-center gap-3 p-3 sm:p-4 border-b last:border-0 hover:bg-accent/5 transition-colors">
                                                    <div className={cn("size-2 shrink-0 rounded-full", key.enabled ? "bg-emerald-500" : "bg-destructive")} />

                                                    <MonoSafeText
                                                        value={key.channel_key.length > 10
                                                            ? `${key.channel_key.slice(0, 4)}...${key.channel_key.slice(-4)}`
                                                            : key.channel_key}
                                                        title={key.channel_key}
                                                        className="text-sm flex-1"
                                                    />

                                                    {key.remark && (
                                                        <SafeText value={key.remark} className="text-xs text-muted-foreground max-w-24" />
                                                    )}

                                                    <div className="flex items-center gap-2 shrink-0">
                                                        {key.last_use_time_stamp > 0 && (
                                                            <span className="text-xs text-muted-foreground whitespace-nowrap hidden sm:inline-block">
                                                                {new Date(key.last_use_time_stamp * 1000).toLocaleString()}
                                                            </span>
                                                        )}

                                                        {key.status_code !== 0 && (
                                                            <Badge
                                                                variant="secondary"
                                                                className={cn(
                                                                    "h-5 px-1.5 text-[10px]",
                                                                    key.status_code === 200
                                                                        ? "bg-green-500/15 text-green-700 dark:text-green-400"
                                                                        : key.status_code === 401 ||
                                                                            key.status_code === 403 ||
                                                                            key.status_code === 429 ||
                                                                            key.status_code >= 500
                                                                            ? "bg-red-500/15 text-red-700 dark:text-red-400"
                                                                            : "bg-orange-500/15 text-orange-700 dark:text-orange-400"
                                                                )}
                                                            >
                                                                {key.status_code}
                                                            </Badge>
                                                        )}

                                                        <Badge variant="secondary" className="h-5 whitespace-nowrap px-1.5 text-[10px] tabular-nums">
                                                            {formatMoney(key.total_cost).formatted.value}
                                                            {formatMoney(key.total_cost).formatted.unit}
                                                        </Badge>
                                                    </div>
                                                </div>
                                            ))}
                                            {(!channel.keys || channel.keys.length === 0) && (
                                                <div className="p-4 text-sm text-muted-foreground text-center">{t('noKeys')}</div>
                                            )}
                                        </div>
                                    </section>

                                    {/* 等待时间 */}
                                    <dl className="rounded-2xl border bg-card p-3 sm:p-4 transition-colors hover:bg-accent/5">
                                        <dt className="flex items-center gap-2 mb-2 text-xs text-muted-foreground">
                                            <Clock className="size-4 text-primary" />
                                            {t('metrics.avgWaitTime')}
                                        </dt>
                                        <dd className="text-2xl font-bold text-primary">
                                            {stats.wait_time.formatted.value}
                                            <span className="text-sm font-normal ml-1 text-muted-foreground">{stats.wait_time.formatted.unit}</span>
                                        </dd>
                                    </dl>
                                </div>
                            </div>

                            {/* 操作按钮 */}
                            <div className="grid shrink-0 gap-3 border-t border-border/60 bg-card pt-3 sm:grid-cols-2">
                                <Button
                                    onClick={() => {
                                        if (isConfirmingDelete) {
                                            setIsConfirmingDelete(false);
                                            return;
                                        }
                                        setAdvancedOpen(false);
                                        setIsEditing(true);
                                    }}
                                    variant={isConfirmingDelete ? 'secondary' : 'default'}
                                    className="w-full rounded-2xl h-12"
                                >
                                    {isConfirmingDelete ? t('actions.cancel') : t('actions.edit')}
                                </Button>
                                <Button
                                    onClick={handleDeleteClick}
                                    disabled={deleteChannel.isPending}
                                    variant="destructive"
                                    className="w-full rounded-2xl h-12"
                                >
                                    <Trash2 className={`size-4 transition-transform ${isConfirmingDelete ? 'scale-110' : ''}`} />
                                    {deleteChannel.isPending
                                        ? t('actions.deleting')
                                        : isConfirmingDelete
                                            ? t('actions.confirmDelete')
                                            : t('actions.delete')}
                                </Button>
                            </div>
                        </TabsContent>

                        <TabsContent value="editing" className="flex min-h-0 flex-1 flex-col overflow-hidden">
                            <div className="mb-4 flex shrink-0 flex-wrap items-center justify-between gap-3 rounded-lg border bg-muted/20 px-3 py-2">
                                <div className="min-w-0">
                                    <div className="truncate text-sm font-medium text-card-foreground">{channel.name}</div>
                                    <div className="text-xs text-muted-foreground">#{channel.id}</div>
                                </div>
                                <Button
                                    type="button"
                                    variant={advancedOpen ? 'default' : 'outline'}
                                    size="sm"
                                    onClick={() => setAdvancedOpen((open) => !open)}
                                    className="h-8 rounded-lg"
                                >
                                    <SlidersHorizontal className="mr-1.5 size-3.5" />
                                    高级设定
                                </Button>
                            </div>
                            <div className="min-h-0 flex-1 overflow-y-auto pr-1">
                                <ChannelForm
                                    formData={formData}
                                    onFormDataChange={setFormData}
                                    onSubmit={handleUpdate}
                                    isPending={updateChannel.isPending}
                                    submitText={t('actions.save')}
                                    pendingText={t('actions.saving')}
                                    onCancel={() => {
                                        setAdvancedOpen(false);
                                        setIsEditing(false);
                                    }}
                                    cancelText={t('actions.cancel')}
                                    idPrefix="channel"
                                    advancedMode="panel"
                                    onAdvancedClose={() => setAdvancedOpen(false)}
                                    advancedOpen={advancedOpen}
                                />
                            </div>
                        </TabsContent>
                    </TabsContents>
                </Tabs>
            </MorphingDialogDescription>
        </div>
    );
}
