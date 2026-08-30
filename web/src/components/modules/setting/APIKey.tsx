'use client';

import { useCallback, useEffect, useId, useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { KeyRound, Plus, Loader, Trash2, Check, X, Info, CalendarDays, Pencil, Maximize2, BookOpen, Terminal, Search } from 'lucide-react';
import { motion, AnimatePresence } from 'motion/react';
import { Input } from '@/components/ui/input';
import { Calendar } from '@/components/ui/calendar';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { Switch } from '@/components/ui/switch';
import { Badge } from '@/components/ui/badge';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import {
    MorphingDialog,
    MorphingDialogContainer,
    MorphingDialogContent,
    MorphingDialogTrigger,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';
import {
    API_KEY_ENDPOINT_FAMILIES,
    useAPIKeyList,
    useCreateAPIKey,
    useUpdateAPIKey,
    useDeleteAPIKey,
    type APIKeyEndpointFamily,
    type APIKey,
} from '@/api/endpoints/apikey';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { useModelChannelList } from '@/api/endpoints/model';
import { useStatsAPIKey } from '@/api/endpoints/stats';
import { useAuthStore, useUserList } from '@/api/endpoints/user';
import { cn } from '@/lib/utils';
import { toast } from '@/components/common/Toast';
import { CopyIconButton } from '@/components/common/CopyButton';
import { MonoSafeText, SafeText } from '@/components/common/SafeText';
import type { ApiError } from '@/api/types';

function toExpireAt(date: Date, time: string): number {
    const t = /^\d{2}:\d{2}$/.test(time) ? time : '00:00';
    const [hh, mm] = t.split(':').map(Number);
    const d = new Date(Date.UTC(date.getFullYear(), date.getMonth(), date.getDate(), hh, mm, 0));
    // 返回 Unix 时间戳（秒）
    return Math.floor(d.getTime() / 1000);
}

function parseExpireDate(expireAt?: number): Date | undefined {
    if (!expireAt) return undefined;
    // 从 Unix 时间戳（秒）转换为 Date
    const d = new Date(expireAt * 1000);
    return isNaN(d.getTime()) ? undefined : d;
}

function normalizeHHmm(input: string): string {
    const cleaned = input.replace(/[^\d:]/g, '');
    const parts = cleaned.includes(':') ? cleaned.split(':') : [cleaned.slice(0, 2), cleaned.slice(2, 4)];
    const hh = Math.min(23, Math.max(0, parseInt(parts[0] || '0', 10)));
    const mm = Math.min(59, Math.max(0, parseInt(parts[1] || '0', 10)));
    return `${hh.toString().padStart(2, '0')}:${mm.toString().padStart(2, '0')}`;
}

function normalizeMoneyInput(input: string): string {
    const cleaned = input.replace(/[^\d.]/g, '');
    const [intPart, ...rest] = cleaned.split('.');
    return rest.length > 0 ? `${intPart}.${rest.join('').slice(0, 6)}` : intPart;
}

function toggleModel(current: string | undefined, model: string): string | undefined {
    const models = current ? current.split(',').filter(Boolean) : [];
    const next = models.includes(model)
        ? models.filter((m) => m !== model)
        : [...models, model];
    return next.length ? next.join(',') : undefined;
}

function hasModel(supported: string | undefined, model: string): boolean {
    return supported ? supported.split(',').includes(model) : false;
}

function toggleAccessPlan(current: number[] | undefined, id: number): number[] | undefined {
    const planIds = current ?? [];
    const next = planIds.includes(id)
        ? planIds.filter((planId) => planId !== id)
        : [...planIds, id];
    return next.length ? next : undefined;
}

function hasAccessPlan(current: number[] | undefined, id: number): boolean {
    return current ? current.includes(id) : false;
}

const endpointFamilyMeta: Record<APIKeyEndpointFamily, { label: string; description: string; example: string }> = {
    'openai-compatible': {
        label: 'OpenAI-compatible',
        description: '/v1/chat/completions · /v1/responses',
        example: 'Authorization: Bearer sk-octopus-... -> /v1/chat/completions',
    },
    gemini: {
        label: 'Gemini',
        description: '/v1beta/models/*',
        example: 'x-goog-api-key: sk-octopus-... -> /v1beta/models/{model}:generateContent',
    },
    anthropic: {
        label: 'Anthropic / Claude',
        description: '/v1/messages',
        example: 'x-api-key: sk-octopus-... -> /v1/messages',
    },
};

function normalizeEndpointFamilies(value: APIKey['endpoint_families'] | undefined): APIKeyEndpointFamily[] {
    const items = Array.isArray(value)
        ? value
        : typeof value === 'string'
            ? value.split(',')
            : API_KEY_ENDPOINT_FAMILIES;
    if (items.length === 0) {
        return [...API_KEY_ENDPOINT_FAMILIES];
    }
    const selected = new Set(
        items
            .map((item) => {
                const normalized = String(item).trim().toLowerCase();
                return normalized === 'openai' ? 'openai-compatible' : normalized;
            })
            .filter((item): item is APIKeyEndpointFamily => (
                API_KEY_ENDPOINT_FAMILIES.includes(item as APIKeyEndpointFamily)
            ))
    );
    return API_KEY_ENDPOINT_FAMILIES.filter((family) => selected.has(family));
}

function getInitialEndpointFamilies(apiKey?: APIKey): APIKeyEndpointFamily[] {
    const candidates = [
        apiKey?.endpoint_families,
        apiKey?.endpoint_scopes,
        apiKey?.allowed_endpoint_families,
        apiKey?.endpoint_family_scopes,
    ];
    const configured = candidates.find((value) => (
        Array.isArray(value) || (typeof value === 'string' && value.trim().length > 0)
    ));
    return normalizeEndpointFamilies(configured);
}

function toggleEndpointFamily(current: APIKey['endpoint_families'] | undefined, family: APIKeyEndpointFamily): APIKeyEndpointFamily[] {
    const families = normalizeEndpointFamilies(current);
    const next = families.includes(family)
        ? families.filter((item) => item !== family)
        : [...families, family];
    return next.length > 0 ? next : families;
}

function getBrowserBaseUrl(): string {
    if (typeof window === 'undefined') return 'https://your-octopus.example';
    return window.location.origin;
}

type APIKeyAccessPlan = NonNullable<APIKey['access_plans']>[number];

function getAccessPlanLabel(plan: APIKeyAccessPlan): string {
    return plan.display_name?.trim() || plan.slug || `#${plan.id}`;
}

function getInitialGuidePlanID(apiKey: APIKey | null): number | undefined {
    const plans = apiKey?.access_plans ?? [];
    if (plans.length === 0) return undefined;
    const defaultPlan = plans.find((plan) => plan.id === apiKey?.default_access_plan_id);
    if (defaultPlan) return defaultPlan.id;
    const enabledPlan = plans.find((plan) => plan.enabled);
    return enabledPlan?.id ?? plans[0]?.id;
}

function buildUsageExample(family: APIKeyEndpointFamily, baseUrl: string, apiKey: string, accessPlanSlug?: string): string {
    const planHeader = accessPlanSlug ? `  -H "X-Octopus-Plan: ${accessPlanSlug}" \\\n` : '';
    switch (family) {
        case 'gemini':
            return `curl "${baseUrl}/v1beta/models/gemini-2.5-flash:generateContent" \\
  -H "x-goog-api-key: ${apiKey}" \\
${planHeader}  -H "Content-Type: application/json" \\
  -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'`;
        case 'anthropic':
            return `curl "${baseUrl}/v1/messages" \\
  -H "x-api-key: ${apiKey}" \\
${planHeader}  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"claude-sonnet-4.5","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`;
        case 'openai-compatible':
        default:
            return `curl "${baseUrl}/v1/chat/completions" \\
  -H "Authorization: Bearer ${apiKey}" \\
${planHeader}  -H "Content-Type: application/json" \\
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}'`;
    }
}

function getUsageDescriptionKey(family: APIKeyEndpointFamily): string {
    switch (family) {
        case 'gemini':
            return 'apiKey.guide.geminiDescription';
        case 'anthropic':
            return 'apiKey.guide.anthropicDescription';
        case 'openai-compatible':
        default:
            return 'apiKey.guide.openaiDescription';
    }
}

interface APIKeyFormProps {
    apiKey?: APIKey;
    isPending: boolean;
    submitLabel: string;
    onSubmit: (data: Omit<APIKey, 'id' | 'api_key'>) => void;
    onClose: () => void;
}

function APIKeyForm({ apiKey, isPending, submitLabel, onSubmit, onClose }: APIKeyFormProps) {
    const t = useTranslations('setting');
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');
    const currentUserID = useAuthStore((state) => state.user?.id);
    const { data: modelChannels } = useModelChannelList({ enabled: isAdmin });
    const { data: users = [] } = useUserList({ enabled: isAdmin });
    const { data: accessPlans = [] } = useAccessPlanList({ enabled: isAdmin });

    const [form, setForm] = useState<Omit<APIKey, 'id' | 'api_key'>>(() => ({
        name: apiKey?.name ?? '',
        user_id: apiKey?.user_id,
        enabled: apiKey?.enabled ?? true,
        expire_at: apiKey?.expire_at,
        max_cost: apiKey?.max_cost,
        supported_models: apiKey?.supported_models,
        access_plan_ids: apiKey?.access_plan_ids,
        endpoint_families: getInitialEndpointFamilies(apiKey),
        default_access_plan_id: apiKey?.default_access_plan_id,
    }));
    const [maxCostInput, setMaxCostInput] = useState(() =>
        apiKey?.max_cost != null ? String(apiKey.max_cost) : ''
    );
    const [expireTime, setExpireTime] = useState(() => {
        if (apiKey?.expire_at) {
            const d = new Date(apiKey.expire_at * 1000);
            if (!isNaN(d.getTime())) {
                return `${d.getUTCHours().toString().padStart(2, '0')}:${d.getUTCMinutes().toString().padStart(2, '0')}`;
            }
        }
        return '00:00';
    });
    const [expireOpen, setExpireOpen] = useState(false);

    const [modelSearch, setModelSearch] = useState('');

    // 仅暴露「当前有存活渠道在服务」的模型（渠道模型即对外可见模型名），过滤掉所有
    // 渠道都被禁用/删除、实际无渠道可路由的幽灵模型。
    const availableModels = useMemo(() => {
        const served = new Set<string>();
        for (const mc of modelChannels ?? []) {
            if (mc.enabled && mc.name) served.add(mc.name);
        }
        return Array.from(served).sort((a, b) => a.localeCompare(b));
    }, [modelChannels]);

    // 搜索只过滤展示的复选框；全选/清空仍作用于完整列表，保持与既有标签语义一致。
    const filteredModels = useMemo(() => {
        const term = modelSearch.trim().toLowerCase();
        if (!term) return availableModels;
        return availableModels.filter((m) => m.toLowerCase().includes(term));
    }, [availableModels, modelSearch]);

    const expireDate = parseExpireDate(form.expire_at);
    const neverExpire = !form.expire_at;
    const isUnlimitedCost = maxCostInput.trim() === '';
    const availableAccessPlans = useMemo(() => {
        const ownerUser = users.find((user) => user.id === (form.user_id ?? currentUserID));
        const ownerPlanIDs = ownerUser?.access_plan_ids ?? ownerUser?.access_plans?.map((plan) => plan.id) ?? [];
        return ownerPlanIDs.length > 0
            ? accessPlans.filter((plan) => ownerPlanIDs.includes(plan.id))
            : accessPlans;
    }, [accessPlans, currentUserID, form.user_id, users]);
    const selectedAccessPlans = useMemo(() => (
        availableAccessPlans.filter((plan) => (form.access_plan_ids ?? []).includes(plan.id))
    ), [availableAccessPlans, form.access_plan_ids]);
    const selectedEndpointFamilies = useMemo(
        () => normalizeEndpointFamilies(form.endpoint_families),
        [form.endpoint_families]
    );

    const expireLabel = neverExpire
        ? t('apiKey.form.neverExpire')
        : expireDate
            ? expireDate.toLocaleDateString()
            : t('apiKey.form.selectDate');

    const updateForm = useCallback((updater: Partial<Omit<APIKey, 'id' | 'api_key'>>) => {
        setForm((prev) => ({ ...prev, ...updater }));
    }, []);

    const handleSelectDate = useCallback((d: Date | undefined) => {
        if (d) {
            updateForm({ expire_at: toExpireAt(d, expireTime) });
            setExpireOpen(false);
        } else {
            updateForm({ expire_at: undefined });
        }
    }, [updateForm, expireTime]);

    const handleTimeBlur = useCallback(() => {
        if (!expireDate) return;
        const normalized = normalizeHHmm(expireTime);
        setExpireTime(normalized);
        updateForm({ expire_at: toExpireAt(expireDate, normalized) });
    }, [expireDate, expireTime, updateForm]);

    const handleToggleNeverExpire = useCallback(() => {
        if (neverExpire) {
            updateForm({ expire_at: toExpireAt(new Date(), expireTime) });
        } else {
            updateForm({ expire_at: undefined });
            setExpireOpen(false);
        }
    }, [neverExpire, expireTime, updateForm]);

    const handleMaxCostChange = useCallback((val: string) => {
        const normalized = normalizeMoneyInput(val);
        setMaxCostInput(normalized);
        const num = parseFloat(normalized);
        updateForm({ max_cost: Number.isFinite(num) ? num : undefined });
    }, [updateForm]);

    const handleClearMaxCost = useCallback(() => {
        setMaxCostInput('');
        updateForm({ max_cost: undefined });
    }, [updateForm]);

    const handleSubmit = useCallback((e: React.FormEvent) => {
        e.preventDefault();
        if (!form.name.trim()) return;
        const payload = { ...form };
        payload.endpoint_families = normalizeEndpointFamilies(payload.endpoint_families);
        if (isAdmin && payload.access_plan_ids?.length && !payload.default_access_plan_id) {
            payload.default_access_plan_id = payload.access_plan_ids[0];
        }
        if (isAdmin && !payload.access_plan_ids?.length) {
            const defaultPlan = availableAccessPlans.find((plan) => plan.enabled && plan.is_default)
                ?? availableAccessPlans.find((plan) => plan.enabled);
            if (defaultPlan) {
                payload.access_plan_ids = [defaultPlan.id];
                payload.default_access_plan_id = defaultPlan.id;
            }
        }
        onSubmit(payload);
    }, [availableAccessPlans, form, isAdmin, onSubmit]);

    return (
        <form onSubmit={handleSubmit} className="grid gap-2">
            <label className="grid gap-1 text-xs text-muted-foreground">
                {t('apiKey.form.name')}
                <Input
                    type="text"
                    value={form.name}
                    onChange={(e) => updateForm({ name: e.target.value })}
                    className="h-9 text-sm rounded-xl"
                    disabled={isPending}
                    required
                />
            </label>

            {isAdmin && (
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('apiKey.form.owner')}
                    <select
                        value={form.user_id ?? currentUserID ?? ''}
                        onChange={(e) => {
                            const nextUserID = Number(e.target.value) || undefined;
                            const nextOwner = users.find((user) => user.id === nextUserID);
                            const nextOwnerPlanIDs = nextOwner?.access_plan_ids ?? nextOwner?.access_plans?.map((plan) => plan.id) ?? [];
                            const nextPlanOptions = nextOwnerPlanIDs.length > 0
                                ? accessPlans.filter((plan) => nextOwnerPlanIDs.includes(plan.id))
                                : accessPlans;
                            const nextDefault = nextPlanOptions.find((plan) => plan.enabled && plan.is_default)
                                ?? nextPlanOptions.find((plan) => plan.enabled);
                            updateForm({
                                user_id: nextUserID,
                                access_plan_ids: nextDefault ? [nextDefault.id] : undefined,
                                default_access_plan_id: nextDefault?.id,
                            });
                        }}
                        className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                        disabled={isPending}
                    >
                        {currentUserID && users.length === 0 && (
                            <option value={currentUserID}>{t('apiKey.form.currentAdmin')}</option>
                        )}
                        {users.map((user) => (
                            <option key={user.id} value={user.id}>
                                {user.username} · {user.role}
                            </option>
                        ))}
                    </select>
                </label>
            )}

            <div className="grid gap-1 text-xs text-muted-foreground">
                {t('apiKey.form.maxCost')}
                <div className="flex items-center gap-2">
                    <div className="relative flex-1">
                        <span className="absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">$</span>
                        <Input
                            type="text"
                            inputMode="decimal"
                            placeholder={t('apiKey.form.maxCostPlaceholder')}
                            value={maxCostInput}
                            onChange={(e) => handleMaxCostChange(e.target.value)}
                            className="h-9 text-sm rounded-xl pl-7"
                            disabled={isPending}
                        />
                    </div>
                    <button
                        type="button"
                        onClick={handleClearMaxCost}
                        disabled={isPending}
                        aria-pressed={isUnlimitedCost}
                        className={cn(
                            'h-9 px-3 rounded-xl border text-sm transition-colors shrink-0',
                            isUnlimitedCost
                                ? 'bg-primary text-primary-foreground border-primary/30'
                                : 'border-border bg-muted/20 text-foreground hover:bg-muted/30',
                            isPending && 'opacity-50 cursor-not-allowed'
                        )}
                    >
                        {t('apiKey.form.unlimited')}
                    </button>
                </div>
            </div>

            <div className="grid gap-1 text-xs text-muted-foreground">
                {t('apiKey.form.expireAt')}
                <div className="flex items-center gap-2 relative">
                    <Popover
                        open={expireOpen && !neverExpire}
                        onOpenChange={setExpireOpen}
                    >
                        <PopoverTrigger asChild>
                            <button
                                type="button"
                                disabled={isPending || neverExpire}
                                className="h-9 flex-1 flex items-center justify-between gap-2 rounded-xl border border-border bg-muted/20 px-3 text-sm text-foreground transition-colors hover:bg-muted/30 disabled:opacity-50"
                            >
                                <span className="truncate">{expireLabel}</span>
                                <CalendarDays className="size-4 text-muted-foreground" />
                            </button>
                        </PopoverTrigger>
                        <PopoverContent
                            align="start"
                            side="bottom"
                            sideOffset={8}
                            className="w-fit rounded-2xl border border-border/60 shadow-xl overflow-hidden bg-card p-0"
                        >
                            <Calendar
                                mode="single"
                                selected={expireDate}
                                onSelect={handleSelectDate}
                                disabled={isPending}
                                classNames={{ today: '' }}
                            />
                        </PopoverContent>
                    </Popover>

                    <Input
                        type="text"
                        value={expireTime}
                        onChange={(e) => setExpireTime(e.target.value.replace(/[^\d:]/g, '').slice(0, 5))}
                        onBlur={handleTimeBlur}
                        className="h-9 w-[92px] text-sm rounded-xl"
                        disabled={isPending || neverExpire || !expireDate}
                        inputMode="numeric"
                        placeholder="HH:mm"
                    />

                    <button
                        type="button"
                        onClick={handleToggleNeverExpire}
                        disabled={isPending}
                        aria-pressed={neverExpire}
                        className={cn(
                            'h-9 px-3 rounded-xl border text-sm transition-colors whitespace-nowrap shrink-0',
                            neverExpire
                                ? 'bg-primary text-primary-foreground border-primary/30'
                                : 'border-border bg-muted/20 text-foreground hover:bg-muted/30',
                            isPending && 'opacity-50 cursor-not-allowed'
                        )}
                    >
                        {t('apiKey.form.neverExpire')}
                    </button>
                </div>
            </div>

            <div className="grid gap-1">
                <div className="flex items-center justify-between gap-2">
                    <div className="text-xs text-muted-foreground">{t('apiKey.form.supportedModels')}</div>
                    {availableModels.length > 0 && (
                        <div className="flex gap-1">
                            <button
                                type="button"
                                disabled={isPending}
                                onClick={() => updateForm({ supported_models: availableModels.join(',') })}
                                className="h-7 rounded-lg border border-border bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-50"
                            >
                                {t('apiKey.form.selectAllModels')}
                            </button>
                            <button
                                type="button"
                                disabled={isPending}
                                onClick={() => updateForm({ supported_models: undefined })}
                                className="h-7 rounded-lg border border-border bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-50"
                            >
                                {t('apiKey.form.unlimitedModels')}
                            </button>
                        </div>
                    )}
                </div>
                {availableModels.length > 0 && (
                    <div className="relative">
                        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                        <Input
                            type="text"
                            value={modelSearch}
                            onChange={(e) => setModelSearch(e.target.value)}
                            placeholder={t('apiKey.form.searchModels')}
                            disabled={isPending}
                            className="h-8 rounded-xl pl-8 text-xs"
                        />
                    </div>
                )}
                <div className="max-h-40 overflow-auto rounded-xl p-2">
                    {availableModels.length === 0 ? (
                        <div className="text-xs text-muted-foreground py-2 text-center">
                            {t('apiKey.form.noModels')}
                        </div>
                    ) : filteredModels.length === 0 ? (
                        <div className="text-xs text-muted-foreground py-2 text-center">
                            {t('apiKey.form.noMatchedModels')}
                        </div>
                    ) : (
                        <div className="flex flex-wrap gap-2">
                            {filteredModels.map((m) => {
                                const checked = hasModel(form.supported_models, m);
                                return (
                                    <button
                                        key={m}
                                        type="button"
                                        disabled={isPending}
                                        onClick={() => updateForm({ supported_models: toggleModel(form.supported_models, m) })}
                                        className="max-w-full text-left disabled:opacity-50"
                                    >
                                        <Badge
                                            variant={checked ? 'default' : 'outline'}
                                            className={cn(
                                                'max-w-[12rem] cursor-pointer select-none',
                                                !checked && 'bg-background/40 hover:bg-background/70'
                                            )}
                                        >
                                            <SafeText value={m} />
                                        </Badge>
                                    </button>
                                );
                            })}
                        </div>
                    )}
                </div>
                <div className="text-[11px] text-muted-foreground/80">{t('apiKey.form.modelsHint')}</div>
            </div>

            <div className="grid gap-1">
                <div className="text-xs text-muted-foreground">{t('apiKey.form.endpointFamilies')}</div>
                <div className="grid gap-2 rounded-xl p-2">
                    {API_KEY_ENDPOINT_FAMILIES.map((family) => {
                        const checked = selectedEndpointFamilies.includes(family);
                        const meta = endpointFamilyMeta[family];
                        return (
                            <label
                                key={family}
                                className={cn(
                                    'flex items-center gap-2 rounded-xl border px-3 py-2 text-sm transition-colors',
                                    checked
                                        ? 'border-primary/30 bg-primary/10 text-foreground'
                                        : 'border-border bg-muted/20 text-muted-foreground'
                                )}
                            >
                                <input
                                    type="checkbox"
                                    checked={checked}
                                    disabled={isPending}
                                    onChange={() => updateForm({ endpoint_families: toggleEndpointFamily(form.endpoint_families, family) })}
                                    className="size-4 accent-primary"
                                />
                                <SafeText value={meta.label} className="flex-1" />
                                <span className="hidden shrink-0 text-[11px] text-muted-foreground sm:inline">
                                    {meta.description}
                                </span>
                            </label>
                        );
                    })}
                </div>
                <div className="text-[11px] text-muted-foreground/80">
                    {t('apiKey.form.endpointFamiliesHint')}
                </div>
            </div>

            {isAdmin && (
                <div className="grid gap-2">
                    <div className="flex items-center justify-between gap-2">
                        <div className="text-xs text-muted-foreground">{t('apiKey.form.accessPlans')}</div>
                        {availableAccessPlans.length > 0 && (
                            <div className="flex gap-1">
                                <button
                                    type="button"
                                    disabled={isPending}
                                    onClick={() => {
                                        const enabledPlans = availableAccessPlans.filter((plan) => plan.enabled);
                                        updateForm({
                                            access_plan_ids: enabledPlans.map((plan) => plan.id),
                                            default_access_plan_id: form.default_access_plan_id && enabledPlans.some((plan) => plan.id === form.default_access_plan_id)
                                                ? form.default_access_plan_id
                                                : enabledPlans[0]?.id,
                                        });
                                    }}
                                    className="h-7 rounded-lg border border-border bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-50"
                                >
                                    {t('apiKey.form.selectAllPlans')}
                                </button>
                                <button
                                    type="button"
                                    disabled={isPending}
                                    onClick={() => {
                                        const defaultPlan = availableAccessPlans.find((plan) => plan.enabled && plan.is_default)
                                            ?? availableAccessPlans.find((plan) => plan.enabled);
                                        updateForm({
                                            access_plan_ids: defaultPlan ? [defaultPlan.id] : undefined,
                                            default_access_plan_id: defaultPlan?.id,
                                        });
                                    }}
                                    className="h-7 rounded-lg border border-border bg-background px-2 text-xs text-foreground hover:bg-muted disabled:opacity-50"
                                >
                                    {t('apiKey.form.defaultOnly')}
                                </button>
                            </div>
                        )}
                    </div>
                    <div className="max-h-36 overflow-auto rounded-xl p-2">
                        {availableAccessPlans.length === 0 ? (
                            <div className="py-2 text-center text-xs text-muted-foreground">
                                {t('apiKey.form.noAccessPlans')}
                            </div>
                        ) : (
                            <div className="flex flex-wrap gap-2">
                                {availableAccessPlans.map((plan) => {
                                    const checked = hasAccessPlan(form.access_plan_ids, plan.id);
                                    return (
                                        <button
                                            key={plan.id}
                                            type="button"
                                            disabled={isPending || !plan.enabled}
                                            onClick={() => {
                                                const next = toggleAccessPlan(form.access_plan_ids, plan.id);
                                                const nextDefault = next?.includes(form.default_access_plan_id ?? 0)
                                                    ? form.default_access_plan_id
                                                    : next?.[0];
                                                updateForm({
                                                    access_plan_ids: next,
                                                    default_access_plan_id: nextDefault,
                                                });
                                            }}
                                            className="max-w-full text-left disabled:opacity-50"
                                        >
                                            <Badge
                                                variant={checked ? 'default' : 'outline'}
                                                className={cn(
                                                    'max-w-[12rem] cursor-pointer select-none',
                                                    !checked && 'bg-background/40 hover:bg-background/70'
                                                )}
                                            >
                                                <SafeText value={plan.display_name || plan.slug} />
                                            </Badge>
                                        </button>
                                    );
                                })}
                            </div>
                        )}
                    </div>
                    {selectedAccessPlans.length > 0 && (
                        <label className="grid gap-1 text-xs text-muted-foreground">
                            {t('apiKey.form.defaultAccessPlan')}
                            <select
                                value={form.default_access_plan_id ?? selectedAccessPlans[0]?.id ?? ''}
                                onChange={(e) => updateForm({ default_access_plan_id: Number(e.target.value) || undefined })}
                                className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                disabled={isPending}
                            >
                                {selectedAccessPlans.map((plan) => (
                                    <option key={plan.id} value={plan.id}>
                                        {plan.display_name || plan.slug}
                                    </option>
                                ))}
                            </select>
                        </label>
                    )}
                    <div className="text-[11px] text-muted-foreground/80">{t('apiKey.form.accessPlansHint')}</div>
                </div>
            )}

            <div className="flex items-center justify-between pt-1">
                <span className="text-xs text-muted-foreground">{t('apiKey.form.enabled')}</span>
                <Switch
                    checked={form.enabled ?? true}
                    onCheckedChange={(checked) => updateForm({ enabled: checked })}
                    disabled={isPending}
                />
            </div>

            <div className="flex gap-2 pt-2 mt-3">
                <button
                    type="button"
                    onClick={onClose}
                    disabled={isPending}
                    className="flex-1 h-9 flex items-center justify-center gap-1.5 rounded-xl bg-muted text-muted-foreground text-sm font-medium transition-all hover:bg-muted/80 active:scale-[0.98] disabled:opacity-50"
                >
                    <X className="size-4" />
                    {t('apiKey.form.cancel')}
                </button>
                <button
                    type="submit"
                    disabled={isPending || !form.name.trim()}
                    className="flex-1 h-9 flex items-center justify-center gap-1.5 rounded-xl bg-primary text-primary-foreground text-sm font-medium transition-all hover:bg-primary/90 active:scale-[0.98] disabled:opacity-50"
                >
                    {isPending ? <Loader className="size-4 animate-spin" /> : <Check className="size-4" />}
                    {submitLabel}
                </button>
            </div>
        </form>
    );
}

function APIKeyFormOverlay({
    layoutId,
    apiKey,
    isPending,
    submitLabel,
    onSubmit,
    onClose,
}: {
    layoutId: string;
    apiKey?: APIKey;
    isPending: boolean;
    submitLabel: string;
    onSubmit: (data: Omit<APIKey, 'id' | 'api_key'>) => void;
    onClose: () => void;
}) {
    return (
        <motion.div
            layoutId={layoutId}
            className="fixed left-1/2 top-1/2 z-[60] w-[min(460px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-border bg-card p-5 shadow-2xl max-h-[calc(100dvh-2rem)] overflow-auto"
            transition={{ type: 'spring', stiffness: 400, damping: 30 }}
        >
            <APIKeyForm
                apiKey={apiKey}
                isPending={isPending}
                submitLabel={submitLabel}
                onSubmit={onSubmit}
                onClose={onClose}
            />
        </motion.div>
    );
}

function APIKeyOverlayBackdrop({ label, onClick }: { label: string; onClick: () => void }) {
    return (
        <motion.button
            type="button"
            aria-label={label}
            className="fixed inset-0 z-50 cursor-default bg-background/55 backdrop-blur-[2px]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.16 }}
            onClick={onClick}
        />
    );
}

function APIKeyUsageGuideDialog({
    apiKey,
    open,
    onOpenChange,
}: {
    apiKey: APIKey | null;
    open: boolean;
    onOpenChange: (open: boolean) => void;
}) {
    const t = useTranslations('setting');
    const baseUrl = getBrowserBaseUrl();
    const endpointFamilies = apiKey ? normalizeEndpointFamilies(apiKey.endpoint_families) : [];
    const guidePlans = apiKey?.access_plans ?? [];
    const [guidePlanID, setGuidePlanID] = useState<number | undefined>(() => getInitialGuidePlanID(apiKey));
    const selectedGuidePlan = guidePlans.find((plan) => plan.id === guidePlanID);
    const selectedGuidePlanSlug = selectedGuidePlan?.slug;

    useEffect(() => {
        setGuidePlanID(getInitialGuidePlanID(apiKey));
    }, [apiKey]);

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="max-h-[calc(100dvh-1rem)] overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-3xl">
                <DialogHeader className="border-b border-border/60 px-5 py-4 text-left">
                    <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground">
                        <BookOpen className="size-4" />
                        {t('apiKey.guide.button')}
                    </div>
                    <DialogTitle className="text-xl">{t('apiKey.guide.title')}</DialogTitle>
                    <DialogDescription className="min-w-0">
                        <SafeText
                            mode="wrap"
                            value={apiKey?.name ? `${apiKey.name} · ${t('apiKey.guide.description')}` : t('apiKey.guide.description')}
                        />
                    </DialogDescription>
                </DialogHeader>

                <div className="max-h-[calc(100dvh-10rem)] space-y-3 overflow-y-auto px-4 py-4 sm:px-5">
                    <div className="grid gap-2 rounded-2xl border border-border/60 bg-muted/20 p-3 text-sm">
                        <div className="flex items-center justify-between gap-3">
                            <div className="min-w-0">
                                <div className="text-xs text-muted-foreground">{t('apiKey.guide.baseUrl')}</div>
                                <code>
                                    <MonoSafeText value={baseUrl} className="block text-foreground" />
                                </code>
                            </div>
                            <CopyIconButton
                                text={baseUrl}
                                className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-background text-muted-foreground transition-colors hover:text-foreground"
                                copyIconClassName="size-4"
                                checkIconClassName="size-4"
                            />
                        </div>
                        <div className="flex flex-wrap gap-1.5">
                            <span className="text-[11px] text-muted-foreground">{t('apiKey.guide.allowedEndpoints')}</span>
                            {endpointFamilies.map((family) => (
                                <Badge key={family} variant="outline" className="text-[11px]">
                                    {endpointFamilyMeta[family].label}
                                </Badge>
                            ))}
                        </div>
                        {guidePlans.length > 0 && (
                            <div className="grid gap-2 border-t border-border/60 pt-2">
                                <div className="flex flex-wrap items-center justify-between gap-2">
                                    <span className="text-[11px] text-muted-foreground">{t('apiKey.guide.accessPlan')}</span>
                                    <select
                                        value={guidePlanID ?? ''}
                                        onChange={(event) => setGuidePlanID(Number(event.target.value) || undefined)}
                                        className="h-8 max-w-full rounded-xl border border-input bg-background px-2 text-xs text-foreground"
                                    >
                                        {guidePlans.map((plan) => (
                                            <option key={plan.id} value={plan.id}>
                                                {getAccessPlanLabel(plan)}
                                            </option>
                                        ))}
                                    </select>
                                </div>
                                <div className="grid gap-1 rounded-xl bg-background/60 px-2 py-2 text-xs">
                                    <span className="text-muted-foreground">{t('apiKey.guide.requestHeader')}</span>
                                    <code>
                                        <MonoSafeText
                                            mode="wrap"
                                            value={selectedGuidePlanSlug ? `X-Octopus-Plan: ${selectedGuidePlanSlug}` : t('apiKey.guide.defaultPlan')}
                                            className="text-foreground break-all"
                                        />
                                    </code>
                                </div>
                                <p className="text-[11px] leading-5 text-muted-foreground">{t('apiKey.guide.planHint')}</p>
                            </div>
                        )}
                    </div>

                    {apiKey && endpointFamilies.length > 0 ? endpointFamilies.map((family) => {
                        const example = buildUsageExample(family, baseUrl, apiKey.api_key, selectedGuidePlanSlug);
                        return (
                            <section key={family} className="overflow-hidden rounded-2xl border border-border/60">
                                <div className="flex items-start justify-between gap-3 bg-muted/25 px-3 py-2">
                                    <div className="min-w-0">
                                        <div className="flex items-center gap-2 text-sm font-medium text-foreground">
                                            <Terminal className="size-4 text-muted-foreground" />
                                            {endpointFamilyMeta[family].label}
                                        </div>
                                        <p className="mt-1 text-xs text-muted-foreground">
                                            {t(getUsageDescriptionKey(family))}
                                        </p>
                                    </div>
                                    <CopyIconButton
                                        text={example}
                                        className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-background text-muted-foreground transition-colors hover:text-foreground"
                                        copyIconClassName="size-4"
                                        checkIconClassName="size-4"
                                    />
                                </div>
                                <pre className="overflow-x-auto bg-zinc-950 p-3 text-xs leading-5 text-zinc-100"><code>{example}</code></pre>
                            </section>
                        );
                    }) : (
                        <div className="rounded-2xl border border-border/60 bg-muted/20 p-4 text-sm text-muted-foreground">
                            {t('apiKey.guide.empty')}
                        </div>
                    )}
                </div>
            </DialogContent>
        </Dialog>
    );
}

function APIKeyStatsCard({
    layoutId,
    apiKey,
    onClose,
}: {
    layoutId: string;
    apiKey: APIKey;
    onClose: () => void;
}) {
    const t = useTranslations('setting');
    const { data: statsList = [] } = useStatsAPIKey();
    const stats = useMemo(() => statsList.find((s) => s.api_key_id === apiKey.id), [statsList, apiKey.id]);

    return (
        <motion.div
            layoutId={layoutId}
            className="fixed left-1/2 top-1/2 z-[60] w-[min(340px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex flex-col rounded-3xl border border-border bg-card p-5 shadow-2xl max-h-[calc(100dvh-2rem)] overflow-auto"
            transition={{ type: 'spring', stiffness: 400, damping: 30 }}
        >
            <div className="flex items-center justify-between gap-2 mb-3">
                <h3 className="text-sm font-semibold text-card-foreground line-clamp-1">
                    <SafeText value={apiKey.name} className="block" />
                </h3>
                <button
                    type="button"
                    onClick={onClose}
                    className="size-8 flex items-center justify-center rounded-lg bg-muted text-muted-foreground transition-colors hover:bg-muted/80"
                >
                    <X className="size-4" />
                </button>
            </div>

            {!stats ? (
                <div className="text-sm text-muted-foreground">{t('apiKey.stats.noData')}</div>
            ) : (
                <div className="grid grid-cols-2 gap-3 text-sm">
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.inputToken')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.input_token.formatted.value}
                            {stats.input_token.formatted.unit}
                        </div>
                    </div>
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.outputToken')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.output_token.formatted.value}
                            {stats.output_token.formatted.unit}
                        </div>
                    </div>
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.inputCost')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.input_cost.formatted.value}
                            {stats.input_cost.formatted.unit}
                        </div>
                    </div>
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.outputCost')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.output_cost.formatted.value}
                            {stats.output_cost.formatted.unit}
                        </div>
                    </div>
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.requestSuccess')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.request_success.formatted.value}
                            {stats.request_success.formatted.unit}
                        </div>
                    </div>
                    <div className="rounded-lg bg-muted/40 p-3">
                        <div className="text-xs text-muted-foreground">{t('apiKey.stats.requestFailed')}</div>
                        <div className="font-medium tabular-nums">
                            {stats.request_failed.formatted.value}
                            {stats.request_failed.formatted.unit}
                        </div>
                    </div>
                </div>
            )}
        </motion.div>
    );
}

function APIKeyKeyItem({
    apiKey,
    statsLayoutId,
    editLayoutId,
    deleteLayoutId,
    onViewStats,
    onEdit,
    onUse,
    onDelete,
    isDeleting,
    showUsageLabel,
}: {
    apiKey: APIKey;
    statsLayoutId: string;
    editLayoutId: string;
    deleteLayoutId: string;
    onViewStats: () => void;
    onEdit: () => void;
    onUse: () => void;
    onDelete: () => void;
    isDeleting: boolean;
    showUsageLabel: boolean;
}) {
    const t = useTranslations('setting');
    const [confirmDelete, setConfirmDelete] = useState(false);
    const endpointFamilies = normalizeEndpointFamilies(apiKey.endpoint_families);
    const accessPlans = apiKey.access_plans ?? [];
    const hiddenAccessPlans = accessPlans.slice(2);

    return (
        <motion.div
            layout
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95, transition: { duration: 0.2 } }}
            transition={{ type: 'spring', stiffness: 500, damping: 30 }}
            className="group relative grid gap-2 rounded-xl bg-muted/50 p-3 overflow-hidden origin-top"
        >
            <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                <div className="min-w-0">
                    <div className="flex min-w-0 items-center gap-2">
                        <SafeText value={apiKey.name} mode="wrap" className="text-sm font-medium sm:truncate" />
                        {apiKey.user_name && (
                            <Badge variant="outline" className="hidden max-w-[10rem] shrink-0 text-[11px] md:inline-flex">
                                <SafeText value={apiKey.user_name} />
                            </Badge>
                        )}
                    </div>
                </div>
                <div className="flex flex-wrap items-center gap-1.5 sm:shrink-0 sm:justify-end">
                    <motion.button
                        type="button"
                        onClick={onUse}
                        className={cn(
                            "flex h-8 items-center justify-center gap-1.5 rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground active:scale-95",
                            showUsageLabel ? "px-2 text-xs" : "w-8"
                        )}
                        title={t('apiKey.guide.button')}
                    >
                        <BookOpen className="size-4" />
                        {showUsageLabel && <span>{t('apiKey.guide.button')}</span>}
                    </motion.button>
                    <motion.button
                        type="button"
                        layoutId={statsLayoutId}
                        onClick={onViewStats}
                        className="flex size-8 items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground active:scale-95"
                        title="Stats"
                    >
                        <Info className="size-4" />
                    </motion.button>
                    <motion.button
                        type="button"
                        layoutId={editLayoutId}
                        onClick={onEdit}
                        className="flex size-8 items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground active:scale-95"
                        title="Edit"
                    >
                        <Pencil className="size-4" />
                    </motion.button>
                    <CopyIconButton
                        text={apiKey.api_key}
                        className="flex size-8 items-center justify-center rounded-lg bg-primary/10 text-primary transition-all hover:bg-primary hover:text-primary-foreground active:scale-95"
                        copyIconClassName="size-4"
                        checkIconClassName="size-4"
                    />

                    {!confirmDelete && (
                        <motion.button
                            layoutId={deleteLayoutId}
                            onClick={() => setConfirmDelete(true)}
                            className="flex size-8 items-center justify-center rounded-lg bg-destructive/10 text-destructive transition-colors hover:bg-destructive hover:text-destructive-foreground"
                        >
                            <Trash2 className="size-4" />
                        </motion.button>
                    )}
                </div>
            </div>

            <div className="flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
                <span>{t('apiKey.list.plans')}</span>
                {accessPlans.length === 0 ? (
                    <Badge variant="outline" className="text-[11px]">
                        {t('apiKey.list.defaultPlan')}
                    </Badge>
                ) : (
                    <>
                        {accessPlans.slice(0, 2).map((plan) => (
                            <Badge
                                key={plan.id}
                                variant={apiKey.default_access_plan_id === plan.id ? 'default' : 'outline'}
                                className="max-w-[9rem] shrink-0 text-[11px]"
                            >
                                <SafeText value={plan.display_name || plan.slug} />
                            </Badge>
                        ))}
                        {hiddenAccessPlans.length > 0 && (
                            <Badge
                                variant="outline"
                                className="shrink-0 text-[11px]"
                                title={hiddenAccessPlans.map((plan) => plan.display_name || plan.slug).join(' / ')}
                            >
                                {t('apiKey.list.morePlans', { count: hiddenAccessPlans.length })}
                            </Badge>
                        )}
                    </>
                )}
                <span className="ml-1">{t('apiKey.list.endpoints')}</span>
                {endpointFamilies.map((family) => (
                    <Badge key={family} variant="outline" className="shrink-0 text-[11px]">
                        {endpointFamilyMeta[family].label}
                    </Badge>
                ))}
            </div>

            <AnimatePresence>
                {confirmDelete && (
                    <motion.div
                        layoutId={deleteLayoutId}
                        className="absolute inset-0 flex items-center justify-center gap-2 bg-destructive p-3 rounded-xl"
                        transition={{ type: 'spring', stiffness: 400, damping: 30 }}
                    >
                        <button
                            onClick={() => setConfirmDelete(false)}
                            className="flex size-8 items-center justify-center rounded-lg bg-destructive-foreground/20 text-destructive-foreground transition-all hover:bg-destructive-foreground/30 active:scale-95"
                        >
                            <X className="size-4" />
                        </button>
                        <button
                            onClick={onDelete}
                            disabled={isDeleting}
                            className="flex-1 h-8 flex items-center justify-center gap-1.5 rounded-lg bg-destructive-foreground text-destructive text-sm font-medium transition-all hover:bg-destructive-foreground/90 active:scale-[0.98] disabled:opacity-50"
                        >
                            <Trash2 className="size-3.5" />
                            {isDeleting ? '...' : t('apiKey.form.confirm')}
                        </button>
                    </motion.div>
                )}
            </AnimatePresence>
        </motion.div>
    );
}

function APIKeyPanelBase({
    idPrefix,
    containerClassName,
    listClassName,
    renderHeaderExtra,
}: {
    idPrefix: string;
    containerClassName: string;
    listClassName: string;
    renderHeaderExtra?: (ctx: {
        disabled: boolean;
        onCloseAllOverlays: () => void;
    }) => React.ReactNode;
}) {
    const t = useTranslations('setting');
    const { data: apiKeys, isLoading: apiKeysLoading, error: apiKeysError } = useAPIKeyList();
    const createAPIKey = useCreateAPIKey();
    const updateAPIKey = useUpdateAPIKey();
    const deleteAPIKey = useDeleteAPIKey();
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');

    const instanceId = useId();
    const addLayoutId = `add-btn-${idPrefix}-${instanceId}`;
    const statsPrefix = `${idPrefix}-stats-${instanceId}`;
    const editPrefix = `${idPrefix}-edit-${instanceId}`;
    const deletePrefix = `${idPrefix}-delete-`;

    const [isAdding, setIsAdding] = useState(false);
    const [viewingStats, setViewingStats] = useState<{ apiKey: APIKey; layoutId: string } | null>(null);
    const [editingKey, setEditingKey] = useState<{ apiKey: APIKey; layoutId: string } | null>(null);
    const [usingKey, setUsingKey] = useState<APIKey | null>(null);
    const [deletingId, setDeletingId] = useState<number | null>(null);

    const sortedApiKeys = useMemo(() => {
        if (!apiKeys) return [];
        return [...apiKeys].sort((a, b) => a.id - b.id);
    }, [apiKeys]);

    const handleDelete = useCallback((id: number) => {
        setDeletingId(id);
        deleteAPIKey.mutate(id, {
            onSuccess: () => {
                toast.success(t('apiKey.toast.deleteSuccess'));
            },
            onError: (error) => {
                const msg = (error as unknown as ApiError)?.message;
                toast.error(t('apiKey.toast.deleteError'), { description: msg });
            },
            onSettled: () => setDeletingId((cur) => (cur === id ? null : cur)),
        });
    }, [deleteAPIKey, t]);

    const closeAllOverlays = useCallback(() => {
        setIsAdding(false);
        setViewingStats(null);
        setEditingKey(null);
        setUsingKey(null);
    }, []);

    const disabledHeaderActions = createAPIKey.isPending || isAdding || !!viewingStats || !!editingKey || !!usingKey;

    const handleCreate = useCallback((data: Omit<APIKey, 'id' | 'api_key'>) => {
        createAPIKey.mutate(data, {
            onSuccess: () => {
                toast.success(t('apiKey.toast.createSuccess'));
                setIsAdding(false);
            },
            onError: (error) => {
                const msg = (error as unknown as ApiError)?.message;
                toast.error(t('apiKey.toast.createError'), { description: msg });
            },
        });
    }, [createAPIKey, t]);

    const handleUpdate = useCallback((apiKey: APIKey, data: Omit<APIKey, 'id' | 'api_key'>) => {
        updateAPIKey.mutate({ id: apiKey.id, ...data }, {
            onSuccess: () => {
                toast.success(t('apiKey.toast.updateSuccess'));
                setEditingKey(null);
            },
            onError: (error) => {
                const msg = (error as unknown as ApiError)?.message;
                toast.error(t('apiKey.toast.updateError'), { description: msg });
            },
        });
    }, [t, updateAPIKey]);

    return (
        <div className={containerClassName}>
            <div className="flex items-center justify-between gap-3">
                <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                    <KeyRound className="h-5 w-5" />
                    {t('apiKey.title')}
                </h2>
                <div className="flex items-center gap-2">
                    <motion.button
                        layoutId={addLayoutId}
                        type="button"
                        onClick={() => setIsAdding(true)}
                        disabled={disabledHeaderActions}
                        className="h-9 w-9 flex items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted disabled:opacity-50"
                        title={t('apiKey.add')}
                    >
                        <Plus className="size-4" />
                    </motion.button>
                    {renderHeaderExtra?.({ disabled: disabledHeaderActions, onCloseAllOverlays: closeAllOverlays })}
                </div>
            </div>

            <AnimatePresence>
                {isAdding && (
                    <>
                        <APIKeyOverlayBackdrop label={t('apiKey.form.cancel')} onClick={() => setIsAdding(false)} />
                        <APIKeyFormOverlay
                            layoutId={addLayoutId}
                            isPending={createAPIKey.isPending}
                            submitLabel={t('apiKey.form.create')}
                            onSubmit={handleCreate}
                            onClose={() => setIsAdding(false)}
                        />
                    </>
                )}
            </AnimatePresence>

            <AnimatePresence>
                {viewingStats && (
                    <>
                        <APIKeyOverlayBackdrop label={t('apiKey.form.cancel')} onClick={() => setViewingStats(null)} />
                        <APIKeyStatsCard
                            layoutId={viewingStats.layoutId}
                            apiKey={viewingStats.apiKey}
                            onClose={() => setViewingStats(null)}
                        />
                    </>
                )}
            </AnimatePresence>

            <AnimatePresence>
                {editingKey && (
                    <>
                        <APIKeyOverlayBackdrop label={t('apiKey.form.cancel')} onClick={() => setEditingKey(null)} />
                        <APIKeyFormOverlay
                            layoutId={editingKey.layoutId}
                            apiKey={editingKey.apiKey}
                            isPending={updateAPIKey.isPending}
                            submitLabel={t('apiKey.form.save')}
                            onSubmit={(data) => handleUpdate(editingKey.apiKey, data)}
                            onClose={() => setEditingKey(null)}
                        />
                    </>
                )}
            </AnimatePresence>

            <APIKeyUsageGuideDialog
                apiKey={usingKey}
                open={!!usingKey}
                onOpenChange={(open) => {
                    if (!open) setUsingKey(null);
                }}
            />

            <div className={listClassName}>
                {apiKeysLoading ? (
                    <div className="h-full flex items-center justify-center text-sm text-muted-foreground">
                        <Loader className="size-4 animate-spin" />
                    </div>
                ) : apiKeysError ? (
                    <div className="h-full flex items-center justify-center text-sm text-destructive">
                        {t('apiKey.loadFailed')}
                    </div>
                ) : apiKeys?.length === 0 ? (
                    <div className="h-full flex items-center justify-center text-sm text-muted-foreground">
                        {t('apiKey.empty')}
                    </div>
                ) : (
                    <AnimatePresence>
                        {sortedApiKeys.map((apiKey) => {
                            const statsLayoutId = `${statsPrefix}-${apiKey.id}`;
                            const editLayoutId = `${editPrefix}-${apiKey.id}`;
                            const deleteLayoutId = `${deletePrefix}${apiKey.id}`;
                            return (
                                <APIKeyKeyItem
                                    key={apiKey.id}
                                    apiKey={apiKey}
                                    statsLayoutId={statsLayoutId}
                                    editLayoutId={editLayoutId}
                                    deleteLayoutId={deleteLayoutId}
                                    onViewStats={() => {
                                        closeAllOverlays();
                                        setViewingStats({ apiKey, layoutId: statsLayoutId });
                                    }}
                                    onEdit={() => {
                                        closeAllOverlays();
                                        setEditingKey({ apiKey, layoutId: editLayoutId });
                                    }}
                                    onUse={() => {
                                        closeAllOverlays();
                                        setUsingKey(apiKey);
                                    }}
                                    onDelete={() => handleDelete(apiKey.id)}
                                    isDeleting={deleteAPIKey.isPending && deletingId === apiKey.id}
                                    showUsageLabel={!isAdmin}
                                />
                            );
                        })}
                    </AnimatePresence>
                )}
            </div>
        </div>
    );
}

function APIKeyDialogPanel() {
    const { setIsOpen } = useMorphingDialog();
    return (
        <APIKeyPanelBase
            idPrefix="apikey-dialog"
            containerClassName="relative w-[calc(100vw-1rem)] max-w-full space-y-5 rounded-3xl border border-border bg-card p-4 md:max-w-xl md:p-6"
            listClassName="space-y-2 max-h-[calc(100dvh-11rem)] overflow-y-auto"
            renderHeaderExtra={() => (
                <button
                    type="button"
                    onClick={() => setIsOpen(false)}
                    className="h-9 w-9 flex items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted"
                    title="Close"
                >
                    <X className="size-4" />
                </button>
            )}
        />
    );
}

export function SettingAPIKey({ fullPage = false }: { fullPage?: boolean }) {
    return (
        <APIKeyPanelBase
            idPrefix="apikey"
            containerClassName={cn(
                "relative space-y-5 rounded-3xl border border-border bg-card p-4 md:p-6",
                fullPage && "h-full min-h-0 flex flex-col"
            )}
            listClassName={cn("space-y-2 overflow-y-auto", fullPage ? "flex-1 min-h-0" : "h-36")}
            renderHeaderExtra={() => (
                <MorphingDialog>
                    <MorphingDialogTrigger className="h-9 w-9 flex items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted">
                        <Maximize2 className="size-4" />
                    </MorphingDialogTrigger>
                    <MorphingDialogContainer>
                        <MorphingDialogContent className="relative">
                            <APIKeyDialogPanel />
                        </MorphingDialogContent>
                    </MorphingDialogContainer>
                </MorphingDialog>
            )}
        />
    );
}
