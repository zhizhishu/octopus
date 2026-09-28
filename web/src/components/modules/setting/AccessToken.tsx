'use client';

import { useEffect, useRef, useState } from 'react';
import { Check, Copy, Eye, EyeOff, KeyRound, RefreshCw, Trash2, X } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';

const MIN_LEN = 24;

// 客户端生成一个高熵管理员令牌（浏览器 crypto，明文只此一次可见）。
function generateToken(): string {
    const bytes = new Uint8Array(24);
    crypto.getRandomValues(bytes);
    return 'oct_' + Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('');
}

// 复制文本：优先 navigator.clipboard（仅安全上下文可用），不可用或失败时退回
// 隐藏 textarea + execCommand('copy')，保证 http://内网IP 部署也能一键复制。
async function copyText(value: string): Promise<boolean> {
    try {
        if (navigator.clipboard && window.isSecureContext) {
            await navigator.clipboard.writeText(value);
            return true;
        }
    } catch {
        // 落到 execCommand 回退
    }
    try {
        const ta = document.createElement('textarea');
        ta.value = value;
        ta.setAttribute('readonly', '');
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        const ok = document.execCommand('copy');
        document.body.removeChild(ta);
        return ok;
    } catch {
        return false;
    }
}

// 专给 AI / 脚本 / CLI 直连后台用的长效管理员令牌。
// 后端已存令牌时 setting/list 只回 SECRET_MASK（非空），明文不回读；
// 因此“已设置”只做状态提示，明文只在刚生成时经一次性面板展示。
export function SettingAccessToken() {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [token, setToken] = useState('');
    const [reveal, setReveal] = useState(false);
    const [stored, setStored] = useState(false);
    const [dirty, setDirty] = useState(false);
    // 刚生成的明文：独立于 settings 查询的状态，列表重取/30s 轮询都不会清掉它，
    // 只有用户关闭面板或清除令牌才消失（后端永不回读明文，这是唯一可见窗口）。
    const [freshToken, setFreshToken] = useState('');
    const plaintextRef = useRef('');

    useEffect(() => {
        if (!settings) return;
        const s = settings.find((x) => x.key === SettingKey.AdminAccessToken);
        setStored(!!(s && s.value));
        setToken('');
        setDirty(false);
        plaintextRef.current = '';
    }, [settings]);

    const persist = (value: string, okMsg: string) => {
        setSetting.mutate(
            { key: SettingKey.AdminAccessToken, value },
            {
                onSuccess: () => toast.success(okMsg),
                onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
            }
        );
    };

    const handleGenerate = () => {
        const next = generateToken();
        setFreshToken(next);
        setStored(true);
        persist(next, '已生成并启用，请立刻复制保存');
    };

    const handleSaveManual = () => {
        const value = token.trim();
        setDirty(false);
        if (!value) return;
        if (value.length < MIN_LEN) {
            toast.error(`令牌至少 ${MIN_LEN} 位`);
            return;
        }
        plaintextRef.current = value;
        setStored(true);
        persist(value, '已保存并启用');
    };

    const handleCopy = async () => {
        const value = freshToken || plaintextRef.current || token.trim();
        if (!value) {
            toast.error('明文已隐藏无法回读，请重新生成以获取新令牌');
            return;
        }
        if (await copyText(value)) {
            toast.success('已复制到剪贴板');
        } else {
            toast.error('复制失败，请点击令牌文本全选后手动复制');
        }
    };

    const handleClear = () => {
        setToken('');
        setFreshToken('');
        plaintextRef.current = '';
        setStored(false);
        setDirty(false);
        persist('', '已清除，令牌直连已禁用');
    };

    return (
        <div className="min-w-0 space-y-4 rounded-3xl border border-border bg-card p-4 sm:p-6">
            <div className="min-w-0 space-y-1">
                <h2 className="flex min-w-0 items-center gap-2 text-lg font-bold text-card-foreground">
                    <KeyRound className="h-5 w-5 shrink-0" />
                    管理员访问令牌
                </h2>
                <p className="text-xs leading-5 text-muted-foreground">
                    给 AI / 脚本 / CLI 直连后台用：请求头带{' '}
                    <span className="rounded bg-background px-1 font-mono">Authorization: Bearer &lt;令牌&gt;</span>{' '}
                    即获管理员权限，不必再走浏览器登录态。至少 {MIN_LEN} 位；留空即禁用（空值不是后门）。生成后请立刻复制保存，之后只保留隐藏态、无法再回读明文。
                </p>
            </div>

            <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                <div className="relative flex-1">
                    <Input
                        type={reveal ? 'text' : 'password'}
                        value={token}
                        onChange={(e) => {
                            setToken(e.target.value);
                            setDirty(true);
                        }}
                        onBlur={() => dirty && handleSaveManual()}
                        placeholder={stored ? '已设置（已隐藏，可重新生成覆盖）' : '点右侧「生成」，或手动粘贴 ≥24 位令牌'}
                        className="rounded-xl pr-10 font-mono"
                    />
                    <button
                        type="button"
                        onClick={() => setReveal((v) => !v)}
                        className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                        aria-label={reveal ? '隐藏' : '显示'}
                    >
                        {reveal ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                    </button>
                </div>
                <div className="flex shrink-0 gap-2">
                    <Button type="button" variant="outline" size="sm" onClick={handleGenerate} className="h-9 rounded-xl">
                        <RefreshCw className="size-3.5" />
                        生成
                    </Button>
                    <Button type="button" variant="outline" size="sm" onClick={handleCopy} className="h-9 rounded-xl">
                        <Copy className="size-3.5" />
                        复制
                    </Button>
                    {stored && (
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={handleClear}
                            className="h-9 rounded-xl text-muted-foreground hover:text-destructive"
                        >
                            <Trash2 className="size-3.5" />
                            清除
                        </Button>
                    )}
                </div>
            </div>

            {freshToken && (
                <div className="space-y-2 rounded-2xl border border-emerald-500/40 bg-emerald-500/5 p-3 sm:p-4">
                    <div className="flex items-center justify-between gap-2">
                        <p className="text-xs font-medium text-emerald-700 dark:text-emerald-400">
                            新令牌已生成 —— 仅此一次显示，请立即复制保存
                        </p>
                        <button
                            type="button"
                            onClick={() => setFreshToken('')}
                            aria-label="关闭"
                            className="shrink-0 rounded-md text-muted-foreground hover:text-foreground"
                        >
                            <X className="size-4" />
                        </button>
                    </div>
                    <div className="flex items-center gap-2">
                        <code className="min-w-0 flex-1 break-all rounded-xl bg-background px-3 py-2 font-mono text-sm select-all">
                            {freshToken}
                        </code>
                        <Button type="button" size="sm" onClick={handleCopy} className="h-9 shrink-0 rounded-xl">
                            <Copy className="size-3.5" />
                            复制
                        </Button>
                    </div>
                </div>
            )}

            {stored && (
                <div className="flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
                    <Check className="size-3.5 shrink-0" />
                    已启用：带此令牌的请求即拥有管理员权限
                </div>
            )}
        </div>
    );
}
