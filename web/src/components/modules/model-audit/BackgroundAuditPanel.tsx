'use client';

import { useState } from 'react';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { toast } from '@/components/common/Toast';
import { useSettingList, useSetSetting } from '@/api/endpoints/setting';
import { cn } from '@/lib/utils';
import {
    clampLimit,
    clampRounds,
    parseBudgetUsage,
    settingBool,
    todayLocal,
    type AuditRounds,
} from '@/lib/audit-budget';

/**
 * 后台检测面板：把「不用守着页面」的两种检测放在一张卡上，并说清各自花不花钱。
 *
 * 三条硬约定都是钱的问题，一个都不能含糊：
 *   1. 被动检测不花钱、不发请求——它只读已经经过的日志；
 *   2. 少量 token 抽检是真的花钱：每渠道每天几轮、全站每天上限、今天已经用掉多少，
 *      全部摆出来；读不懂的用量按 0 说，绝不编数字；
 *   3. 关着的时候后台什么都不会自己发。
 */

// 设置键写字面量、不引 SettingKey 常量：这几个键由后端同期实现，setting.ts 的
// SettingKey 表这次不动。键名只在这里定义一次，改也只改这一处。
// 被动检测开关（log_passive_probe_enabled）不在这里：本页「日志查账」卡（PassiveProbeCard）已有，
// 同一页同一个键放两个开关只会让人疑惑哪个算数。
const KEY_ENABLED = 'model_audit_enabled'; // 抽检总开关，默认关
const KEY_FOLLOWUP = 'model_audit_followup_enabled'; // 可疑日志自动跟查，默认关
const KEY_ROUNDS = 'model_audit_channel_daily_rounds'; // 每渠道每天轮数 1-4，默认 1
const KEY_LIMIT = 'model_audit_daily_request_limit'; // 全站每天上限，默认 200
const KEY_USAGE = 'model_audit_budget_usage'; // 只读：{"date","requests"}

const ROUND_OPTIONS: readonly AuditRounds[] = [1, 2, 3, 4];

export function BackgroundAuditPanel() {
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();
    // 上限输入框的本地草稿：null 表示「没在编辑，跟着服务端值走」。
    // 边打字边落库会把 200 变成 2、20、200 三次写盘，所以只在失焦时保存一次。
    const [limitDraft, setLimitDraft] = useState<string | null>(null);

    const raw = (key: string) => settings?.find((item) => item.key === key)?.value;

    const auditEnabled = settingBool(raw(KEY_ENABLED), false);
    const followup = settingBool(raw(KEY_FOLLOWUP), false);
    const rounds = clampRounds(raw(KEY_ROUNDS));
    const limit = clampLimit(raw(KEY_LIMIT));
    const used = parseBudgetUsage(raw(KEY_USAGE), todayLocal());

    // 保存照本页现有写法：useSetSetting 成功后自己会 invalidate ['settings','list']，
    // 所以这里只管提示，不再手动刷新一遍。
    const save = (key: string, value: string, okText: string) =>
        setSetting.mutate(
            { key, value },
            {
                onSuccess: () => toast.success(okText),
                onError: (error) =>
                    toast.error('保存失败', {
                        description: error instanceof Error ? error.message : String(error),
                    }),
            },
        );

    const toggleEnabled = (next: boolean) =>
        save(KEY_ENABLED, next ? 'true' : 'false', next ? '已开启少量 token 抽检' : '已关闭少量 token 抽检');

    const toggleFollowup = (next: boolean) =>
        save(KEY_FOLLOWUP, next ? 'true' : 'false', next ? '可疑日志会自动跟查' : '可疑日志不再自动跟查');

    const chooseRounds = (next: AuditRounds) => {
        if (next === rounds || setSetting.isPending) return;
        save(KEY_ROUNDS, String(next), `改为每个渠道每天 ${next} 轮`);
    };

    const saveLimit = () => {
        if (limitDraft === null) return;
        const next = clampLimit(limitDraft);
        setLimitDraft(null);
        if (next === limit) return;
        save(KEY_LIMIT, String(next), `全站每天上限改为 ${next} 次`);
    };

    return (
        <div data-bg-audit className="overflow-hidden rounded-2xl border border-border bg-card">
            <div className="px-4 pt-3 pb-4">
                <p className="text-[11px] font-medium tracking-[0.16em] text-muted-foreground">后台检测</p>

                <p className="mt-1 text-xs leading-5 text-muted-foreground">
                    被动检测（不花钱）的开关在下面「日志查账」卡里。
                </p>

                {/* 少量 token 抽检：真的花钱 */}
                <div className="mt-3 flex items-start justify-between gap-3 border-t border-border/60 pt-3">
                    <div className="min-w-0">
                        <p className="flex items-center gap-1.5 text-sm font-medium text-foreground">
                            少量 token 抽检
                            <span className="rounded-full bg-amber-500/15 px-1.5 py-px text-[10px] font-medium text-amber-800 dark:text-amber-300">
                                花钱
                            </span>
                        </p>
                        <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                            每个渠道每天抽检 {rounds} 轮，只跑认模型的检查（分词器、指纹；Claude
                            渠道加签名和复述系统提示词）。全站每天最多 {limit} 次请求。
                        </p>
                    </div>
                    <Switch
                        checked={auditEnabled}
                        onCheckedChange={toggleEnabled}
                        disabled={setSetting.isPending}
                        aria-label="少量 token 抽检"
                    />
                </div>

                {/* 3. 抽检开着才显示的花钱参数与今天用量 */}
                {auditEnabled && (
                    <div className="mt-3 space-y-3 rounded-xl bg-muted/40 px-3 py-2.5">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                            <span className="text-xs text-muted-foreground">每个渠道每天轮数</span>
                            <div
                                role="radiogroup"
                                aria-label="每个渠道每天轮数"
                                className="inline-flex rounded-lg border border-border bg-background p-0.5"
                            >
                                {ROUND_OPTIONS.map((option) => (
                                    <button
                                        key={option}
                                        type="button"
                                        role="radio"
                                        aria-checked={rounds === option}
                                        disabled={setSetting.isPending}
                                        onClick={() => chooseRounds(option)}
                                        className={cn(
                                            'rounded-md px-2.5 py-0.5 text-xs font-medium tabular-nums transition-colors disabled:opacity-60',
                                            rounds === option
                                                ? 'bg-primary text-primary-foreground'
                                                : 'text-muted-foreground hover:text-foreground',
                                        )}
                                    >
                                        {option}
                                    </button>
                                ))}
                            </div>
                        </div>

                        <label className="flex flex-wrap items-center justify-between gap-2">
                            <span className="text-xs text-muted-foreground">全站每天上限（次）</span>
                            <Input
                                type="number"
                                inputMode="numeric"
                                min={1}
                                max={100000}
                                value={limitDraft ?? String(limit)}
                                onChange={(event) => setLimitDraft(event.target.value)}
                                onBlur={saveLimit}
                                aria-label="全站每天上限"
                                className="h-8 w-28 shrink-0 rounded-lg text-xs"
                            />
                        </label>

                        <p className="text-xs tabular-nums text-muted-foreground">
                            今日已用 {used} / {limit}
                        </p>

                        <label className="flex items-start gap-2 border-t border-border/60 pt-2 text-xs">
                            <input
                                type="checkbox"
                                className="mt-0.5"
                                checked={followup}
                                disabled={setSetting.isPending}
                                onChange={(event) => toggleFollowup(event.target.checked)}
                            />
                            <span className="min-w-0">
                                <span className="font-medium text-foreground">可疑日志自动跟查</span>
                                <span className="mt-0.5 block text-muted-foreground">
                                    被动检测标成可疑、或上游自报模型名对不上时，自动用抽检额度复查一次
                                </span>
                            </span>
                        </label>
                    </div>
                )}

                <p className="mt-3 border-t border-border/60 pt-2 text-[11px] leading-5 text-muted-foreground">
                    抽检会真的消耗上游额度；关着时后台不会自己发任何检测请求。
                </p>
            </div>
        </div>
    );
}
