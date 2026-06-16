'use client';

import { useState, useSyncExternalStore } from 'react';
import { BookOpen, KeyRound, Loader2, ShieldCheck, Terminal, Ticket } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { SettingAPIKey } from '@/components/modules/setting/APIKey';
import { Input } from '@/components/ui/input';
import { useRedeemCode } from '@/api/endpoints/redeem';
import { useAuthStore } from '@/api/endpoints/user';
import { toast } from '@/components/common/Toast';
import { CopyIconButton } from '@/components/common/CopyButton';
import type { ApiError } from '@/api/types';

function getBrowserBaseUrl(): string {
    if (typeof window === 'undefined') return 'https://your-octopus.example';
    return window.location.origin;
}

function RedeemBox() {
    const [code, setCode] = useState('');
    const redeem = useRedeemCode();

    const submit = () => {
        const value = code.trim();
        if (!value) return;
        redeem.mutate(value, {
            onSuccess: (user) => {
                useAuthStore.setState({ user });
                setCode('');
                toast.success('兑换成功');
            },
            onError: (error) => {
                toast.error('兑换失败', { description: (error as unknown as ApiError)?.message });
            },
        });
    };

    return (
        <section className="rounded-3xl border border-border bg-card p-5">
            <div className="flex items-start justify-between gap-4">
                <div>
                    <div className="flex items-center gap-2">
                        <Ticket className="size-5" />
                        <h2 className="text-base font-semibold">兑换码</h2>
                    </div>
                    <p className="mt-2 text-sm text-muted-foreground">兑换管理员发放的额度或月卡。</p>
                </div>
            </div>
            <div className="mt-4 flex flex-col gap-2 sm:flex-row">
                <Input
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                    placeholder="输入兑换码"
                    className="rounded-xl"
                />
                <button
                    type="button"
                    onClick={submit}
                    disabled={redeem.isPending || !code.trim()}
                    className="h-10 shrink-0 rounded-xl bg-primary px-4 text-sm font-medium text-primary-foreground disabled:opacity-50"
                >
                    {redeem.isPending ? <Loader2 className="size-4 animate-spin" /> : '兑换'}
                </button>
            </div>
        </section>
    );
}

function APIKeyIntro({ isAdmin }: { isAdmin: boolean }) {
    return (
        <section className="rounded-3xl border border-border bg-card p-5 md:p-6">
            <div className="flex min-w-0 flex-wrap items-start justify-between gap-4">
                <div className="min-w-0 max-w-2xl">
                    <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                        <KeyRound className="size-3.5" />
                        API Key
                    </div>
                    <h2 className="text-xl font-bold md:text-2xl">{isAdmin ? 'API Key 管理' : '我的 API Key'}</h2>
                    <p className="mt-2 text-sm leading-6 text-muted-foreground">
                        {isAdmin
                            ? '管理员可在这里集中管理所有用户密钥、端点协议、可用方案和限额。'
                            : '在这里创建和复制自己的密钥，按需选择 OpenAI / Gemini / Anthropic 协议端点。'}
                    </p>
                </div>
                <div className="min-w-0 rounded-2xl border border-border bg-muted/20 px-4 py-3 text-sm text-muted-foreground">
                    <div className="flex items-center gap-2 font-medium text-foreground">
                        <ShieldCheck className="size-4" />
                        独立页面
                    </div>
                    <p className="mt-1 text-xs">不再混在系统设置里。</p>
                </div>
            </div>
        </section>
    );
}

function BasicUsageBox() {
    const baseUrl = useSyncExternalStore(
        () => () => undefined,
        getBrowserBaseUrl,
        () => 'https://your-octopus.example'
    );
    const example = `curl "${baseUrl}/v1/chat/completions" \\
  -H "Authorization: Bearer sk-octopus-..." \\
  -H "Content-Type: application/json" \\
  -d '{"model":"你的模型","messages":[{"role":"user","content":"Hello"}]}'`;

    return (
        <section className="rounded-lg border border-border bg-card p-4 md:p-5">
            <div className="flex min-w-0 flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                    <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                        <BookOpen className="size-4 text-muted-foreground" />
                        基础使用方法
                    </div>
                    <p className="mt-1 text-sm leading-6 text-muted-foreground">
                        创建密钥后，点密钥右侧的“使用教程”按钮，会按允许协议和方案生成可复制示例。
                    </p>
                </div>
                <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/30 px-3 py-2 text-xs">
                    <span className="text-muted-foreground">Base URL</span>
                    <code className="font-mono text-foreground">{baseUrl}</code>
                    <CopyIconButton
                        text={baseUrl}
                        className="flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-background hover:text-foreground"
                        copyIconClassName="size-3.5"
                        checkIconClassName="size-3.5"
                    />
                </div>
            </div>
            <div className="mt-3 overflow-hidden rounded-lg border border-border/70">
                <div className="flex items-center justify-between gap-3 bg-muted/30 px-3 py-2">
                    <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground">
                        <Terminal className="size-3.5" />
                        OpenAI-compatible
                    </div>
                    <CopyIconButton
                        text={example}
                        className="flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-background hover:text-foreground"
                        copyIconClassName="size-3.5"
                        checkIconClassName="size-3.5"
                    />
                </div>
                <pre className="overflow-x-auto bg-zinc-950 p-3 text-xs leading-5 text-zinc-100"><code>{example}</code></pre>
            </div>
            <p className="mt-2 text-xs leading-5 text-muted-foreground">
                多方案密钥可在请求头加 <code className="rounded bg-muted px-1 py-0.5">X-Octopus-Plan</code>；缓存命中会在日志和密钥统计里显示。
            </p>
        </section>
    );
}

export function Key() {
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <APIKeyIntro key="apikey-intro" isAdmin={isAdmin} />
            {!isAdmin && <BasicUsageBox key="basic-usage" />}
            {!isAdmin && <RedeemBox key="redeem-box" />}
            <div key="apikey-panel" className="min-h-[560px] min-w-0">
                <SettingAPIKey fullPage />
            </div>
        </PageWrapper>
    );
}
