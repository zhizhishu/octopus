'use client';

import { useMemo, useState } from 'react';
import { CheckCircle2, FlaskConical, Loader2, Play, Rows3, XCircle } from 'lucide-react';
import { useAPIKeyList } from '@/api/endpoints/apikey';
import type { ApiError } from '@/api/types';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { ChannelType, defaultModelTestEndpointForChannel, useChannelList } from '@/api/endpoints/channel';
import {
    type ModelTestResult,
    useModelChannelList,
    useModelTest,
} from '@/api/endpoints/model';
import { useUserList } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { SearchableSelect } from '@/components/ui/searchable-select';
import { Switch } from '@/components/ui/switch';
import {
    Table,
    TableBody,
    TableCell,
    TableHead,
    TableHeader,
    TableRow,
} from '@/components/ui/table';
import { cn } from '@/lib/utils';
import { expandOneMillionModelAliases, isStreamRequiredModel, shouldForceTestStream } from '@/lib/model-aliases';
import { getSelectedChannelModels } from '../channel/channel-utils';
import { useNavStore } from '@/components/modules/navbar/nav-store';

const LEGACY_DEFAULT_PROMPT = 'Reply with exactly OK.';
const AUTO_MATH_PROMPT_RE = /^请只回答算式结果，不要解释：\d{4} \+ \d{4} = \?$/;
const ENDPOINT_OPTIONS = [
    { value: 'openai_chat', label: 'OpenAI Chat', path: '/v1/chat/completions' },
    { value: 'openai_responses', label: 'OpenAI Responses', path: '/v1/responses' },
    { value: 'anthropic_messages', label: 'Claude Messages', path: '/v1/messages' },
    { value: 'gemini_generate_content', label: 'Gemini generateContent', path: '/v1beta/models/{model}:generateContent' },
] as const;
type ModelTestEndpoint = (typeof ENDPOINT_OPTIONS)[number]['value'];

// 180s for every endpoint (was 30 for chat/responses/gemini): a thinking model
// (glm-5.2 / deepseek-reasoner) emits a long reasoning preamble before any content
// token, so a 30s probe died as "context deadline exceeded" on a working channel.
// Kept in sync with the channel-form test and the backend default (all 180s).
const DEFAULT_TIMEOUT_BY_ENDPOINT: Record<ModelTestEndpoint, number> = {
    openai_chat: 180,
    openai_responses: 180,
    anthropic_messages: 180,
    gemini_generate_content: 180,
};
const MAX_TIMEOUT_SECONDS = 300;

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function splitModels(value: string) {
    const seen = new Set<string>();
    return value
        .split(/[\n,]/)
        .map((item) => item.trim())
        .filter((item) => {
            if (!item) return false;
            const key = item.toLowerCase();
            if (seen.has(key)) return false;
            seen.add(key);
            return true;
        });
}

function clampInt(value: string, fallback: number, min: number, max: number) {
    const next = Math.trunc(Number(value));
    if (!Number.isFinite(next)) return fallback;
    return Math.min(max, Math.max(min, next));
}

function makeRandomMathPrompt() {
    const randomInt = (min: number, max: number) => {
        const span = max - min + 1;
        if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
            const values = new Uint32Array(1);
            crypto.getRandomValues(values);
            return min + (values[0] % span);
        }
        return min + Math.floor(Math.random() * span);
    };
    const left = randomInt(1000, 9999);
    const right = randomInt(1000, 9999);
    return `请只回答算式结果，不要解释：${left} + ${right} = ?`;
}

function shouldAutoRotatePrompt(value: string) {
    const trimmed = value.trim();
    return trimmed === '' || trimmed === LEGACY_DEFAULT_PROMPT || AUTO_MATH_PROMPT_RE.test(trimmed);
}

function formatDuration(ms?: number) {
    if (!ms) return '0ms';
    if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
    return `${ms}ms`;
}

function summarizeHTML(value: string): string | undefined {
    if (!/<\/?[a-z][\s\S]*>/i.test(value)) return undefined;
    const title = value.match(/<title[^>]*>([\s\S]*?)<\/title>/i)?.[1]
        || value.match(/<h1[^>]*>([\s\S]*?)<\/h1>/i)?.[1];
    const server = [...value.matchAll(/<center[^>]*>([\s\S]*?)<\/center>/gi)]
        .map((match) => match[1]?.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim())
        .filter(Boolean)
        .at(-1);
    const cleanTitle = title?.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
    if (cleanTitle && server && server !== cleanTitle) return `${cleanTitle} (${server})`;
    return cleanTitle || 'HTML error page returned by upstream proxy';
}

function displayError(value?: string) {
    if (!value) return '失败';
    return summarizeHTML(value) ?? value;
}

function ResultBadge({ success }: { success: boolean }) {
    return (
        <Badge
            variant={success ? 'secondary' : 'destructive'}
            className={success ? 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300' : undefined}
        >
            {success ? <CheckCircle2 className="size-3" /> : <XCircle className="size-3" />}
            {success ? '成功' : '失败'}
        </Badge>
    );
}

function SummaryCard({ label, value }: { label: string; value: string | number }) {
    return (
        <div className="min-w-0 rounded-2xl border border-border bg-card px-4 py-3">
            <div className="min-w-0 break-words text-xs text-muted-foreground">{label}</div>
            <div className="mt-1 min-w-0 break-words text-lg font-semibold tabular-nums">{value}</div>
        </div>
    );
}

function SingleModelPicker({
    value,
    options,
    onChange,
}: {
    value: string;
    options: string[];
    onChange: (value: string) => void;
}) {
    const selectValue = options.includes(value) ? value : '';

    return (
        <div className="grid gap-2">
            <select
                value={selectValue}
                onChange={(event) => onChange(event.target.value)}
                className="h-9 min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
            >
                <option value="">从模型池选择</option>
                {options.map((model) => (
                    <option key={model} value={model}>
                        {model}
                    </option>
                ))}
            </select>
            <Input
                value={value}
                onChange={(event) => onChange(event.target.value)}
                placeholder="也可以手输模型名"
                className="rounded-xl"
            />
        </div>
    );
}

function ResultOutputCell({ result }: { result: ModelTestResult }) {
    const text = result.success ? (result.response_preview || 'OK') : displayError(result.error);

    return (
        <div className="min-w-0 text-sm">
            <div
                className={cn(
                    'max-w-[22rem] whitespace-pre-wrap break-words md:max-w-[30rem]',
                    result.success ? 'text-foreground' : 'text-destructive'
                )}
            >
                {text}
            </div>
            {!result.success && result.error ? (
                <details className="mt-2">
                    <summary className="cursor-pointer text-xs text-muted-foreground">完整错误</summary>
                    <pre className="mt-2 max-h-56 max-w-[min(72vw,44rem)] overflow-auto whitespace-pre-wrap break-words rounded-xl border border-border bg-muted/30 p-2 text-xs text-foreground">
                        {result.error}
                    </pre>
                </details>
            ) : null}
            {result.error_code ? (
                <div className="mt-1 text-xs text-muted-foreground">code: {result.error_code}</div>
            ) : null}
        </div>
    );
}

function proxyLabel(proxy?: {
    proxy_used?: boolean;
    proxy_source?: string;
    proxy_scheme?: string;
    proxy_status?: number;
}) {
    if (!proxy?.proxy_used) return 'direct';
    const parts = ['proxy'];
    if (proxy.proxy_source) parts.push(proxy.proxy_source);
    if (proxy.proxy_scheme) parts.push(proxy.proxy_scheme);
    if (proxy.proxy_status) parts.push(`HTTP ${proxy.proxy_status}`);
    return parts.join(' ');
}

function AttemptsCell({ result }: { result: ModelTestResult }) {
    const attempts = result.attempts ?? [];
    if (attempts.length === 0) return <span className="text-muted-foreground">-</span>;

    return (
        <div className="grid min-w-[18rem] gap-1 text-xs">
            {attempts.slice(0, 3).map((attempt) => (
                <div key={attempt.attempt_num} className="flex min-w-0 items-start gap-2">
                    <span className={cn(
                        'mt-1.5 size-2 shrink-0 rounded-full',
                        attempt.status === 'success' ? 'bg-emerald-500' :
                            attempt.status === 'failed' ? 'bg-destructive' : 'bg-muted-foreground/40'
                    )} />
                    <span
                        className="min-w-0 whitespace-normal break-all font-mono leading-5"
                        title={`#${attempt.channel_id || '-'} ${attempt.channel_name || 'unknown'} / ${attempt.model_name || '-'}${attempt.upstream_path ? ` / ${attempt.upstream_path}` : ''}`}
                    >
                        #{attempt.channel_id || '-'} {attempt.channel_name || 'unknown'} / {attempt.model_name || '-'}{attempt.upstream_path ? ` / ${attempt.upstream_path}` : ''}
                    </span>
                    <span
                        className={cn(
                            'shrink-0 rounded-md border px-1.5 py-0.5 font-mono text-[10px]',
                            attempt.proxy_used ? 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300' : 'border-border bg-muted/40 text-muted-foreground'
                        )}
                        title={attempt.proxy_target || proxyLabel(attempt)}
                    >
                        {proxyLabel(attempt)}
                    </span>
                </div>
            ))}
            {attempts.length > 3 ? (
                <span className="text-muted-foreground">+{attempts.length - 3} 次</span>
            ) : null}
        </div>
    );
}

export function ModelTest() {
    const { data: channelModels = [] } = useModelChannelList();
    const { data: channels = [] } = useChannelList();
    const { data: plans = [] } = useAccessPlanList();
    const { data: users = [] } = useUserList();
    const { data: apiKeys = [] } = useAPIKeyList();
    const testMutation = useModelTest();
    const setActiveItem = useNavStore((state) => state.setActiveItem);
    const [singleModel, setSingleModel] = useState('');
    const [batchText, setBatchText] = useState('');
    const [accessPlanSlug, setAccessPlanSlug] = useState('');
    const [selectedChannelID, setSelectedChannelID] = useState<number | undefined>();
    const [selectedUserID, setSelectedUserID] = useState<number | undefined>();
    const [selectedAPIKeyID, setSelectedAPIKeyID] = useState<number | undefined>();
    const [endpoint, setEndpoint] = useState<ModelTestEndpoint>('openai_responses');
    const [prompt, setPrompt] = useState(() => makeRandomMathPrompt());
    const [streamTest, setStreamTest] = useState(true);
    const [concurrency, setConcurrency] = useState(4);
    const [timeoutSeconds, setTimeoutSeconds] = useState(DEFAULT_TIMEOUT_BY_ENDPOINT.openai_responses);
    const [response, setResponse] = useState<ModelTestResult[] | null>(null);
    const [summary, setSummary] = useState<{
        total: number;
        success: number;
        failed: number;
        duration_ms: number;
    } | null>(null);

    const modelOptions = useMemo(() => {
        const names = new Set<string>();
        channelModels.forEach((model) => {
            if (model.enabled) names.add(model.name);
        });
        return expandOneMillionModelAliases([...names]).sort((a, b) => a.localeCompare(b));
    }, [channelModels]);

    const channelOptions = useMemo(
        () => channels.map((item) => item.raw).sort((a, b) => a.name.localeCompare(b.name)),
        [channels]
    );

    const selectedChannel = useMemo(
        () => channelOptions.find((channel) => channel.id === selectedChannelID),
        [channelOptions, selectedChannelID]
    );

    const selectedChannelModels = useMemo(() => {
        if (!selectedChannel) return [];
        return getSelectedChannelModels(selectedChannel);
    }, [selectedChannel]);

    const effectiveModelOptions = selectedChannel ? selectedChannelModels : modelOptions;
    const batchModels = splitModels(batchText);
    const currentModelForStreamHint = singleModel.trim() || batchModels[0] || '';
    const forcedStreamHint = isStreamRequiredModel(currentModelForStreamHint) || (
        endpoint === 'openai_responses' && selectedChannel?.type === ChannelType.OpenAIResponse
    ) || (
        endpoint === 'anthropic_messages' && selectedChannel?.type === ChannelType.Anthropic && !!selectedChannel?.anthropic_context_1m
    );

    const apiKeysForSelectedUser = useMemo(() => {
        return apiKeys
            .filter((apiKey) => !selectedUserID || apiKey.user_id === selectedUserID)
            .sort((a, b) => a.name.localeCompare(b.name));
    }, [apiKeys, selectedUserID]);

    const selectedAPIKey = useMemo(() => {
        if (!selectedAPIKeyID) return undefined;
        return apiKeys.find((apiKey) => apiKey.id === selectedAPIKeyID);
    }, [apiKeys, selectedAPIKeyID]);

    const effectiveSelectedAPIKeyID = selectedAPIKey && (!selectedUserID || selectedAPIKey.user_id === selectedUserID)
        ? selectedAPIKeyID
        : undefined;

    const updateSelectedUser = (value: string) => {
        const nextUserID = Number(value) || undefined;
        setSelectedUserID(nextUserID);
        if (selectedAPIKey && nextUserID && selectedAPIKey.user_id !== nextUserID) {
            setSelectedAPIKeyID(undefined);
        }
    };

    const updateSelectedAPIKey = (value: string) => {
        const nextAPIKeyID = Number(value) || undefined;
        setSelectedAPIKeyID(nextAPIKeyID);
        const nextAPIKey = apiKeys.find((apiKey) => apiKey.id === nextAPIKeyID);
        if (nextAPIKey?.user_id) {
            setSelectedUserID(nextAPIKey.user_id);
        }
    };

    const updateEndpoint = (nextEndpoint: ModelTestEndpoint) => {
        setTimeoutSeconds((current) => {
            const previousDefault = DEFAULT_TIMEOUT_BY_ENDPOINT[endpoint];
            const nextDefault = DEFAULT_TIMEOUT_BY_ENDPOINT[nextEndpoint];
            return current === previousDefault ? nextDefault : current;
        });
        setEndpoint(nextEndpoint);
    };

    const updateSelectedChannel = (value: string) => {
        const nextChannelID = Number(value) || undefined;
        setSelectedChannelID(nextChannelID);
        const nextChannel = channelOptions.find((channel) => channel.id === nextChannelID);
        if (!nextChannel) return;
        const nextEndpoint = defaultModelTestEndpointForChannel(nextChannel.type) as ModelTestEndpoint;
        updateEndpoint(nextEndpoint);
        const nextModels = getSelectedChannelModels(nextChannel);
        if (nextModels.length > 0 && !nextModels.includes(singleModel)) {
            setSingleModel(nextModels[0]);
        }
    };

    const runTest = (modelsToTest: string[]) => {
        if (modelsToTest.length === 0) {
            toast.error('请选择模型');
            return;
        }

        const forcedStream = shouldForceTestStream({
            models: modelsToTest,
            endpoint,
            anthropicContext1M: selectedChannel?.anthropic_context_1m,
        });

        const effectivePrompt = shouldAutoRotatePrompt(prompt) ? makeRandomMathPrompt() : prompt;
        if (effectivePrompt !== prompt) {
            setPrompt(effectivePrompt);
        }

        testMutation.mutate(
            {
                models: modelsToTest,
                channel_id: selectedChannelID,
                access_plan_slug: accessPlanSlug || undefined,
                endpoint,
                prompt: effectivePrompt,
                stream: forcedStream ? true : streamTest,
                concurrency,
                timeout_seconds: timeoutSeconds,
                user_id: selectedUserID,
                api_key_id: effectiveSelectedAPIKeyID,
                // 管理员专用：测试结果一律写入日志，测完直接进日志页看完整记录
                audit_log: true,
            },
            {
                onSuccess: (data) => {
                    setResponse(data.results);
                    setSummary(data.summary);
                    toast.success(`测试完成：${data.summary.success}/${data.summary.total} 成功，正在打开日志`);
                    setActiveItem('log');
                },
                onError: (error) => {
                    toast.error('模型测试失败', { description: apiErrorMessage(error) });
                },
            }
        );
    };

    const isPending = testMutation.isPending;

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 pb-24 md:pb-4 rounded-t-3xl">
            <div className="grid min-w-0 gap-5 xl:grid-cols-[360px_minmax(0,1fr)]">
                <div className="min-w-0 space-y-5">
                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex min-w-0 items-center justify-between gap-3">
                            <h3 className="flex items-center gap-2 text-base font-semibold">
                                <FlaskConical className="size-5" />
                                模型测试
                            </h3>
                            <Badge variant="outline">{effectiveModelOptions.length}</Badge>
                        </div>

                        <div className="grid gap-4">
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">单个模型</span>
                                <SingleModelPicker
                                    value={singleModel}
                                    options={effectiveModelOptions}
                                    onChange={setSingleModel}
                                />
                            </label>

                            <Button
                                type="button"
                                className="rounded-xl"
                                disabled={isPending || singleModel.trim().length === 0}
                                onClick={() => runTest([singleModel.trim()])}
                            >
                                {isPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
                                单个测试
                            </Button>
                        </div>
                    </div>

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex min-w-0 items-center justify-between gap-3">
                            <h3 className="flex items-center gap-2 text-base font-semibold">
                                <Rows3 className="size-5" />
                                并发测试
                            </h3>
                            <Badge variant="outline">{batchModels.length}</Badge>
                        </div>

                        <div className="grid gap-4">
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">模型列表</span>
                                <textarea
                                    value={batchText}
                                    onChange={(event) => setBatchText(event.target.value)}
                                    placeholder="每行一个模型，或用英文逗号分隔"
                                    className="min-h-40 rounded-xl border border-input bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                />
                            </label>
                            <div className="grid grid-cols-2 gap-2">
                                <Button
                                    type="button"
                                    variant="outline"
                                    className="rounded-xl"
                                    disabled={effectiveModelOptions.length === 0}
                                    onClick={() => setBatchText(effectiveModelOptions.join('\n'))}
                                >
                                    全选当前模型
                                </Button>
                                <Button
                                    type="button"
                                    variant="outline"
                                    className="rounded-xl"
                                    disabled={batchText.length === 0}
                                    onClick={() => setBatchText('')}
                                >
                                    清空
                                </Button>
                            </div>
                            <Button
                                type="button"
                                className="rounded-xl"
                                disabled={isPending || batchModels.length === 0}
                                onClick={() => runTest(batchModels)}
                            >
                                {isPending ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
                                并发测试
                            </Button>
                        </div>
                    </div>
                </div>

                <div className="min-w-0 space-y-5">
                    <div className="rounded-2xl border border-border bg-card p-4">
                        <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-5">
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">渠道</span>
                                <SearchableSelect
                                    value={selectedChannelID != null ? String(selectedChannelID) : ''}
                                    onValueChange={updateSelectedChannel}
                                    options={[
                                        { value: '', label: '按路由自动选择' },
                                        ...channelOptions.map((channel) => ({
                                            value: String(channel.id),
                                            label: `#${channel.id} ${channel.name}`,
                                            keywords: channel.name,
                                        })),
                                    ]}
                                    placeholder="按路由自动选择"
                                    searchPlaceholder="搜索渠道名或 #ID…"
                                    emptyText="没有匹配的渠道"
                                    className="w-full"
                                />
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">用户</span>
                                <select
                                    value={selectedUserID ?? ''}
                                    onChange={(event) => updateSelectedUser(event.target.value)}
                                    className="h-9 min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="">不指定</option>
                                    {users.map((user) => (
                                        <option key={user.id} value={user.id}>
                                            {user.username}
                                        </option>
                                    ))}
                                </select>
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">API Key</span>
                                <select
                                    value={effectiveSelectedAPIKeyID ?? ''}
                                    onChange={(event) => updateSelectedAPIKey(event.target.value)}
                                    className="h-9 min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="">不指定</option>
                                    {apiKeysForSelectedUser.map((apiKey) => (
                                        <option key={apiKey.id} value={apiKey.id}>
                                            {apiKey.name}{apiKey.user_name ? ` · ${apiKey.user_name}` : ''}
                                        </option>
                                    ))}
                                </select>
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">方案</span>
                                <select
                                    value={accessPlanSlug}
                                    onChange={(event) => setAccessPlanSlug(event.target.value)}
                                    className="h-9 min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    <option value="">系统默认</option>
                                    {plans.filter((plan) => plan.enabled).map((plan) => (
                                        <option key={plan.id} value={plan.slug}>
                                            {plan.display_name || plan.slug}
                                        </option>
                                    ))}
                                </select>
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">端点</span>
                                <select
                                    value={endpoint}
                                    onChange={(event) => updateEndpoint(event.target.value as ModelTestEndpoint)}
                                    className="h-9 min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    {ENDPOINT_OPTIONS.map((item) => (
                                        <option key={item.value} value={item.value}>
                                            {item.label}
                                        </option>
                                    ))}
                                </select>
                                <span className="min-w-0 break-all text-xs text-muted-foreground">
                                    {ENDPOINT_OPTIONS.find((item) => item.value === endpoint)?.path}
                                </span>
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">测试方式</span>
                                <span className="inline-flex h-9 items-center gap-2 rounded-xl border border-border bg-background px-3 text-sm text-foreground">
                                    <Switch
                                        checked={forcedStreamHint || streamTest}
                                        onCheckedChange={setStreamTest}
                                        disabled={forcedStreamHint}
                                    />
                                    <span className="text-xs text-muted-foreground">{forcedStreamHint || streamTest ? '流式优先' : '非流式'}</span>
                                </span>
                                {forcedStreamHint ? (
                                    <span className="min-w-0 break-words text-xs text-primary">
                                        gpt-5.5 / Claude 1M 这类模型会强制流式测试；这是这类流式模型的正确链路。
                                    </span>
                                ) : null}
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">并发数</span>
                                <Input
                                    type="number"
                                    min={1}
                                    max={20}
                                    value={concurrency}
                                    onChange={(event) => setConcurrency(clampInt(event.target.value, 4, 1, 20))}
                                    className="rounded-xl"
                                />
                            </label>
                            <label className="grid gap-2 text-sm">
                                <span className="font-medium">超时秒数</span>
                                <Input
                                    type="number"
                                    min={1}
                                    max={MAX_TIMEOUT_SECONDS}
                                    value={timeoutSeconds}
                                    onChange={(event) => setTimeoutSeconds(clampInt(event.target.value, DEFAULT_TIMEOUT_BY_ENDPOINT[endpoint], 1, MAX_TIMEOUT_SECONDS))}
                                    className="rounded-xl"
                                />
                                <span className="min-w-0 break-words text-xs text-muted-foreground">
                                    Claude/cliproxy 慢模型建议 180-300 秒；只影响模型测试，不影响正式请求。
                                </span>
                            </label>
                        </div>
                        <label className="mt-4 grid gap-2 text-sm">
                            <span className="flex items-center justify-between gap-3 font-medium">
                                <span>测试提示词</span>
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    className="h-7 rounded-lg px-2 text-xs"
                                    onClick={() => setPrompt(makeRandomMathPrompt())}
                                >
                                    换一题
                                </Button>
                            </span>
                            <textarea
                                value={prompt}
                                onChange={(event) => setPrompt(event.target.value)}
                                className="min-h-20 rounded-xl border border-input bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                            <span className="text-xs text-muted-foreground">
                                默认每次测试自动换一组四位数加法，避免同渠道/同模型命中旧缓存或假阳性。
                            </span>
                        </label>
                    </div>

                    {summary ? (
                        <div className="grid gap-3 md:grid-cols-4">
                            <SummaryCard label="总数" value={summary.total} />
                            <SummaryCard label="成功" value={summary.success} />
                            <SummaryCard label="失败" value={summary.failed} />
                            <SummaryCard label="总耗时" value={formatDuration(summary.duration_ms)} />
                        </div>
                    ) : null}

                    <div className="rounded-3xl border border-border bg-card p-5">
                        <div className="mb-4 flex min-w-0 items-center justify-between gap-3">
                            <h3 className="text-base font-semibold">测试结果</h3>
                            {isPending ? <Loader2 className="size-4 animate-spin text-muted-foreground" /> : null}
                        </div>

                        {!response ? (
                            <div className="rounded-2xl border border-dashed border-border px-4 py-12 text-center text-sm text-muted-foreground">
                                暂无测试结果
                            </div>
                        ) : (
                            <div className="min-w-0 overflow-x-auto">
                                <Table className="min-w-[1180px] table-fixed">
                                    <TableHeader>
                                        <TableRow>
                                            <TableHead className="w-[220px]">模型</TableHead>
                                            <TableHead className="w-[120px]">状态</TableHead>
                                            <TableHead className="w-[180px]">渠道 / 上游</TableHead>
                                            <TableHead className="w-[90px]">耗时</TableHead>
                                            <TableHead className="w-[250px]">响应</TableHead>
                                            <TableHead className="w-[320px]">尝试</TableHead>
                                        </TableRow>
                                    </TableHeader>
                                    <TableBody>
                                        {response.map((result) => (
                                            <TableRow key={`${result.model}-${result.channel_id ?? 0}-${result.duration_ms}`}>
                                                <TableCell className="align-top">
                                                    <div className="min-w-0">
                                                        <div className="min-w-0 break-all font-mono text-sm font-semibold">{result.request_model}</div>
                                                        <div className="mt-1 flex min-w-0 flex-wrap gap-1">
                                                            {result.access_plan_slug ? (
                                                                <Badge variant="outline" className="max-w-[11rem]">
                                                                    <span className="truncate">{result.access_plan_slug}</span>
                                                                </Badge>
                                                            ) : null}
                                                            {result.request_endpoint ? (
                                                                <Badge variant="outline" className="max-w-[11rem]">
                                                                    <span className="truncate">{result.request_endpoint}</span>
                                                                </Badge>
                                                            ) : null}
                                                            {result.route_used ? <Badge variant="secondary">方案映射</Badge> : <Badge variant="outline">模型池</Badge>}
                                                            {result.route_fallback_used ? <Badge variant="outline">回落</Badge> : null}
                                                        </div>
                                                        {result.request_path ? (
                                                            <div className="mt-1 min-w-0 break-all font-mono text-xs text-muted-foreground">
                                                                {result.request_path}
                                                            </div>
                                                        ) : null}
                                                        {result.upstream_path ? (
                                                            <div className="mt-1 min-w-0 break-all font-mono text-xs text-muted-foreground">
                                                                upstream: {result.upstream_path}
                                                            </div>
                                                        ) : null}
                                                    </div>
                                                </TableCell>
                                                <TableCell className="align-top">
                                                    <div className="grid min-w-0 gap-2">
                                                        <ResultBadge success={result.success} />
                                                        {result.status_code ? (
                                                            <span className="text-xs text-muted-foreground">HTTP {result.status_code}</span>
                                                        ) : null}
                                                    </div>
                                                </TableCell>
                                                <TableCell className="align-top">
                                                    <div className="min-w-0 text-sm">
                                                        <div className="min-w-0 break-all font-medium" title={result.channel_name || '-'}>
                                                            {result.channel_name || '-'}
                                                        </div>
                                                        <div className="min-w-0 break-all font-mono text-xs text-muted-foreground" title={result.upstream_model || result.group_name || '-'}>
                                                            {result.upstream_model || result.group_name || '-'}
                                                        </div>
                                                    </div>
                                                </TableCell>
                                                <TableCell className="align-top tabular-nums">{formatDuration(result.duration_ms)}</TableCell>
                                                <TableCell className="align-top">
                                                    <ResultOutputCell result={result} />
                                                </TableCell>
                                                <TableCell className="align-top">
                                                    <AttemptsCell result={result} />
                                                </TableCell>
                                            </TableRow>
                                        ))}
                                    </TableBody>
                                </Table>
                            </div>
                        )}
                    </div>
                </div>
            </div>
        </PageWrapper>
    );
}
