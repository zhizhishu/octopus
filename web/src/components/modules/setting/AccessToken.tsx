'use client';

import { useRef, useState } from 'react';
import { Check, CircleAlert, Copy, Eye, EyeOff, Info, KeyRound, RefreshCw, Trash2 } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import {
    useSettingList,
    useSetSetting,
    fetchSettingSecret,
    SettingKey,
    type SettingSecret,
} from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';

const MIN_LEN = 24;

// 遮罩只是「这里存着一串东西」的视觉占位，不是真值；真值只从 /setting/secret 现读。
const MASK = '•'.repeat(24);

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
        try {
            ta.focus();
            ta.select();
            return document.execCommand('copy');
        } finally {
            document.body.removeChild(ta);
        }
    } catch {
        return false;
    }
}

function sourceHint(secret: SettingSecret): string {
    return secret.source === 'env'
        ? '令牌由环境变量提供，请到部署配置中管理'
        : '当前未设置管理员访问令牌，请先点「生成」';
}

// 专给 AI / 脚本 / CLI 直连后台用的长效管理员令牌。
// 版式：一行输入框 + 行内眼睛（默认遮罩，点开现读明文）/ 第二行生成·复制·清除 / 第三行状态。
// 明文只存在本组件内存里：不写 localStorage / sessionStorage / URL / console，离开页面即丢。
export function SettingAccessToken() {
    // 复制被拦下时用它兜底选中令牌（Input 未透传 ref，所以从容器里取）。
    const fieldRef = useRef<HTMLDivElement | null>(null);
    const { data: settings, isError: listError } = useSettingList();
    const setSetting = useSetSetting();

    // 草稿：用户手动输入的内容（null = 未进入编辑态，输入框显示遮罩或已读明文）。
    const [draft, setDraft] = useState<string | null>(null);
    // 后端现读到的明文 / 刚生成并保存成功的明文，本页唯一的复制来源。
    const [plain, setPlain] = useState('');
    const [reveal, setReveal] = useState(false);
    // 明文来源：null = 本页还没读过（不要凭空猜是 env 还是没设置）。
    const [source, setSource] = useState<SettingSecret['source'] | null>(null);
    const [readFailed, setReadFailed] = useState(false);
    const saving = setSetting.isPending;
    // 「已启用」只认 setting/list 的非空回执（密钥类固定回遮罩串，非空即已存），
    // 直接由查询结果推导，绝不乐观置位。
    const stored = !!(settings?.find((x) => x.key === SettingKey.AdminAccessToken)?.value);

    // 换值/清空时旧明文立刻作废，避免复制到一个已经不在生效的令牌。
    const forgetPlain = () => {
        setPlain('');
        setReveal(false);
    };

    // 明文只有一个来源：受保护的 /setting/secret。env/none 只回 source、不回明文。
    // 失败时已 toast，返回 null 让调用方保持遮罩，绝不显示假值。
    const readSecret = async (): Promise<SettingSecret | null> => {
        try {
            const secret = await fetchSettingSecret(SettingKey.AdminAccessToken);
            setSource(secret.source);
            setReadFailed(false);
            if (secret.source === 'setting' && secret.value) setPlain(secret.value);
            return secret;
        } catch (e) {
            setReadFailed(true);
            toast.error(e instanceof Error ? e.message : '读取令牌失败');
            return null;
        }
    };

    const handleToggleReveal = async () => {
        if (reveal) {
            setReveal(false); // 收回遮罩；明文仍留在内存，再点开不必重读
            return;
        }
        if (plain) {
            setReveal(true);
            return;
        }
        const secret = await readSecret();
        if (!secret) return;
        if (secret.source === 'setting' && secret.value) {
            setReveal(true);
            return;
        }
        toast.info(sourceHint(secret));
    };

    const handleGenerate = () => {
        const next = generateToken();
        setSetting.mutate(
            { key: SettingKey.AdminAccessToken, value: next },
            {
                onSuccess: () => {
                    // 服务端确认保存成功才展示：等价于已展开，用户可立刻复制。
                    setDraft(null);
                    setPlain(next);
                    setReveal(true);
                    setSource('setting');
                    toast.success('已生成并启用，请立刻复制保存');
                },
                onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
            }
        );
    };

    const handleSaveManual = () => {
        const value = (draft ?? '').trim();
        if (!value) return;
        if (value.length < MIN_LEN) {
            toast.error(`令牌至少 ${MIN_LEN} 位`);
            return;
        }
        setSetting.mutate(
            { key: SettingKey.AdminAccessToken, value },
            {
                onSuccess: () => {
                    // 旧明文作废，复制必须拿到刚保存的这一个。
                    setDraft(null);
                    setPlain(value);
                    setReveal(true);
                    setSource('setting');
                    toast.success('已保存并启用');
                },
                onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
            }
        );
    };

    const handleCopy = async () => {
        // 真值优先用刚生成/刚读到的明文；都没有就先现读一次 —— 绝不复制遮罩或未保存草稿。
        let value = plain;
        if (!value) {
            const secret = await readSecret();
            if (!secret) return;
            if (secret.source !== 'setting' || !secret.value) {
                toast.info(sourceHint(secret));
                return;
            }
            value = secret.value;
        }
        if (await copyText(value)) {
            toast.success('已复制到剪贴板');
            return;
        }
        // 到这里说明自动复制被浏览器拦下了（http:// 内网 IP 部署没有异步剪贴板，
        // 只剩 execCommand，而它要求真实用户手势）。这时至少替用户把令牌选中：
        // 「点了按钮没反应」比一句提示更糟，选中后 Ctrl/⌘+C 就能拿到真值。
        setReveal(true);
        requestAnimationFrame(() => {
            const input = fieldRef.current?.querySelector('input');
            input?.focus();
            input?.select();
        });
        toast.error('浏览器拦下了自动复制，已替你选中令牌，按 Ctrl/⌘+C 即可');
    };

    const handleClear = () => {
        setSetting.mutate(
            { key: SettingKey.AdminAccessToken, value: '' },
            {
                onSuccess: () => {
                    setDraft(null);
                    forgetPlain();
                    setSource('none');
                    toast.success('已清除，令牌直连已禁用');
                },
                onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
            }
        );
    };

    const draftValue = draft ?? '';
    const shortDraft = draft !== null && draftValue.trim().length < MIN_LEN;
    const display = draft !== null ? draftValue : reveal ? plain : stored ? MASK : '';
    const editing = draft !== null;

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
                    即获管理员权限，不必再走浏览器登录态。至少 {MIN_LEN} 位；留空即禁用（空值不是后门）。
                </p>
            </div>

            {/* 第一行：当前密钥占满一行，右端行内眼睛。默认遮罩，点开现读真值。 */}
            <div className="relative min-w-0" ref={fieldRef}>
                <Input
                    type={editing || reveal ? 'text' : 'password'}
                    value={display}
                    onChange={(e) => setDraft(e.target.value)}
                    placeholder={stored ? '已设置（点右侧眼睛查看当前令牌）' : '未设置（点「生成」，或手动粘贴 ≥24 位）'}
                    aria-label="管理员访问令牌"
                    className="rounded-xl pr-11 font-mono"
                />
                <button
                    type="button"
                    onClick={handleToggleReveal}
                    className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                    aria-label={reveal ? '隐藏令牌' : '显示令牌'}
                    title={reveal ? '隐藏令牌' : '显示当前令牌明文'}
                >
                    {reveal ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                </button>
            </div>

            {/* 第二行：操作按钮。生成 / 复制为主按钮，清除弱化，草稿才有保存。 */}
            <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                    <Button type="button" onClick={handleGenerate} disabled={saving} className="h-9 rounded-xl">
                        <RefreshCw className="size-3.5" />
                        {stored ? '重新生成' : '生成'}
                    </Button>
                    {editing && (
                        <Button
                            type="button"
                            variant="outline"
                            onClick={handleSaveManual}
                            disabled={saving || shortDraft}
                            title={shortDraft ? `令牌至少 ${MIN_LEN} 位` : '保存并启用'}
                            className="h-9 rounded-xl"
                        >
                            <Check className="size-3.5" />
                            保存
                            {shortDraft && <span className="text-[10px] text-muted-foreground">≥{MIN_LEN} 位</span>}
                        </Button>
                    )}
                    <Button type="button" onClick={handleCopy} disabled={saving} className="h-9 rounded-xl">
                        <Copy className="size-3.5" />
                        复制
                    </Button>
                </div>
                {stored && (
                    <Button
                        type="button"
                        variant="ghost"
                        onClick={handleClear}
                        disabled={saving}
                        className="h-9 rounded-xl text-muted-foreground hover:text-destructive"
                    >
                        <Trash2 className="size-3.5" />
                        清除
                    </Button>
                )}
            </div>

            {/* 第三行：状态。已启用只认 setting/list 的非空回执。 */}
            {readFailed || listError ? (
                <div className="flex items-center gap-1.5 text-xs text-destructive">
                    <CircleAlert className="size-3.5 shrink-0" />
                    读取失败
                </div>
            ) : stored ? (
                <div className="flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
                    <Check className="size-3.5 shrink-0" />
                    已启用：带此令牌的请求即拥有管理员权限
                </div>
            ) : source === 'env' ? (
                <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <Info className="size-3.5 shrink-0" />
                    由环境变量提供：请到部署配置中管理
                </div>
            ) : (
                <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <Info className="size-3.5 shrink-0" />
                    未设置
                </div>
            )}
        </div>
    );
}
