'use client';

import { useEffect, useRef, useState } from 'react';
import { Check, Copy, Eye, EyeOff, KeyRound, RefreshCw, Trash2 } from 'lucide-react';
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

// 专给 AI / 脚本 / CLI 直连后台用的长效管理员令牌。
// 后端已存令牌时 setting/list 只回 SECRET_MASK（非空），明文不回读；
// 因此这里“已设置”只做状态提示，复制只在刚生成、内存里还握着明文时有效。
export function SettingAccessToken() {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [token, setToken] = useState('');
    const [reveal, setReveal] = useState(false);
    const [stored, setStored] = useState(false);
    const [dirty, setDirty] = useState(false);
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
        setToken(next);
        plaintextRef.current = next;
        setReveal(true);
        setDirty(false);
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
        const value = plaintextRef.current || token.trim();
        if (!value) {
            toast.error('明文已隐藏无法回读，请重新生成以获取新令牌');
            return;
        }
        try {
            await navigator.clipboard.writeText(value);
            toast.success('已复制到剪贴板');
        } catch {
            toast.error('复制失败，请手动选中输入框内容');
        }
    };

    const handleClear = () => {
        setToken('');
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

            {stored && (
                <div className="flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
                    <Check className="size-3.5 shrink-0" />
                    已启用：带此令牌的请求即拥有管理员权限
                </div>
            )}
        </div>
    );
}
