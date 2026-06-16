'use client';

import { useState } from 'react';
import { CalendarDays, Check, ChevronDown, History, Loader2, MapPin, Plus, RotateCcw, ShieldCheck, Ticket, Trash2, UserCog, WalletCards } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { Progress } from '@/components/ui/progress';
import { Switch } from '@/components/ui/switch';
import { CopyIconButton } from '@/components/common/CopyButton';
import { toast } from '@/components/common/Toast';
import type { ApiError } from '@/api/types';
import {
    useCreateUser,
    useDeleteUser,
    useResetUserPassword,
    useUpdateUser,
    useUserList,
    type User,
    type UserRole,
    type UserStatus,
} from '@/api/endpoints/user';
import { useAccessPlanList, type AccessPlan } from '@/api/endpoints/access-plan';
import {
    useDeleteRedeemCode,
    useGenerateRedeemCode,
    useRedeemCodeList,
    useUpdateRedeemCode,
    type RedeemCodeType,
} from '@/api/endpoints/redeem';
import { useUserUsageRank } from '@/api/endpoints/stats';
import { cn } from '@/lib/utils';
import { MonoSafeText, SafeText } from '@/components/common/SafeText';

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function formatMoney(value: number | undefined) {
    return `$${Number(value ?? 0).toFixed(6)}`;
}

const DAY_SECONDS = 24 * 60 * 60;

function formatDate(ts?: number) {
    if (!ts) return '未设置';
    return new Date(ts * 1000).toLocaleDateString();
}

function formatDateTime(ts?: number) {
    if (!ts) return '暂无';
    return new Date(ts * 1000).toLocaleString();
}

function emptyText(value?: string) {
    return value?.trim() || '暂无';
}

function accessPlanLabel(plan: AccessPlan) {
    return plan.display_name?.trim() || plan.slug || `#${plan.id}`;
}

function togglePlanID(current: number[], id: number) {
    return current.includes(id) ? current.filter((item) => item !== id) : [...current, id];
}

function nowUnix() {
    return Math.floor(Date.now() / 1000);
}

function nextLocalMidnightUnix() {
    const date = new Date();
    date.setHours(24, 0, 0, 0);
    return Math.floor(date.getTime() / 1000);
}

function unixToDateInput(ts?: number) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');
    return `${year}-${month}-${day}`;
}

function dateInputToEndOfDayUnix(value: string) {
    if (!value) return 0;
    const [year, month, day] = value.split('-').map(Number);
    if (!year || !month || !day) return 0;
    return Math.floor(new Date(year, month - 1, day, 23, 59, 59).getTime() / 1000);
}

function expireByDays(days: number) {
    if (!Number.isFinite(days) || days <= 0) return 0;
    return nowUnix() + Math.ceil(days) * DAY_SECONDS;
}

function safeNumber(value: number | undefined) {
    return Number.isFinite(Number(value)) ? Number(value) : 0;
}

function daysRemainingLabel(ts?: number) {
    if (!ts) return '未设置有效期';
    const seconds = ts - nowUnix();
    if (seconds <= 0) return '已过期';
    return `还剩 ${Math.ceil(seconds / DAY_SECONDS)} 天`;
}

function formatResetLabel(ts?: number, limit?: number) {
    if (!ts && safeNumber(limit) > 0) return `${formatDateTime(nextLocalMidnightUnix())}（保存时写入）`;
    if (!ts) return '未设置';
    const suffix = ts <= nowUnix() ? '（待重置）' : '';
    return `${formatDateTime(ts)}${suffix}`;
}

function monthlySummary(user: Pick<User, 'monthly_limit' | 'monthly_used' | 'monthly_expire_at' | 'monthly_reset_at'>) {
    const limit = Math.max(0, safeNumber(user.monthly_limit));
    const used = Math.max(0, safeNumber(user.monthly_used));
    const remaining = Math.max(limit - used, 0);
    const expireAt = safeNumber(user.monthly_expire_at);
    const resetAt = safeNumber(user.monthly_reset_at);
    const configured = limit > 0 || expireAt > 0 || used > 0;
    const expired = expireAt > 0 && expireAt <= nowUnix();
    const exhausted = limit > 0 && used >= limit;
    const active = configured && limit > 0 && !expired;

    return {
        limit,
        used,
        remaining,
        expireAt,
        resetAt,
        configured,
        expired,
        exhausted,
        active,
        progress: limit > 0 ? Math.min(100, (used / limit) * 100) : 0,
        status: !configured ? '未开通' : expired ? '已过期' : exhausted ? '今日已用尽' : active ? '生效中' : '待配置',
    };
}

function UserRow({
    user,
    plans,
    active,
    onSelect,
}: {
    user: User;
    plans: AccessPlan[];
    active: boolean;
    onSelect: () => void;
}) {
    const monthly = monthlySummary(user);
    const userPlanIDs = user.access_plan_ids ?? user.access_plans?.map((plan) => plan.id) ?? [];
    const planLabel = userPlanIDs.length > 0
        ? userPlanIDs
            .map((id) => accessPlanLabel(plans.find((plan) => plan.id === id) ?? user.access_plans?.find((plan) => plan.id === id) ?? ({ id, slug: `#${id}`, display_name: '' } as AccessPlan)))
            .join(' / ')
        : '系统默认';

    return (
        <button
            type="button"
            onClick={onSelect}
            className={cn(
                'w-full rounded-2xl border px-4 py-3 text-left transition-colors',
                active ? 'border-primary/40 bg-primary/10' : 'border-border bg-card hover:bg-muted/30'
            )}
        >
            <div className="flex items-center gap-2">
                <SafeText value={user.username} className="flex-1 text-sm font-semibold" />
                <Badge variant={user.role === 'admin' ? 'default' : 'outline'}>{user.role}</Badge>
                <Badge variant={user.status === 'active' ? 'secondary' : 'destructive'}>
                    {user.status === 'active' ? '启用' : '停用'}
                </Badge>
            </div>
            <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground">
                <span>余额 {formatMoney(user.balance)}</span>
                <span>每日额度 {formatMoney(monthly.limit)}</span>
                <span>今日已用 {formatMoney(monthly.used)}</span>
                <span>今日剩余 {formatMoney(monthly.remaining)}</span>
                <span>到期 {formatDate(monthly.expireAt)}</span>
                <span>{daysRemainingLabel(monthly.expireAt)}</span>
                <span>下次重置 {formatResetLabel(monthly.resetAt, monthly.limit)}</span>
                <SafeText value={`方案 ${planLabel}`} />
            </div>
            <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground">
                <span className="inline-flex min-w-0 items-center gap-1">
                    <MapPin className="size-3.5 shrink-0" />
                    注册 IP <MonoSafeText value={emptyText(user.register_ip)} className="max-w-[8rem]" />
                </span>
                <span className="inline-flex min-w-0 items-center gap-1">
                    <History className="size-3.5 shrink-0" />
                    调用 IP <MonoSafeText value={emptyText(user.last_relay_ip)} className="max-w-[8rem]" />
                </span>
            </div>
        </button>
    );
}

function UserEditor({ user, accessPlans }: { user: User | null; accessPlans: AccessPlan[] }) {
    const updateUser = useUpdateUser();
    const resetPassword = useResetUserPassword();
    const deleteUser = useDeleteUser();
    const [draft, setDraft] = useState<User | null>(user);
    const [newPassword, setNewPassword] = useState('');

    if (!draft) {
        return (
            <div className="flex h-full items-center justify-center rounded-3xl border border-border bg-card p-8 text-sm text-muted-foreground">
                选择一个用户后编辑额度、月卡和权限
            </div>
        );
    }

    const monthly = monthlySummary(draft);
    const expireDaysValue = monthly.expireAt ? Math.max(0, Math.ceil((monthly.expireAt - nowUnix()) / DAY_SECONDS)) : '';
    const enabledPlans = accessPlans.filter((plan) => plan.enabled);
    const defaultEnabledPlan = enabledPlans.find((plan) => plan.is_default) ?? enabledPlans[0];
    const selectedPlanIDs = draft.access_plan_ids ?? draft.access_plans?.map((plan) => plan.id) ?? (defaultEnabledPlan ? [defaultEnabledPlan.id] : []);
    const selectedPlans = enabledPlans.filter((plan) => selectedPlanIDs.includes(plan.id));
    const defaultPlanID = draft.default_access_plan_id && selectedPlanIDs.includes(draft.default_access_plan_id)
        ? draft.default_access_plan_id
        : selectedPlanIDs[0];

    const setAccessPlanIDs = (ids: number[]) => {
        const fallbackIDs = ids.length > 0 ? ids : defaultEnabledPlan ? [defaultEnabledPlan.id] : [];
        setDraft({
            ...draft,
            access_plan_ids: fallbackIDs,
            default_access_plan_id: draft.default_access_plan_id && fallbackIDs.includes(draft.default_access_plan_id)
                ? draft.default_access_plan_id
                : fallbackIDs[0],
        });
    };

    const setDailyLimit = (value: number) => {
        setDraft({
            ...draft,
            monthly_limit: Math.max(0, safeNumber(value)),
            monthly_reset_at: draft.monthly_reset_at || nextLocalMidnightUnix(),
        });
    };

    const setExpireDays = (value: number) => {
        setDraft({
            ...draft,
            monthly_expire_at: expireByDays(value),
            monthly_reset_at: draft.monthly_reset_at || nextLocalMidnightUnix(),
        });
    };

    const save = () => {
        const monthlyLimit = Math.max(0, safeNumber(draft.monthly_limit));
        const monthlyResetAt = Math.max(0, safeNumber(draft.monthly_reset_at));
        const normalized: User = {
            ...draft,
            balance: safeNumber(draft.balance),
            monthly_limit: monthlyLimit,
            monthly_used: Math.max(0, safeNumber(draft.monthly_used)),
            monthly_expire_at: Math.max(0, safeNumber(draft.monthly_expire_at)),
            monthly_reset_at: monthlyLimit > 0 && monthlyResetAt <= nowUnix() ? nextLocalMidnightUnix() : monthlyResetAt,
        };

        updateUser.mutate(normalized, {
            onSuccess: () => toast.success('用户已更新'),
            onError: (error) => toast.error('用户更新失败', { description: apiErrorMessage(error) }),
        });
    };

    const reset = () => {
        if (!newPassword.trim()) return;
        resetPassword.mutate({ id: draft.id, password: newPassword }, {
            onSuccess: () => {
                setNewPassword('');
                toast.success('密码已重置');
            },
            onError: (error) => toast.error('密码重置失败', { description: apiErrorMessage(error) }),
        });
    };

    const remove = () => {
        deleteUser.mutate(draft.id, {
            onSuccess: () => toast.success('用户已删除'),
            onError: (error) => toast.error('用户删除失败', { description: apiErrorMessage(error) }),
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <div className="mb-4 flex items-center justify-between gap-3">
                <h3 className="flex min-w-0 items-center gap-2 text-base font-semibold">
                    <UserCog className="size-5" />
                    <SafeText value={draft.username} />
                </h3>
                <div className="flex items-center gap-2">
                    <button
                        type="button"
                        onClick={save}
                        disabled={updateUser.isPending}
                        className="inline-flex h-9 items-center gap-1.5 rounded-xl bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50"
                    >
                        {updateUser.isPending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
                        保存
                    </button>
                    <button
                        type="button"
                        onClick={remove}
                        disabled={deleteUser.isPending}
                        className="inline-flex h-9 items-center justify-center rounded-xl bg-destructive/10 px-3 text-destructive hover:bg-destructive hover:text-destructive-foreground disabled:opacity-50"
                    >
                        <Trash2 className="size-4" />
                    </button>
                </div>
            </div>

            <div className="mb-4 grid gap-2 md:grid-cols-3">
                <div className="rounded-2xl border border-border/70 bg-muted/30 px-3 py-2">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <MapPin className="size-3.5" />
                        注册 IP
                    </div>
                    <MonoSafeText value={emptyText(draft.register_ip)} className="mt-1 block text-xs text-foreground" />
                </div>
                <div className="rounded-2xl border border-border/70 bg-muted/30 px-3 py-2">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <History className="size-3.5" />
                        最近调用 IP
                    </div>
                    <MonoSafeText value={emptyText(draft.last_relay_ip)} className="mt-1 block text-xs text-foreground" />
                </div>
                <div className="rounded-2xl border border-border/70 bg-muted/30 px-3 py-2">
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                        <History className="size-3.5" />
                        最近调用时间
                    </div>
                    <div className="mt-1 truncate text-xs text-foreground">{formatDateTime(draft.last_relay_at)}</div>
                </div>
            </div>

            <div className="mb-4 rounded-2xl border border-border/70 bg-muted/20 p-4">
                <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
                    <div>
                        <h4 className="flex items-center gap-2 text-sm font-semibold">
                            <WalletCards className="size-4" />
                            月卡每日额度
                        </h4>
                        <p className="mt-1 text-xs text-muted-foreground">
                            按每日额度展示，保存时会自动转换到旧接口兼容字段。
                        </p>
                    </div>
                    <Badge variant={monthly.expired ? 'destructive' : monthly.active ? 'default' : 'outline'}>{monthly.status}</Badge>
                </div>
                <div className="grid gap-2 md:grid-cols-4">
                    <div className="rounded-xl border border-border/60 bg-background/60 px-3 py-2">
                        <div className="text-xs text-muted-foreground">每日额度</div>
                        <div className="mt-1 text-sm font-semibold">{formatMoney(monthly.limit)}</div>
                    </div>
                    <div className="rounded-xl border border-border/60 bg-background/60 px-3 py-2">
                        <div className="text-xs text-muted-foreground">今日已用</div>
                        <div className="mt-1 text-sm font-semibold">{formatMoney(monthly.used)}</div>
                    </div>
                    <div className="rounded-xl border border-border/60 bg-background/60 px-3 py-2">
                        <div className="text-xs text-muted-foreground">今日剩余</div>
                        <div className="mt-1 text-sm font-semibold">{formatMoney(monthly.remaining)}</div>
                    </div>
                    <div className="rounded-xl border border-border/60 bg-background/60 px-3 py-2">
                        <div className="text-xs text-muted-foreground">剩余天数</div>
                        <div className="mt-1 text-sm font-semibold">{daysRemainingLabel(monthly.expireAt)}</div>
                    </div>
                </div>
                <div className="mt-3">
                    <div className="mb-1 flex justify-between text-xs text-muted-foreground">
                        <span>今日用量</span>
                        <span>{monthly.progress.toFixed(1)}%</span>
                    </div>
                    <Progress value={monthly.progress} className="h-2" />
                </div>
                <div className="mt-3 grid gap-2 text-xs text-muted-foreground md:grid-cols-2">
                    <span className="inline-flex items-center gap-1.5">
                        <CalendarDays className="size-3.5" />
                        到期日期 {formatDate(monthly.expireAt)}
                    </span>
                    <span className="inline-flex items-center gap-1.5">
                        <RotateCcw className="size-3.5" />
                        下次重置 {formatResetLabel(monthly.resetAt, monthly.limit)}
                    </span>
                </div>
            </div>

            <div className="mb-4 rounded-2xl border border-border/70 bg-muted/20 p-4">
                <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
                    <div className="min-w-0">
                        <h4 className="flex items-center gap-2 text-sm font-semibold">
                            <ShieldCheck className="size-4" />
                            可用方案
                        </h4>
                        <p className="mt-1 text-xs leading-5 text-muted-foreground">
                            管理员在这里限制用户能用哪些方案；用户创建 API Key 时会自动继承这些方案。
                        </p>
                    </div>
                    <Badge variant="outline">{selectedPlans.length || 1} 个</Badge>
                </div>
                {enabledPlans.length === 0 ? (
                    <div className="rounded-xl border border-dashed border-border px-3 py-6 text-center text-xs text-muted-foreground">
                        暂无启用的方案
                    </div>
                ) : (
                    <>
                        <div className="mb-3 flex flex-wrap gap-2">
                            <button
                                type="button"
                                onClick={() => setAccessPlanIDs(enabledPlans.map((plan) => plan.id))}
                                className="h-8 rounded-xl border border-border bg-background px-3 text-xs font-medium hover:bg-muted"
                            >
                                全选
                            </button>
                            <button
                                type="button"
                                onClick={() => setAccessPlanIDs(defaultEnabledPlan ? [defaultEnabledPlan.id] : [])}
                                className="h-8 rounded-xl border border-border bg-background px-3 text-xs font-medium hover:bg-muted"
                            >
                                默认
                            </button>
                        </div>
                        <div className="flex min-w-0 flex-wrap gap-2">
                            {enabledPlans.map((plan) => {
                                const checked = selectedPlanIDs.includes(plan.id);
                                return (
                                    <button
                                        key={plan.id}
                                        type="button"
                                        onClick={() => setAccessPlanIDs(togglePlanID(selectedPlanIDs, plan.id))}
                                        className="min-w-0 text-left"
                                        title={`${accessPlanLabel(plan)} (${plan.slug})`}
                                    >
                                        <Badge
                                            variant={checked ? 'default' : 'outline'}
                                            className={cn(
                                                'max-w-[10rem] cursor-pointer select-none',
                                                !checked && 'bg-background/60 hover:bg-background'
                                            )}
                                        >
                                            <SafeText value={accessPlanLabel(plan)} />
                                        </Badge>
                                    </button>
                                );
                            })}
                        </div>
                        {selectedPlans.length > 0 && (
                            <label className="mt-3 grid gap-1 text-xs text-muted-foreground">
                                默认方案
                                <select
                                    value={defaultPlanID ?? ''}
                                    onChange={(event) => setDraft({ ...draft, default_access_plan_id: Number(event.target.value) || undefined })}
                                    className="h-10 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                                >
                                    {selectedPlans.map((plan) => (
                                        <option key={plan.id} value={plan.id}>
                                            {accessPlanLabel(plan)}
                                        </option>
                                    ))}
                                </select>
                            </label>
                        )}
                    </>
                )}
            </div>

            <div className="grid gap-3 md:grid-cols-2">
                <label className="grid gap-1 text-xs text-muted-foreground">
                    用户名
                    <Input value={draft.username} onChange={(e) => setDraft({ ...draft, username: e.target.value })} className="rounded-xl" />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    备注
                    <Input value={draft.note ?? ''} onChange={(e) => setDraft({ ...draft, note: e.target.value })} className="rounded-xl" />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    角色
                    <select
                        value={draft.role}
                        onChange={(e) => setDraft({ ...draft, role: e.target.value as UserRole })}
                        className="h-10 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                    >
                        <option value="admin">管理员</option>
                        <option value="user">普通用户</option>
                    </select>
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    状态
                    <select
                        value={draft.status}
                        onChange={(e) => setDraft({ ...draft, status: e.target.value as UserStatus })}
                        className="h-10 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                    >
                        <option value="active">启用</option>
                        <option value="disabled">停用</option>
                    </select>
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    余额额度
                    <Input
                        type="number"
                        step="0.000001"
                        value={draft.balance}
                        onChange={(e) => setDraft({ ...draft, balance: Number(e.target.value) })}
                        className="rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    每日额度
                    <Input
                        type="number"
                        step="0.000001"
                        value={draft.monthly_limit}
                        onChange={(e) => setDailyLimit(Number(e.target.value))}
                        className="rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    今日已用
                    <Input
                        type="number"
                        step="0.000001"
                        value={draft.monthly_used}
                        onChange={(e) => setDraft({ ...draft, monthly_used: Math.max(0, Number(e.target.value)) })}
                        className="rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    有效天数
                    <Input
                        type="number"
                        min={0}
                        value={expireDaysValue}
                        onChange={(e) => setExpireDays(Number(e.target.value))}
                        placeholder="例如 30"
                        className="rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    到期日期
                    <Input
                        type="date"
                        value={unixToDateInput(draft.monthly_expire_at)}
                        onChange={(e) => setDraft({ ...draft, monthly_expire_at: dateInputToEndOfDayUnix(e.target.value), monthly_reset_at: draft.monthly_reset_at || nextLocalMidnightUnix() })}
                        className="rounded-xl"
                    />
                </label>
            </div>

            <details className="mt-3 rounded-2xl border border-dashed border-border/70 bg-muted/10 px-3 py-2">
                <summary className="flex cursor-pointer list-none items-center gap-2 text-xs font-medium text-muted-foreground">
                    <ChevronDown className="size-3.5" />
                    高级/调试：兼容 Unix 秒字段
                </summary>
                <div className="mt-3 grid gap-3 md:grid-cols-2">
                    <label className="grid gap-1 text-xs text-muted-foreground">
                        monthly_expire_at Unix 秒
                        <Input
                            type="number"
                            value={draft.monthly_expire_at || ''}
                            onChange={(e) => setDraft({ ...draft, monthly_expire_at: Number(e.target.value) || 0 })}
                            className="rounded-xl"
                        />
                    </label>
                    <label className="grid gap-1 text-xs text-muted-foreground">
                        monthly_reset_at Unix 秒
                        <Input
                            type="number"
                            value={draft.monthly_reset_at || ''}
                            onChange={(e) => setDraft({ ...draft, monthly_reset_at: Number(e.target.value) || 0 })}
                            className="rounded-xl"
                        />
                    </label>
                </div>
            </details>

            <div className="mt-4 flex gap-2">
                <Input
                    type="password"
                    value={newPassword}
                    onChange={(e) => setNewPassword(e.target.value)}
                    placeholder="新密码"
                    className="rounded-xl"
                />
                <button
                    type="button"
                    onClick={reset}
                    disabled={resetPassword.isPending || !newPassword.trim()}
                    className="h-10 shrink-0 rounded-xl bg-muted px-3 text-sm font-medium text-muted-foreground hover:bg-muted/80 disabled:opacity-50"
                >
                    重置密码
                </button>
            </div>
        </div>
    );
}

function CreateUserPanel({ accessPlans }: { accessPlans: AccessPlan[] }) {
    const createUser = useCreateUser();
    const [username, setUsername] = useState('');
    const [password, setPassword] = useState('');
    const [role, setRole] = useState<UserRole>('user');
    const [balance, setBalance] = useState(0);
    const enabledPlans = accessPlans.filter((plan) => plan.enabled);
    const defaultPlan = enabledPlans.find((plan) => plan.is_default) ?? enabledPlans[0];
    const [accessPlanIDs, setAccessPlanIDs] = useState<number[]>(() => (defaultPlan ? [defaultPlan.id] : []));
    const [defaultAccessPlanID, setDefaultAccessPlanID] = useState<number | undefined>(() => defaultPlan?.id);
    const effectiveAccessPlanIDs = accessPlanIDs.length > 0 ? accessPlanIDs : defaultPlan ? [defaultPlan.id] : [];
    const effectiveDefaultAccessPlanID = defaultAccessPlanID && effectiveAccessPlanIDs.includes(defaultAccessPlanID)
        ? defaultAccessPlanID
        : effectiveAccessPlanIDs[0];

    const setCreateAccessPlanIDs = (ids: number[]) => {
        const fallbackIDs = ids.length > 0 ? ids : defaultPlan ? [defaultPlan.id] : [];
        setAccessPlanIDs(fallbackIDs);
        setDefaultAccessPlanID((current) => (current && fallbackIDs.includes(current) ? current : fallbackIDs[0]));
    };

    const submit = () => {
        createUser.mutate({ username, password, role, status: 'active', balance, access_plan_ids: effectiveAccessPlanIDs, default_access_plan_id: effectiveDefaultAccessPlanID }, {
            onSuccess: () => {
                setUsername('');
                setPassword('');
                setBalance(0);
                setCreateAccessPlanIDs(defaultPlan ? [defaultPlan.id] : []);
                toast.success('用户已创建');
            },
            onError: (error) => toast.error('用户创建失败', { description: apiErrorMessage(error) }),
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <h3 className="mb-4 flex items-center gap-2 text-base font-semibold">
                <Plus className="size-5" />
                新建用户
            </h3>
            <div className="grid gap-3 md:grid-cols-4">
                <Input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="用户名" className="rounded-xl" />
                <Input value={password} onChange={(e) => setPassword(e.target.value)} placeholder="初始密码" className="rounded-xl" />
                <select value={role} onChange={(e) => setRole(e.target.value as UserRole)} className="h-10 rounded-xl border border-input bg-background px-3 text-sm">
                    <option value="user">普通用户</option>
                    <option value="admin">管理员</option>
                </select>
                <div className="flex gap-2">
                    <Input type="number" value={balance} onChange={(e) => setBalance(Number(e.target.value))} placeholder="余额" className="rounded-xl" />
                    <button
                        type="button"
                        onClick={submit}
                        disabled={createUser.isPending || !username.trim() || !password.trim()}
                        className="h-10 shrink-0 rounded-xl bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50"
                    >
                        {createUser.isPending ? <Loader2 className="size-4 animate-spin" /> : '创建'}
                    </button>
                </div>
            </div>
            {enabledPlans.length > 0 && (
                <div className="mt-4 rounded-2xl border border-border/70 bg-muted/20 p-3">
                    <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                        <div className="text-xs font-medium text-muted-foreground">新用户可用方案</div>
                        <div className="flex gap-2">
                            <button
                                type="button"
                                onClick={() => setCreateAccessPlanIDs(enabledPlans.map((plan) => plan.id))}
                                className="h-7 rounded-lg border border-border bg-background px-2 text-xs hover:bg-muted"
                            >
                                全选
                            </button>
                            <button
                                type="button"
                                onClick={() => setCreateAccessPlanIDs(defaultPlan ? [defaultPlan.id] : [])}
                                className="h-7 rounded-lg border border-border bg-background px-2 text-xs hover:bg-muted"
                            >
                                默认
                            </button>
                        </div>
                    </div>
                    <div className="flex min-w-0 flex-wrap gap-2">
                        {enabledPlans.map((plan) => {
                            const checked = effectiveAccessPlanIDs.includes(plan.id);
                            return (
                                <button
                                    key={plan.id}
                                    type="button"
                                    onClick={() => setCreateAccessPlanIDs(togglePlanID(effectiveAccessPlanIDs, plan.id))}
                                    className="min-w-0"
                                    title={`${accessPlanLabel(plan)} (${plan.slug})`}
                                >
                                    <Badge
                                        variant={checked ? 'default' : 'outline'}
                                        className={cn('max-w-[10rem] cursor-pointer select-none', !checked && 'bg-background/60 hover:bg-background')}
                                    >
                                        <SafeText value={accessPlanLabel(plan)} />
                                    </Badge>
                                </button>
                            );
                        })}
                    </div>
                    {effectiveAccessPlanIDs.length > 0 && (
                        <label className="mt-3 grid gap-1 text-xs text-muted-foreground">
                            默认方案
                            <select
                                value={effectiveDefaultAccessPlanID ?? ''}
                                onChange={(event) => setDefaultAccessPlanID(Number(event.target.value) || undefined)}
                                className="h-9 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                            >
                                {enabledPlans.filter((plan) => effectiveAccessPlanIDs.includes(plan.id)).map((plan) => (
                                    <option key={plan.id} value={plan.id}>{accessPlanLabel(plan)}</option>
                                ))}
                            </select>
                        </label>
                    )}
                </div>
            )}
        </div>
    );
}

function RedeemAdminPanel() {
    const { data: codes = [], isLoading } = useRedeemCodeList();
    const generate = useGenerateRedeemCode();
    const update = useUpdateRedeemCode();
    const remove = useDeleteRedeemCode();
    const [type, setType] = useState<RedeemCodeType>('balance');
    const [count, setCount] = useState(1);
    const [amount, setAmount] = useState(1);
    const [monthlyDays, setMonthlyDays] = useState(30);
    const canSubmit = count > 0 && amount > 0 && (type === 'balance' || monthlyDays > 0);

    const submit = () => {
        if (!canSubmit) return;
        generate.mutate({
            type,
            count: Math.min(500, Math.max(1, Math.floor(count))),
            balance_amount: type === 'balance' ? amount : undefined,
            monthly_limit: type === 'monthly' ? amount : undefined,
            monthly_days: type === 'monthly' ? Math.max(1, Math.floor(monthlyDays)) : undefined,
        }, {
            onSuccess: () => toast.success('兑换码已生成'),
            onError: (error) => toast.error('兑换码生成失败', { description: apiErrorMessage(error) }),
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <div className="mb-4 flex items-center justify-between gap-3">
                <h3 className="flex items-center gap-2 text-base font-semibold">
                    <Ticket className="size-5" />
                    兑换码
                </h3>
                <Badge variant="outline">{codes.length}</Badge>
            </div>
            <div className="grid gap-2 md:grid-cols-[120px_90px_1fr_1fr_auto] md:items-end">
                <label className="grid gap-1 text-xs text-muted-foreground">
                    类型
                    <select value={type} onChange={(e) => setType(e.target.value as RedeemCodeType)} className="h-10 rounded-xl border border-input bg-background px-3 text-sm text-foreground">
                        <option value="balance">额度</option>
                        <option value="monthly">月卡</option>
                    </select>
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    数量
                    <Input type="number" min={1} max={500} value={count} onChange={(e) => setCount(Number(e.target.value))} className="rounded-xl" />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {type === 'balance' ? '额度金额' : '每日额度'}
                    <Input
                        type="number"
                        step="0.000001"
                        value={amount}
                        onChange={(e) => setAmount(Number(e.target.value))}
                        placeholder={type === 'balance' ? '一次性额度金额' : '每天可用额度'}
                        className="rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    有效天数
                    <Input
                        type="number"
                        min={1}
                        value={monthlyDays}
                        onChange={(e) => setMonthlyDays(Number(e.target.value))}
                        disabled={type !== 'monthly'}
                        placeholder="例如 30"
                        className="rounded-xl"
                    />
                </label>
                <button type="button" onClick={submit} disabled={generate.isPending || !canSubmit} className="h-10 rounded-xl bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50">
                    生成
                </button>
            </div>
            <div className="mt-4 max-h-72 space-y-2 overflow-y-auto">
                {isLoading ? (
                    <div className="flex justify-center py-8 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
                ) : codes.length === 0 ? (
                    <div className="py-8 text-center text-sm text-muted-foreground">暂无兑换码</div>
                ) : codes.map((code) => (
                    <div key={code.id} className="grid gap-2 rounded-2xl border border-border bg-muted/20 p-3 md:grid-cols-[1fr_auto_auto_auto] md:items-center">
                        <div className="min-w-0">
                            <div className="flex items-center gap-2">
                                <MonoSafeText value={code.code} className="text-sm flex-1" />
                                <CopyIconButton text={code.code} className="text-muted-foreground hover:text-foreground" copyIconClassName="size-4" checkIconClassName="size-4" />
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                                {code.type === 'balance' ? `额度 ${formatMoney(code.balance_amount)}` : `月卡 · 每日额度 ${formatMoney(code.monthly_limit)} · 有效 ${code.monthly_days} 天`}
                                {code.used && ` · 已由用户 #${code.used_by_user_id} 使用`}
                            </div>
                        </div>
                        <Badge variant={code.used ? 'secondary' : code.enabled ? 'default' : 'destructive'}>
                            {code.used ? '已用' : code.enabled ? '可用' : '停用'}
                        </Badge>
                        <Switch
                            checked={code.enabled}
                            disabled={code.used || update.isPending}
                            onCheckedChange={(enabled) => update.mutate({ id: code.id, enabled, note: code.note })}
                        />
                        <button
                            type="button"
                            disabled={code.used || remove.isPending}
                            onClick={() => remove.mutate(code.id)}
                            className="inline-flex h-9 items-center justify-center rounded-xl bg-destructive/10 px-3 text-destructive hover:bg-destructive hover:text-destructive-foreground disabled:opacity-40"
                        >
                            <Trash2 className="size-4" />
                        </button>
                    </div>
                ))}
            </div>
        </div>
    );
}

function UserRankPanel() {
    const { data: rank = [] } = useUserUsageRank();
    const sorted = [...rank].sort((a, b) => b.total_cost - a.total_cost).slice(0, 6);
    return (
        <div className="rounded-3xl border border-border bg-card p-5">
            <h3 className="mb-3 flex items-center gap-2 text-base font-semibold">
                <WalletCards className="size-5" />
                用户排行
            </h3>
            <div className="space-y-2">
                {sorted.length === 0 ? (
                    <div className="py-8 text-center text-sm text-muted-foreground">暂无用户使用记录</div>
                ) : sorted.map((item, index) => (
                    <div key={item.user_id} className="flex items-center gap-3 rounded-2xl bg-muted/30 px-3 py-2">
                        <span className="w-6 text-sm font-semibold text-muted-foreground">{index + 1}</span>
                        <SafeText value={item.username} className="flex-1 text-sm font-medium" />
                        <span className="text-xs text-muted-foreground">{item.request_success + item.request_failed} 次</span>
                        <span className="text-sm font-semibold">{formatMoney(item.total_cost)}</span>
                    </div>
                ))}
            </div>
        </div>
    );
}

export function UserManagement() {
    const { data: users = [], isLoading } = useUserList();
    const { data: accessPlans = [] } = useAccessPlanList();
    const [selectedID, setSelectedID] = useState<number | null>(null);
    const selected = users.find((user) => user.id === (selectedID ?? users[0]?.id)) ?? null;

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 pb-24 md:pb-4 rounded-t-3xl">
            <CreateUserPanel accessPlans={accessPlans} />
            <div className="grid min-w-0 gap-5 lg:grid-cols-[360px_minmax(0,1fr)]">
                <div className="min-w-0 rounded-3xl border border-border bg-card p-4">
                    <div className="mb-3 flex items-center justify-between">
                        <h3 className="text-base font-semibold">用户列表</h3>
                        <Badge variant="outline">{users.length}</Badge>
                    </div>
                    <div className="max-h-[520px] space-y-2 overflow-y-auto">
                        {isLoading ? (
                            <div className="flex justify-center py-10 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>
                        ) : users.map((user) => (
                            <UserRow
                                key={user.id}
                                user={user}
                                plans={accessPlans}
                                active={selected?.id === user.id}
                                onSelect={() => setSelectedID(user.id)}
                            />
                        ))}
                    </div>
                </div>
                <UserEditor key={selected ? `${selected.id}-${selected.updated_at ?? 0}` : 'empty'} user={selected} accessPlans={accessPlans} />
            </div>
            <div className="grid min-w-0 gap-5 xl:grid-cols-[minmax(0,1fr)_360px]">
                <RedeemAdminPanel />
                <UserRankPanel />
            </div>
        </PageWrapper>
    );
}
