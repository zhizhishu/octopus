import { AutoGroupType, ChannelType, KeySelectStrategy, defaultModelTestEndpointForChannel, type Channel, type PromptOverrideMode, useFetchModel, useTestChannelConfig } from '@/api/endpoints/channel';
import { useFingerprintProfileList } from '@/api/endpoints/fingerprint-profile';
import type { ModelTestResult } from '@/api/endpoints/model';
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { toast } from '@/components/common/Toast';
import { useTranslations } from 'next-intl';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Loader2, Play, RefreshCw, X, Plus } from 'lucide-react';
import {
    Accordion,
    AccordionContent,
    AccordionItem,
    AccordionTrigger,
} from "@/components/ui/accordion";
import { cn } from '@/lib/utils';
import { cleanOneMillionModelName, expandOneMillionModelAliases, isStreamRequiredModel } from '@/lib/model-aliases';

type HeaderPresetKey = 'codex' | 'claude' | 'openaiPython';

const HEADER_PRESETS: Record<HeaderPresetKey, Channel['custom_header']> = {
    codex: [
        { header_key: 'Connection', header_value: 'Keep-Alive' },
        { header_key: 'Content-Type', header_value: 'application/json' },
        { header_key: 'Originator', header_value: 'codex_exec' },
        { header_key: 'User-Agent', header_value: 'codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)' },
        { header_key: 'X-Codex-Beta-Features', header_value: 'terminal_resize_reflow' },
    ],
    claude: [
        { header_key: 'Anthropic-Dangerous-Direct-Browser-Access', header_value: 'true' },
        { header_key: 'Anthropic-Version', header_value: '2023-06-01' },
        { header_key: 'User-Agent', header_value: 'claude-cli/2.1.168 (external, sdk-cli)' },
        { header_key: 'X-App', header_value: 'cli' },
        { header_key: 'X-Stainless-Arch', header_value: 'x64' },
        { header_key: 'X-Stainless-Lang', header_value: 'js' },
        { header_key: 'X-Stainless-OS', header_value: 'Windows' },
        { header_key: 'X-Stainless-Package-Version', header_value: '0.94.0' },
        { header_key: 'X-Stainless-Retry-Count', header_value: '0' },
        { header_key: 'X-Stainless-Runtime', header_value: 'node' },
        { header_key: 'X-Stainless-Runtime-Version', header_value: 'v24.3.0' },
        { header_key: 'X-Stainless-Timeout', header_value: '600' },
    ],
    openaiPython: [
        { header_key: 'Accept', header_value: 'application/json' },
        { header_key: 'Accept-Encoding', header_value: 'identity' },
        { header_key: 'Content-Type', header_value: 'application/json' },
        { header_key: 'User-Agent', header_value: 'OpenAI/Python 1.99.9' },
        { header_key: 'X-Stainless-Lang', header_value: 'python' },
        { header_key: 'X-Stainless-Package-Version', header_value: '1.99.9' },
        { header_key: 'X-Stainless-OS', header_value: 'Windows' },
        { header_key: 'X-Stainless-Arch', header_value: 'x64' },
        { header_key: 'X-Stainless-Runtime', header_value: 'CPython' },
        { header_key: 'X-Stainless-Runtime-Version', header_value: '3.12.0' },
        { header_key: 'X-Stainless-Async', header_value: 'false' },
        { header_key: 'X-Stainless-Retry-Count', header_value: '0' },
        { header_key: 'X-Stainless-Timeout', header_value: '600' },
    ],
};

function modelTestProxyLabel(result: Pick<ModelTestResult, 'proxy_used' | 'proxy_source' | 'proxy_scheme' | 'proxy_status'>) {
    if (!result.proxy_used) return 'direct';
    const parts = ['proxy'];
    if (result.proxy_source) parts.push(result.proxy_source);
    if (result.proxy_scheme) parts.push(result.proxy_scheme);
    if (result.proxy_status) parts.push(`HTTP ${result.proxy_status}`);
    return parts.join(' ');
}

function AdvancedSettingsShell({
    panel,
    title,
    children,
}: {
    panel: boolean;
    title: string;
    children: ReactNode;
}) {
    if (panel) {
        return (
            <aside className="absolute right-0 top-0 z-10 w-full max-w-[min(26rem,100%)] rounded-xl border bg-card max-h-[72vh] overflow-y-auto shadow-lg animate-in fade-in slide-in-from-right-3">
                <div className="space-y-4 p-4">
                    <div className="border-b pb-3 text-sm font-medium text-card-foreground">
                        {title}
                    </div>
                    {children}
                </div>
            </aside>
        );
    }

    return (
        <Accordion type="single" collapsible className="w-full rounded-xl border bg-card">
            <AccordionItem value="advanced" className="border-none">
                <AccordionTrigger className="rounded-xl px-4 py-3 text-sm font-medium text-card-foreground transition-colors hover:bg-muted/30 hover:no-underline">
                    {title}
                </AccordionTrigger>
                <AccordionContent className="space-y-4 border-t px-4 pb-4 pt-4">
                    {children}
                </AccordionContent>
            </AccordionItem>
        </Accordion>
    );
}

export interface ChannelKeyFormItem {
    id?: number;
    enabled: boolean;
    channel_key: string;
    status_code?: number;
    last_use_time_stamp?: number;
    total_cost?: number;
    remark?: string;
}

export interface ChannelFormData {
    name: string;
    type: ChannelType;
    priority: number;
    max_concurrent: number;
    rpm_limit: number;
    key_select_strategy: KeySelectStrategy;
    base_urls: Channel['base_urls'];
    custom_header: Channel['custom_header'];
    cloak_mode: string;
    cloak_profile_id: number;
    channel_proxy: string;
    param_override: string;
    system_prompt_override: string;
    prompt_override_mode: PromptOverrideMode;
    keys: ChannelKeyFormItem[];
    model: string;
    custom_model: string;
    discovered_models: string[];
    selected_models: string[];
    anthropic_context_1m: boolean;
    enabled: boolean;
    proxy: boolean;
    auto_sync: boolean;
    auto_group: AutoGroupType;
    match_regex: string;
    openai_chat_path: string;
    openai_models_path: string;
}

export interface ChannelFormProps {
    formData: ChannelFormData;
    onFormDataChange: (data: ChannelFormData) => void;
    onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
    isPending: boolean;
    submitText: string;
    pendingText: string;
    onCancel?: () => void;
    cancelText?: string;
    idPrefix?: string;
    advancedMode?: 'accordion' | 'panel';
    advancedOpen?: boolean;
}

const CHANNEL_BASE_URL_SUFFIXES: Partial<Record<ChannelType, string[]>> = {
    [ChannelType.OpenAIChat]: ['/v1/chat/completions', '/chat/completions', '/v1/models', '/models', '/v1'],
    [ChannelType.OpenAIResponse]: ['/v1/responses', '/responses', '/v1/models', '/models', '/v1'],
    [ChannelType.OpenAIEmbedding]: ['/v1/embeddings', '/embeddings', '/v1/models', '/models', '/v1'],
    [ChannelType.Anthropic]: ['/v1/messages', '/messages', '/v1/models', '/v1'],
    [ChannelType.Gemini]: ['/v1beta/models', '/v1/models', '/models', '/v1beta', '/v1'],
};

function isCustomOpenAIChat(type: ChannelType) {
    return type === ChannelType.CustomOpenAIChat;
}

function stripKnownEndpointSuffix(type: ChannelType, pathname: string) {
    let path = pathname.replace(/\/+$/, '');
    if (!path || path === '/') return '';

    if (type === ChannelType.Gemini) {
        const lowerPath = path.toLowerCase();
        if (lowerPath.includes(':generatecontent') || lowerPath.includes(':streamgeneratecontent')) {
            for (const marker of ['/v1beta/models/', '/v1/models/', '/models/']) {
                const idx = lowerPath.lastIndexOf(marker);
                if (idx >= 0) {
                    path = path.slice(0, idx).replace(/\/+$/, '');
                    break;
                }
            }
        }
    }

    const lowerPath = path.toLowerCase();
    for (const suffix of CHANNEL_BASE_URL_SUFFIXES[type] ?? []) {
        if (lowerPath === suffix || lowerPath.endsWith(suffix)) {
            path = path.slice(0, path.length - suffix.length).replace(/\/+$/, '');
            break;
        }
    }
    return path;
}

export function normalizeBaseUrlForChannelType(type: ChannelType, value: string) {
    const trimmed = value.trim();
    if (!trimmed || !(type in CHANNEL_BASE_URL_SUFFIXES)) return trimmed;

    try {
        const parsed = new URL(trimmed);
        parsed.pathname = stripKnownEndpointSuffix(type, parsed.pathname);
        return parsed.toString().replace(/\/$/, '');
    } catch {
        if (type === ChannelType.Gemini) {
            return trimmed
                .replace(/\/(?:v1beta\/models|v1\/models|models)\/[^?#]+:(?:streamGenerateContent|generateContent)\/?$/i, '')
                .replace(/\/(?:v1beta\/models|v1\/models|models|v1beta|v1)\/?$/i, '');
        }
        return trimmed.replace(/\/(?:v1\/chat\/completions|chat\/completions|v1\/responses|responses|v1\/embeddings|embeddings|v1\/messages|messages|v1\/models|models|v1)\/?$/i, '');
    }
}

export function ChannelForm({
    formData,
    onFormDataChange,
    onSubmit,
    isPending,
    submitText,
    pendingText,
    onCancel,
    cancelText,
    idPrefix = 'channel',
    advancedMode = 'accordion',
    advancedOpen = false,
}: ChannelFormProps) {
    const t = useTranslations('channel.form');

    // Ensure the form always shows at least 1 row for base_urls / keys / custom_header.
    // This avoids "empty list" UI and also keeps URL + APIKEY layout consistent.
    useEffect(() => {
        if (!formData.base_urls || formData.base_urls.length === 0) {
            onFormDataChange({ ...formData, base_urls: [{ url: '', delay: 0 }] });
            return;
        }
        if (!formData.keys || formData.keys.length === 0) {
            onFormDataChange({ ...formData, keys: [{ enabled: true, channel_key: '' }] });
            return;
        }
        if (!formData.custom_header || formData.custom_header.length === 0) {
            onFormDataChange({ ...formData, custom_header: [{ header_key: '', header_value: '' }] });
        }
    }, [formData, onFormDataChange]);

    const legacySelectedModels = `${formData.model || ''},${formData.custom_model || ''}`
        .split(',')
        .map(cleanOneMillionModelName)
        .filter(Boolean);
    const selectedModels = (formData.selected_models?.length ? formData.selected_models : legacySelectedModels)
        .map(cleanOneMillionModelName)
        .filter(Boolean);
    const autoModels = selectedModels;
    const customModels: string[] = [];
    const [inputValue, setInputValue] = useState('');
    const [fetchedModels, setFetchedModels] = useState<string[]>(() => expandOneMillionModelAliases(formData.discovered_models ?? []));
    const inputRef = useRef<HTMLInputElement>(null);
    const [bulkHeaderText, setBulkHeaderText] = useState('');

    const fetchModel = useFetchModel();
    const channelTest = useTestChannelConfig();
    const { data: fingerprintProfiles } = useFingerprintProfileList();
    const [channelTestResult, setChannelTestResult] = useState<ModelTestResult | null>(null);
    const [channelTestStream, setChannelTestStream] = useState(true);

    const effectiveKey =
        formData.keys.find((k) => k.enabled && k.channel_key.trim())?.channel_key.trim() || '';

    const updateModels = (nextAuto: string[], nextCustom: string[]) => {
        const selected_models = [...nextAuto, ...nextCustom]
            .map(cleanOneMillionModelName)
            .filter(Boolean);
        const model = selected_models.join(',');
        const custom_model = '';
        if (formData.model === model && formData.custom_model === custom_model && JSON.stringify(formData.selected_models ?? []) === JSON.stringify(selected_models)) return;
        onFormDataChange({ ...formData, model, custom_model, selected_models });
    };

    const handleRefreshModels = async () => {
        if (!formData.base_urls?.[0]?.url || !effectiveKey) return;
        fetchModel.mutate(
            {
                type: formData.type,
                base_urls: formData.base_urls.map((item) => ({
                    ...item,
                    url: normalizeBaseUrlForChannelType(formData.type, item.url),
                })),
                keys: formData.keys
                    .filter((k) => k.channel_key.trim())
                    .map((k) => ({ enabled: k.enabled, channel_key: k.channel_key.trim() })),
                proxy: formData.proxy,
                channel_proxy: formData.channel_proxy?.trim() || null,
                match_regex: formData.match_regex.trim() || null,
                custom_header: formData.custom_header?.filter((h) => h.header_key.trim()) || [],
                openai_chat_path: formData.openai_chat_path.trim(),
                openai_models_path: formData.openai_models_path.trim(),
            },
            {
                onSuccess: (data) => {
                    if (data && data.length > 0) {
                        const nextFetched = expandOneMillionModelAliases(data.map((m) => m.trim()).filter(Boolean));
                        setFetchedModels(nextFetched);
                        onFormDataChange({ ...formData, discovered_models: nextFetched });
                        toast.success(t('modelRefreshSuccess'));
                    } else {
                        setFetchedModels([]);
                        onFormDataChange({ ...formData, discovered_models: [] });
                        toast.warning(t('modelRefreshEmpty'));
                    }
                },
                onError: (error) => {
                    const errorMessage = error instanceof Error ? error.message : String(error);
                    toast.error(t('modelRefreshFailed'), { description: errorMessage });
                },
            }
        );
    };

    const testModel = inputValue.trim() || autoModels[0] || customModels[0] || '';
    const channelTestForcedStream = formData.type === ChannelType.OpenAIResponse
        || (formData.type === ChannelType.Anthropic && formData.anthropic_context_1m)
        || isStreamRequiredModel(testModel);

    const buildChannelTestConfig = () => ({
        name: formData.name.trim() || 'current channel',
        type: formData.type,
        enabled: formData.enabled,
        priority: formData.priority,
        max_concurrent: formData.max_concurrent,
        rpm_limit: formData.rpm_limit,
        base_urls: formData.base_urls.map((item) => ({
            ...item,
            url: normalizeBaseUrlForChannelType(formData.type, item.url),
        })),
        keys: formData.keys
            .filter((key) => key.channel_key.trim())
            .map((key) => ({
                id: key.id ?? 0,
                channel_id: 0,
                enabled: key.enabled,
                channel_key: key.channel_key.trim(),
                status_code: key.status_code ?? 0,
                last_use_time_stamp: key.last_use_time_stamp ?? 0,
                total_cost: key.total_cost ?? 0,
                remark: key.remark ?? '',
            })),
        model: formData.model,
        custom_model: formData.custom_model,
        selected_models: formData.selected_models?.map(cleanOneMillionModelName).filter(Boolean),
        discovered_models: formData.discovered_models?.map(cleanOneMillionModelName).filter(Boolean),
        anthropic_context_1m: formData.anthropic_context_1m,
        proxy: formData.proxy,
        auto_sync: formData.auto_sync,
        auto_group: formData.auto_group,
        custom_header: formData.custom_header?.filter((header) => header.header_key.trim()) || [],
        cloak: { mode: formData.cloak_mode || 'auto', profile_id: formData.cloak_profile_id ?? 0 },
        channel_proxy: formData.channel_proxy?.trim() || null,
        openai_chat_path: formData.openai_chat_path.trim(),
        openai_models_path: formData.openai_models_path.trim(),
        param_override: formData.param_override.trim() || null,
        system_prompt_override: formData.system_prompt_override,
        prompt_override_mode: formData.prompt_override_mode,
        match_regex: formData.match_regex.trim() || null,
    });

    const handleTestCurrentChannel = () => {
        if (!formData.base_urls?.[0]?.url || !effectiveKey || !testModel) {
            toast.error('请先填写 Base URL、API Key 和模型');
            return;
        }
        channelTest.mutate(
            {
                channel: buildChannelTestConfig(),
                model: testModel,
                endpoint: defaultModelTestEndpointForChannel(formData.type),
                prompt: 'Reply with exactly OK.',
                stream: channelTestForcedStream ? true : channelTestStream,
                timeout_seconds: formData.type === ChannelType.Anthropic ? 180 : 30,
            },
            {
                onSuccess: (data) => {
                    const result = data.results[0] ?? null;
                    setChannelTestResult(result);
                    if (result?.success) {
                        toast.success('渠道测试成功', { description: result.response_preview || 'OK' });
                        return;
                    }
                    toast.error('渠道测试失败', { description: result?.error || '无可用结果' });
                },
                onError: (error) => {
                    setChannelTestResult({
                        model: testModel,
                        request_model: testModel,
                        route_used: false,
                        success: false,
                        duration_ms: 0,
                        error: error.message,
                    });
                    toast.error('渠道测试失败', { description: error.message });
                },
            }
        );
    };

    const handleAddModel = (model: string) => {
        const trimmedModel = cleanOneMillionModelName(model);
        if (trimmedModel && !customModels.includes(trimmedModel) && !autoModels.includes(trimmedModel)) {
            updateModels(autoModels, [...customModels, trimmedModel]);
        }
        setInputValue('');
    };

    const selectedModelSet = new Set([...autoModels, ...customModels]);
    const unselectedFetchedModels = fetchedModels.filter((model) => !selectedModelSet.has(model));

    const handleSelectFetchedModel = (model: string) => {
        if (selectedModelSet.has(model)) return;
        updateModels([...autoModels, model], customModels);
    };

    const handleSelectAllFetchedModels = () => {
        if (unselectedFetchedModels.length === 0) return;
        updateModels([...autoModels, ...unselectedFetchedModels], customModels);
    };

    const handleRemoveAutoModel = (model: string) => {
        updateModels(autoModels.filter(m => m !== model), customModels);
    };

    const handleRemoveCustomModel = (model: string) => {
        updateModels(autoModels, customModels.filter(m => m !== model));
    };

    const handleInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter') {
            e.preventDefault();
            if (inputValue.trim()) handleAddModel(inputValue);
        }
    };

    const handleAddKey = () => {
        onFormDataChange({
            ...formData,
            keys: [...formData.keys, { enabled: true, channel_key: '' }],
        });
    };

    const handleUpdateKey = (idx: number, patch: Partial<ChannelKeyFormItem>) => {
        const next = formData.keys.map((k, i) => (i === idx ? { ...k, ...patch } : k));
        onFormDataChange({ ...formData, keys: next });
    };

    const handleRemoveKey = (idx: number) => {
        const curr = formData.keys ?? [];
        if (curr.length <= 1) return;
        const next = curr.filter((_, i) => i !== idx);
        onFormDataChange({ ...formData, keys: next });
    };

    const handleAddBaseUrl = () => {
        onFormDataChange({
            ...formData,
            base_urls: [...(formData.base_urls ?? []), { url: '', delay: 0 }],
        });
    };

    const handleUpdateBaseUrl = (idx: number, patch: Partial<Channel['base_urls'][number]>) => {
        const next = (formData.base_urls ?? []).map((u, i) => (i === idx ? { ...u, ...patch } : u));
        onFormDataChange({ ...formData, base_urls: next });
    };

    const handleRemoveBaseUrl = (idx: number) => {
        const curr = formData.base_urls ?? [];
        if (curr.length <= 1) return;
        onFormDataChange({ ...formData, base_urls: curr.filter((_, i) => i !== idx) });
    };

    const handleAddHeader = () => {
        onFormDataChange({
            ...formData,
            custom_header: [...(formData.custom_header ?? []), { header_key: '', header_value: '' }],
        });
    };

    const handleUpdateHeader = (idx: number, patch: Partial<Channel['custom_header'][number]>) => {
        const next = (formData.custom_header ?? []).map((h, i) => (i === idx ? { ...h, ...patch } : h));
        onFormDataChange({ ...formData, custom_header: next });
    };

    const handleRemoveHeader = (idx: number) => {
        const curr = formData.custom_header ?? [];
        if (curr.length <= 1) return;
        onFormDataChange({ ...formData, custom_header: curr.filter((_, i) => i !== idx) });
    };

    const parseBulkHeaders = (value: string): Channel['custom_header'] => {
        const trimmed = value.trim();
        if (!trimmed) return [];
        try {
            const parsed = JSON.parse(trimmed) as unknown;
            if (Array.isArray(parsed)) {
                return parsed
                    .filter((item): item is { header_key?: unknown; header_value?: unknown } => typeof item === 'object' && item !== null)
                    .map((item) => ({ header_key: String(item.header_key ?? '').trim(), header_value: String(item.header_value ?? '') }))
                    .filter((item) => item.header_key);
            }
            if (typeof parsed !== 'object' || parsed === null) return [];
            const parsedRecord = parsed as Record<string, unknown>;
            const source = typeof parsedRecord.headers === 'object' && parsedRecord.headers !== null
                ? parsedRecord.headers as Record<string, unknown>
                : parsedRecord;
            return Object.entries(source)
                .map(([key, val]) => ({ header_key: key.trim(), header_value: String(val) }))
                .filter((item) => item.header_key);
        } catch {
            // Fall through to line-based YAML/curl parsing.
        }

        const headers: Channel['custom_header'] = [];
        for (const rawLine of value.split(/\r?\n/)) {
            let line = rawLine.trim();
            if (!line || line.startsWith('#') || line === 'headers:' || line === 'cloak:') continue;
            if (/^mode\s*:/i.test(line)) continue;
            line = line.replace(/^-\s*/, '').replace(/^-H\s+/i, '').trim();
            line = line.replace(/^['"]|['"]$/g, '');
            const idx = line.indexOf(':');
            if (idx <= 0) continue;
            const key = line.slice(0, idx).trim().replace(/^['"]|['"]$/g, '');
            const headerValue = line.slice(idx + 1).trim().replace(/^['"]|['"]$/g, '');
            if (key) headers.push({ header_key: key, header_value: headerValue });
        }
        return headers;
    };

    const handleApplyHeaderPreset = (preset: HeaderPresetKey) => {
        const next = new Map<string, Channel['custom_header'][number]>();
        for (const header of formData.custom_header ?? []) {
            const key = header.header_key.trim();
            if (!key) continue;
            next.set(key.toLowerCase(), { ...header, header_key: key });
        }
        for (const header of HEADER_PRESETS[preset]) {
            next.set(header.header_key.toLowerCase(), { ...header });
        }
        onFormDataChange({
            ...formData,
            custom_header: Array.from(next.values()),
        });
    };

    const handleImportHeaders = () => {
        const imported = parseBulkHeaders(bulkHeaderText);
        if (imported.length === 0) {
            toast.error('没有解析到 Header');
            return;
        }
        const next = new Map<string, Channel['custom_header'][number]>();
        for (const header of formData.custom_header ?? []) {
            const key = header.header_key.trim();
            if (!key) continue;
            next.set(key.toLowerCase(), { ...header, header_key: key });
        }
        for (const header of imported) {
            next.set(header.header_key.toLowerCase(), header);
        }
        onFormDataChange({ ...formData, custom_header: Array.from(next.values()) });
        setBulkHeaderText('');
        toast.success(`已导入 ${imported.length} 个 Header`);
    };

    const isAdvancedPanel = advancedMode === 'panel';
    const showAdvanced = !isAdvancedPanel || advancedOpen;
    const primaryColumnClass = '';

    return (
        <form
            onSubmit={onSubmit}
            className={cn(
                "px-1",
                isAdvancedPanel && advancedOpen
                    ? "relative space-y-4 md:pr-[28rem]"
                    : "space-y-4"
            )}
        >
            <div className={cn("grid grid-cols-1 md:grid-cols-2 gap-4", primaryColumnClass)}>
                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-name`} className="text-sm font-medium text-card-foreground">
                        {t('name')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-name`}
                        type="text"
                        value={formData.name}
                        onChange={(event) => onFormDataChange({ ...formData, name: event.target.value })}
                        required
                    />
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-type`} className="text-sm font-medium text-card-foreground">
                        {t('type')}
                    </label>
                    <Select
                        value={String(formData.type)}
                        onValueChange={(value) => onFormDataChange({ ...formData, type: Number(value) as ChannelType })}
                    >
                        <SelectTrigger id={`${idPrefix}-type`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                            <SelectValue />
                        </SelectTrigger>
                        <SelectContent className='rounded-xl'>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIChat)}>{t('typeOpenAIChat')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIResponse)}>{t('typeOpenAIResponse')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Anthropic)}>{t('typeAnthropic')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Gemini)}>{t('typeGemini')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Volcengine)}>{t('typeVolcengine')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIEmbedding)}>{t('typeOpenAIEmbedding')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.CustomOpenAIChat)}>{t('typeCustomOpenAIChat')}</SelectItem>
                        </SelectContent>
                    </Select>
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-priority`} className="text-sm font-medium text-card-foreground">
                        {t('priority')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-priority`}
                        type="number"
                        inputMode="numeric"
                        step={1}
                        value={String(formData.priority ?? 0)}
                        onChange={(event) => {
                            const n = Number.parseInt(event.target.value, 10);
                            onFormDataChange({ ...formData, priority: Number.isFinite(n) ? n : 0 });
                        }}
                    />
                    <p className="text-xs text-muted-foreground">{t('priorityHint')}</p>
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-max-concurrent`} className="text-sm font-medium text-card-foreground">
                        {t('maxConcurrent')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-max-concurrent`}
                        type="number"
                        inputMode="numeric"
                        min={0}
                        step={1}
                        value={String(formData.max_concurrent ?? 0)}
                        onChange={(event) => {
                            const n = Number.parseInt(event.target.value, 10);
                            onFormDataChange({ ...formData, max_concurrent: Number.isFinite(n) && n > 0 ? n : 0 });
                        }}
                    />
                    <p className="text-xs text-muted-foreground">{t('maxConcurrentHint')}</p>
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-rpm-limit`} className="text-sm font-medium text-card-foreground">
                        {t('rpmLimit')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-rpm-limit`}
                        type="number"
                        inputMode="numeric"
                        min={0}
                        step={1}
                        value={String(formData.rpm_limit ?? 0)}
                        onChange={(event) => {
                            const n = Number.parseInt(event.target.value, 10);
                            onFormDataChange({ ...formData, rpm_limit: Number.isFinite(n) && n > 0 ? n : 0 });
                        }}
                    />
                    <p className="text-xs text-muted-foreground">{t('rpmLimitHint')}</p>
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-key-select-strategy`} className="text-sm font-medium text-card-foreground">
                        {t('keySelectStrategy')}
                    </label>
                    <Select
                        value={String(formData.key_select_strategy ?? 0)}
                        onValueChange={(value) => onFormDataChange({ ...formData, key_select_strategy: Number(value) as KeySelectStrategy })}
                    >
                        <SelectTrigger id={`${idPrefix}-key-select-strategy`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                            <SelectValue />
                        </SelectTrigger>
                        <SelectContent className='rounded-xl'>
                            <SelectItem className='rounded-xl' value="0">{t('keySelectStrategyCostBalanced')}</SelectItem>
                            <SelectItem className='rounded-xl' value="1">{t('keySelectStrategySticky')}</SelectItem>
                        </SelectContent>
                    </Select>
                </div>
            </div>

            <div className={cn("space-y-2", primaryColumnClass)}>
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">
                        {t('baseUrls')} {formData.base_urls.length > 0 ? `(${formData.base_urls.length})` : ''}
                    </label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleAddBaseUrl}
                        className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <Plus className="h-3 w-3 mr-1" />
                        {t('add')}
                    </Button>
                </div>
                <div className="space-y-2">
                    {(formData.base_urls ?? []).map((u, idx) => (
                        <div key={`baseurl-${idx}`} className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-2">
                            <Input
                                id={`${idPrefix}-base-${idx}`}
                                type="url"
                                value={u.url}
                                onChange={(e) => handleUpdateBaseUrl(idx, { url: e.target.value })}
                                onBlur={(e) => {
                                    const normalized = normalizeBaseUrlForChannelType(formData.type, e.target.value);
                                    if (normalized !== e.target.value) {
                                        handleUpdateBaseUrl(idx, { url: normalized });
                                    }
                                }}
                                placeholder={t('baseUrlUrl')}
                                required={idx === 0}
                                className="min-w-0 rounded-xl"
                            />
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => handleRemoveBaseUrl(idx)}
                                disabled={(formData.base_urls ?? []).length <= 1}
                                className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive disabled:opacity-40 hover:bg-transparent"
                                title="Remove"
                            >
                                <X className="h-4 w-4" />
                            </Button>
                        </div>
                    ))}
                </div>
            </div>

            <div className={cn("space-y-2", primaryColumnClass)}>
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">
                        {t('apiKey')} {formData.keys.length > 0 ? `(${formData.keys.length})` : ''}
                    </label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleAddKey}
                        className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <Plus className="h-3 w-3 mr-1" />
                        {t('add')}
                    </Button>
                </div>
                <div className="space-y-2">
                    {(formData.keys ?? []).map((k, idx) => (
                        <div key={k.id ?? `new-${idx}`} className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(7rem,10rem)_auto_auto]">
                            <Input
                                type="text"
                                value={k.channel_key}
                                onChange={(e) => handleUpdateKey(idx, { channel_key: e.target.value })}
                                placeholder={t('apiKey')}
                                required={idx === 0}
                                className="col-span-3 min-w-0 rounded-xl sm:col-span-1"
                            />
                            <Input
                                type="text"
                                value={k.remark ?? ''}
                                onChange={(e) => handleUpdateKey(idx, { remark: e.target.value })}
                                placeholder={t('remark')}
                                className="col-span-3 min-w-0 rounded-xl sm:col-span-1"
                            />
                            <Switch
                                checked={k.enabled}
                                onCheckedChange={(checked) => handleUpdateKey(idx, { enabled: checked })}
                            />
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => handleRemoveKey(idx)}
                                disabled={(formData.keys ?? []).length <= 1}
                                className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive hover:bg-transparent disabled:opacity-40"
                                title="Remove"
                            >
                                <X className="h-4 w-4" />
                            </Button>
                        </div>
                    ))}
                </div>
            </div>

            <div className={cn("space-y-2", primaryColumnClass)}>
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">{t('model')}</label>
                    <div className="flex items-center gap-1">
                        <label
                            className={cn(
                                "mr-1 inline-flex h-6 items-center gap-1 rounded-lg border border-border bg-background px-2 text-[11px] text-muted-foreground",
                                channelTestForcedStream && "border-primary/30 bg-primary/10 text-primary"
                            )}
                            title={channelTestForcedStream ? "该模型必须走流式测试，Octopus 会自动切到正确链路，避免误判。" : "默认流式；需要排查兼容性时可手动切非流。"}
                        >
                            <Switch
                                checked={channelTestForcedStream || channelTestStream}
                                onCheckedChange={setChannelTestStream}
                                disabled={channelTestForcedStream}
                                aria-label="渠道配置测试流式模式"
                            />
                            <span>{channelTestForcedStream || channelTestStream ? '流式' : '非流'}</span>
                        </label>
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={handleTestCurrentChannel}
                            disabled={!formData.base_urls?.[0]?.url || !effectiveKey || !testModel || channelTest.isPending}
                            className="h-6 px-2 text-xs text-muted-foreground/60 hover:text-muted-foreground hover:bg-transparent"
                        >
                            {channelTest.isPending ? <Loader2 className="h-3 w-3 mr-1 animate-spin" /> : <Play className="h-3 w-3 mr-1" />}
                            测试
                        </Button>
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={handleRefreshModels}
                            disabled={!formData.base_urls?.[0]?.url || !effectiveKey || fetchModel.isPending}
                            className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                        >
                            <RefreshCw className={`h-3 w-3 mr-1 ${fetchModel.isPending ? 'animate-spin' : ''}`} />
                            {t('modelRefresh')}
                        </Button>
                    </div>
                </div>
                <input type="hidden" value={formData.model} required />

                <div className="relative">
                    <Input
                        ref={inputRef}
                        id={`${idPrefix}-model-custom`}
                        type="text"
                        value={inputValue}
                        onChange={(e) => setInputValue(e.target.value)}
                        onKeyDown={handleInputKeyDown}
                        placeholder="可手输模型，例如 claude-fable-5 / claude-opus-4-8 / gpt-5.5"
                        className="pr-10 rounded-xl"
                    />
                    {inputValue.trim() && !customModels.includes(inputValue.trim()) && !autoModels.includes(inputValue.trim()) && (
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => handleAddModel(inputValue)}
                            className="absolute rounded-lg right-1 top-1/2 -translate-y-1/2 h-7 w-7 p-0 text-muted-foreground hover:bg-accent hover:text-accent-foreground transition-colors"
                            title={t('modelAdd')}
                        >
                            <Plus className="size-4" />
                        </Button>
                    )}
                </div>

                {fetchedModels.length > 0 && (
                    <div className="rounded-xl border border-border bg-muted/20 p-2.5">
                        <div className="mb-2 flex items-center justify-between gap-2">
                            <label className="text-xs font-medium text-card-foreground">
                                同步发现候选 ({fetchedModels.length})
                            </label>
                            <div className="flex items-center gap-1">
                                <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    onClick={handleSelectAllFetchedModels}
                                    disabled={unselectedFetchedModels.length === 0}
                                    className="h-6 px-2 text-xs text-muted-foreground/60 hover:text-muted-foreground hover:bg-transparent disabled:opacity-40"
                                >
                                    {t('modelSelectAll')}
                                </Button>
                                <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    onClick={() => setFetchedModels([])}
                                    className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                                >
                                    {t('modelClearFetched')}
                                </Button>
                            </div>
                        </div>
                        <div className="flex max-h-36 min-w-0 flex-wrap gap-1.5 overflow-y-auto pr-1">
                            {fetchedModels.map((model) => {
                                const selected = selectedModelSet.has(model);
                                return (
                                    <button
                                        key={model}
                                        type="button"
                                        onClick={() => handleSelectFetchedModel(model)}
                                        disabled={selected}
                                        className={`flex max-w-full min-w-0 items-center rounded-full border px-2.5 py-1 text-xs transition-colors ${selected
                                            ? 'border-border bg-background text-muted-foreground opacity-70'
                                            : 'border-border/70 bg-background text-foreground hover:border-primary/50 hover:bg-accent hover:text-accent-foreground'
                                            }`}
                                        title={selected ? t('modelAlreadySelected') : t('modelAdd')}
                                    >
                                        <span className="min-w-0 truncate">{model}</span>
                                        {selected && <span className="ml-1 shrink-0 text-[10px]">{t('modelAlreadySelected')}</span>}
                                    </button>
                                );
                            })}
                        </div>
                    </div>
                )}

                <div className="space-y-2">
                    <div className="flex items-center justify-between">
                        <label className="text-xs font-medium text-card-foreground">
                            已启用模型 {(autoModels.length + customModels.length) > 0 && `(${autoModels.length + customModels.length})`}
                        </label>
                        {(autoModels.length + customModels.length) > 0 && (
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => {
                                    updateModels([], []);
                                }}
                                className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                            >
                                {t('modelClearAll')}
                            </Button>
                        )}
                    </div>
                    <div className="rounded-xl border border-border bg-muted/30 p-2.5 max-h-40 min-h-12 overflow-y-auto">
                        {(autoModels.length + customModels.length) > 0 ? (
                            <div className="flex min-w-0 flex-wrap gap-1.5">
                                {autoModels.map((model) => (
                                    <Badge key={model} variant="secondary" className="max-w-full bg-muted hover:bg-muted/80">
                                        <span className="min-w-0 truncate" title={model}>{model}</span>
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveAutoModel(model)}
                                            className="ml-1 shrink-0 rounded-sm opacity-70 hover:opacity-100 focus:outline-none focus:ring-1 focus:ring-ring"
                                        >
                                            <X className="h-3 w-3" />
                                        </button>
                                    </Badge>
                                ))}
                                {customModels.map((model) => (
                                    <Badge key={model} className="max-w-full bg-primary hover:bg-primary/90">
                                        <span className="min-w-0 truncate" title={model}>{model}</span>
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveCustomModel(model)}
                                            className="ml-1 shrink-0 rounded-sm opacity-70 hover:opacity-100 focus:outline-none focus:ring-1 focus:ring-ring"
                                        >
                                            <X className="h-3 w-3" />
                                        </button>
                                    </Badge>
                                ))}
                            </div>
                        ) : (
                            <div className="flex items-center justify-center h-8 text-xs text-muted-foreground">
                                {t('modelNoSelected')}
                            </div>
                        )}
                    </div>
                </div>

                {formData.type === ChannelType.Anthropic && (
                    <div className="rounded-xl border border-border bg-background/70 p-3">
                        <div className="flex flex-wrap items-start justify-between gap-3">
                            <div className="min-w-0 space-y-1">
                                <div className="text-xs font-semibold text-card-foreground">Claude 1M 上下文能力</div>
                                <p className="text-xs leading-relaxed text-muted-foreground">
                                    模型名保持干净，例如 claude-fable-5；开启后出站自动附加 Anthropic-Beta: context-1m-2025-08-07。
                                </p>
                            </div>
                            <label className="inline-flex shrink-0 items-center gap-2 rounded-lg border border-border bg-muted/30 px-2.5 py-1.5 text-xs text-muted-foreground">
                                <Switch
                                    checked={formData.anthropic_context_1m}
                                    onCheckedChange={(checked) => onFormDataChange({ ...formData, anthropic_context_1m: checked })}
                                    aria-label="启用 Claude 1M 上下文能力"
                                />
                                <span>{formData.anthropic_context_1m ? '已启用 1M' : '未启用'}</span>
                            </label>
                        </div>
                    </div>
                )}

                {channelTestResult && (
                    <div className={`rounded-xl border p-2.5 text-xs ${channelTestResult.success ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300' : 'border-destructive/30 bg-destructive/10 text-destructive'}`}>
                        <div className="flex min-w-0 flex-wrap items-center gap-2">
                            <span className="font-medium">{channelTestResult.success ? '测试成功' : '测试失败'}</span>
                            {channelTestResult.status_code ? <span>HTTP {channelTestResult.status_code}</span> : null}
                            {channelTestResult.duration_ms ? <span>{channelTestResult.duration_ms}ms</span> : null}
                            <span
                                className={cn(
                                    'rounded-md border px-1.5 py-0.5 font-mono text-[10px]',
                                    channelTestResult.proxy_used ? 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300' : 'border-border bg-background/60 text-muted-foreground'
                                )}
                                title={channelTestResult.proxy_target || modelTestProxyLabel(channelTestResult)}
                            >
                                {modelTestProxyLabel(channelTestResult)}
                            </span>
                            {channelTestResult.upstream_path ? <span className="font-mono">{channelTestResult.upstream_path}</span> : null}
                        </div>
                        <pre className="mt-2 max-h-28 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-background/70 p-2 text-[11px] text-foreground">
                            {channelTestResult.success ? (channelTestResult.response_preview || 'OK') : (channelTestResult.error || '无错误详情')}
                        </pre>
                        {channelTestResult.attempts?.length ? (
                            <div className="mt-2 grid gap-1 text-[11px] text-muted-foreground">
                                {channelTestResult.attempts.slice(0, 2).map((attempt) => (
                                    <div key={attempt.attempt_num} className="min-w-0 truncate font-mono" title={`${attempt.channel_name} / ${attempt.model_name} / ${attempt.upstream_path || '-'} / ${attempt.msg || ''}`}>
                                        #{attempt.attempt_num} {attempt.status} {attempt.channel_name} / {attempt.model_name}{attempt.upstream_path ? ` / ${attempt.upstream_path}` : ''}{attempt.proxy_used ? ` / ${modelTestProxyLabel(attempt)}` : ''}
                                    </div>
                                ))}
                            </div>
                        ) : null}
                    </div>
                )}
            </div>

            {showAdvanced && (
                <AdvancedSettingsShell panel={isAdvancedPanel} title={t('advanced')}>
                        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                            <div className="space-y-2 md:col-span-2">
                                <label htmlFor={`${idPrefix}-auto-group`} className="text-sm font-medium text-card-foreground">
                                    {t('autoGroup')}
                                </label>
                                <Select
                                    value={String(formData.auto_group)}
                                    onValueChange={(value) => onFormDataChange({ ...formData, auto_group: Number(value) as AutoGroupType })}
                                >
                                    <SelectTrigger id={`${idPrefix}-auto-group`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                        <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent className='rounded-xl'>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Fuzzy)}>{t('autoGroupFuzzy')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Exact)}>{t('autoGroupExact')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Regex)}>{t('autoGroupRegex')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.None)}>{t('autoGroupNone')}</SelectItem>
                                    </SelectContent>
                                </Select>
                            </div>

                            <div className="space-y-2">
                                <div className="flex flex-wrap items-center justify-between gap-2">
                                    <label htmlFor={`${idPrefix}-channel-proxy`} className="text-sm font-medium text-card-foreground">
                                        {t('channelProxy')}
                                    </label>
                                    <label className="flex min-w-0 items-center gap-2 cursor-pointer">
                                        <Switch
                                            checked={formData.proxy}
                                            onCheckedChange={(checked) => onFormDataChange({ ...formData, proxy: checked })}
                                        />
                                        <span className="min-w-0 text-sm text-card-foreground">{t('proxy')}</span>
                                    </label>
                                </div>
                                <Input
                                    id={`${idPrefix}-channel-proxy`}
                                    type="text"
                                    value={formData.channel_proxy}
                                    onChange={(e) => onFormDataChange({ ...formData, channel_proxy: e.target.value })}
                                    placeholder={t('channelProxyPlaceholder')}
                                    disabled={!formData.proxy}
                                    className="rounded-xl disabled:cursor-not-allowed disabled:opacity-50"
                                />
                            </div>

                            <div className="space-y-2">
                                <label htmlFor={`${idPrefix}-cloak-mode`} className="text-sm font-medium text-card-foreground">
                                    {t('cloakMode')}
                                </label>
                                <Select
                                    value={formData.cloak_mode || 'auto'}
                                    onValueChange={(value) => onFormDataChange({ ...formData, cloak_mode: value })}
                                >
                                    <SelectTrigger id={`${idPrefix}-cloak-mode`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                        <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent className="rounded-xl">
                                        <SelectItem className="rounded-xl" value="auto">{t('cloakModeAuto')}</SelectItem>
                                        <SelectItem className="rounded-xl" value="always">{t('cloakModeAlways')}</SelectItem>
                                        <SelectItem className="rounded-xl" value="never">{t('cloakModeNever')}</SelectItem>
                                    </SelectContent>
                                </Select>
                                <p className="text-xs text-muted-foreground">{t('cloakModeHint')}</p>
                            </div>

                            <div className="space-y-2 md:col-span-2">
                                <label htmlFor={`${idPrefix}-cloak-profile`} className="text-sm font-medium text-card-foreground">
                                    {t('cloakProfile')}
                                </label>
                                <Select
                                    value={String(formData.cloak_profile_id ?? 0)}
                                    onValueChange={(value) => onFormDataChange({ ...formData, cloak_profile_id: Number(value) })}
                                >
                                    <SelectTrigger id={`${idPrefix}-cloak-profile`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                        <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent className="rounded-xl">
                                        <SelectItem className="rounded-xl" value="0">{t('cloakProfileGlobal')}</SelectItem>
                                        {(fingerprintProfiles ?? []).map((profile) => (
                                            <SelectItem key={profile.id} className="rounded-xl" value={String(profile.id)}>
                                                {profile.name}
                                            </SelectItem>
                                        ))}
                                    </SelectContent>
                                </Select>
                                <p className="text-xs text-muted-foreground">{t('cloakProfileHint')}</p>
                            </div>
                        </div>

                        {isCustomOpenAIChat(formData.type) && (
                            <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                                <div className="space-y-2">
                                    <label htmlFor={`${idPrefix}-openai-chat-path`} className="text-sm font-medium text-card-foreground">
                                        {t('openaiChatPath')}
                                    </label>
                                    <Input
                                        id={`${idPrefix}-openai-chat-path`}
                                        type="text"
                                        value={formData.openai_chat_path}
                                        onChange={(e) => onFormDataChange({ ...formData, openai_chat_path: e.target.value })}
                                        placeholder="/chat/completions"
                                        className="rounded-xl"
                                    />
                                </div>
                                <div className="space-y-2">
                                    <label htmlFor={`${idPrefix}-openai-models-path`} className="text-sm font-medium text-card-foreground">
                                        {t('openaiModelsPath')}
                                    </label>
                                    <Input
                                        id={`${idPrefix}-openai-models-path`}
                                        type="text"
                                        value={formData.openai_models_path}
                                        onChange={(e) => onFormDataChange({ ...formData, openai_models_path: e.target.value })}
                                        placeholder="/models"
                                        className="rounded-xl"
                                    />
                                </div>
                            </div>
                        )}

                        <div className="space-y-2">
                            <div className="flex flex-wrap items-center justify-between gap-2">
                                <label className="text-sm font-medium text-card-foreground">
                                    {t('customHeader')} {formData.custom_header.length > 0 ? `(${formData.custom_header.length})` : ''}
                                </label>
                                <div className="flex flex-wrap items-center gap-1.5">
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        onClick={() => handleApplyHeaderPreset('codex')}
                                        className="h-7 px-2 text-xs rounded-xl"
                                    >
                                        {t('headerPresetCodex')}
                                    </Button>
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        onClick={() => handleApplyHeaderPreset('claude')}
                                        className="h-7 px-2 text-xs rounded-xl"
                                    >
                                        {t('headerPresetClaude')}
                                    </Button>
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        onClick={() => handleApplyHeaderPreset('openaiPython')}
                                        className="h-7 px-2 text-xs rounded-xl"
                                    >
                                        {t('headerPresetOpenAI')}
                                    </Button>
                                    <Button
                                        type="button"
                                        variant="ghost"
                                        size="sm"
                                        onClick={handleAddHeader}
                                        className="h-7 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                                    >
                                        <Plus className="h-3 w-3 mr-1" />
                                        {t('customHeaderAdd')}
                                    </Button>
                                </div>
                            </div>
                            <p className="text-xs text-muted-foreground">{t('headerPresetHint')}</p>
                            <div className="grid gap-2 rounded-xl border border-border/70 bg-muted/20 p-2.5">
                                <textarea
                                    value={bulkHeaderText}
                                    onChange={(event) => setBulkHeaderText(event.target.value)}
                                    placeholder={'headers:\n  User-Agent: ${UA}\n  X-Codex-Beta-Features: terminal_resize_reflow\n-H "Originator: codex_exec"'}
                                    className="min-h-24 rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                />
                                <div className="flex justify-end">
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        onClick={handleImportHeaders}
                                        disabled={bulkHeaderText.trim().length === 0}
                                        className="h-8 rounded-xl px-3 text-xs"
                                    >
                                        批量导入
                                    </Button>
                                </div>
                            </div>
                            <div className="space-y-2">
                                {(formData.custom_header ?? []).map((h, idx) => (
                                    <div key={`hdr-${idx}`} className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]">
                                        <Input
                                            type="text"
                                            value={h.header_key}
                                            onChange={(e) => handleUpdateHeader(idx, { header_key: e.target.value })}
                                            placeholder={t('customHeaderKey')}
                                            className="min-w-0 rounded-xl"
                                        />
                                        <Input
                                            type="text"
                                            value={h.header_value}
                                            onChange={(e) => handleUpdateHeader(idx, { header_value: e.target.value })}
                                            placeholder={t('customHeaderValue')}
                                            className="col-span-2 min-w-0 rounded-xl sm:col-span-1"
                                        />
                                        <Button
                                            type="button"
                                            variant="ghost"
                                            size="sm"
                                            onClick={() => handleRemoveHeader(idx)}
                                            disabled={(formData.custom_header ?? []).length <= 1}
                                            className="col-start-2 row-start-1 h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive hover:bg-transparent disabled:opacity-40 sm:col-start-auto sm:row-start-auto"
                                            title="Remove"
                                        >
                                            <X className="h-4 w-4" />
                                        </Button>
                                    </div>
                                ))}
                            </div>
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-match-regex`} className="text-sm font-medium text-card-foreground">
                                {t('matchRegex')}
                            </label>
                            <Input
                                id={`${idPrefix}-match-regex`}
                                type="text"
                                value={formData.match_regex}
                                onChange={(e) => onFormDataChange({ ...formData, match_regex: e.target.value })}
                                placeholder={t('matchRegexPlaceholder')}
                                className="rounded-xl"
                            />
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-param-override`} className="text-sm font-medium text-card-foreground">
                                {t('paramOverride')}
                            </label>
                            <textarea
                                id={`${idPrefix}-param-override`}
                                value={formData.param_override}
                                onChange={(e) => onFormDataChange({ ...formData, param_override: e.target.value })}
                                placeholder={t('paramOverridePlaceholder')}
                                className="min-h-28 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                        </div>

                        <div className="space-y-2">
                            <div className="flex flex-wrap items-center justify-between gap-2">
                                <label htmlFor={`${idPrefix}-system-prompt-override`} className="text-sm font-medium text-card-foreground">
                                    {t('promptOverride')}
                                </label>
                                <select
                                    value={formData.prompt_override_mode}
                                    onChange={(e) => onFormDataChange({
                                        ...formData,
                                        prompt_override_mode: e.target.value as PromptOverrideMode,
                                    })}
                                    aria-label={t('promptOverrideMode')}
                                    className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="append_system">{t('promptAppend')}</option>
                                    <option value="replace_system">{t('promptReplace')}</option>
                                </select>
                            </div>
                            <textarea
                                id={`${idPrefix}-system-prompt-override`}
                                value={formData.system_prompt_override}
                                onChange={(e) => onFormDataChange({ ...formData, system_prompt_override: e.target.value })}
                                placeholder={t('promptOverridePlaceholder')}
                                className="min-h-28 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                        </div>
                </AdvancedSettingsShell>
            )}

            <div className={cn("flex flex-wrap items-center justify-between gap-4 p-4 rounded-xl bg-muted/20 border border-border/50", primaryColumnClass)}>
                <label className="flex items-center gap-2 cursor-pointer">
                    <Switch
                        checked={formData.enabled}
                        onCheckedChange={(checked) => onFormDataChange({ ...formData, enabled: checked })}
                    />
                    <span className="text-sm font-medium text-card-foreground">{t('enabled')}</span>
                </label>
                <div className="flex min-w-0 flex-wrap items-center gap-3 sm:gap-6">
                    <label className="flex min-w-0 items-center gap-2 cursor-pointer">
                        <Switch
                            checked={formData.auto_sync}
                            onCheckedChange={(checked) => onFormDataChange({ ...formData, auto_sync: checked })}
                        />
                        <span className="min-w-0">
                            <span className="block text-sm text-card-foreground">{t('autoSync')}</span>
                            <span className="block max-w-[18rem] text-xs text-muted-foreground">{t('autoSyncHint')}</span>
                        </span>
                    </label>
                </div>
            </div>

            <div className={cn(`flex flex-col gap-3 pt-2 ${onCancel ? 'sm:flex-row' : ''}`, primaryColumnClass)}>
                {onCancel && cancelText && (
                    <Button
                        type="button"
                        variant="secondary"
                        onClick={onCancel}
                        className="w-full sm:flex-1 rounded-2xl h-12"
                    >
                        {cancelText}
                    </Button>
                )}
                <Button
                    type="submit"
                    disabled={isPending}
                    className="w-full sm:flex-1 rounded-2xl h-12"
                >
                    {isPending ? pendingText : submitText}
                </Button>
            </div>
        </form>
    );
}
