'use client';

import { useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Switch } from '@/components/ui/switch';
import { SearchableSelect, type SearchableSelectOption } from '@/components/ui/searchable-select';
import { toast } from '@/components/common/Toast';
import {
    HLWY_IMPORT_MAX_BYTES,
    hlwyBaselineOptionLabel,
    hlwyEntryUnavailableText,
    hlwySourceLabel,
} from './hlwy';
import { useHlwyBaselines, useImportHlwyBaseline, type HlwyPlanResponse, type HlwySampleCount } from '@/api/endpoints/model-audit';
import { useSettingList, useSetSetting } from '@/api/endpoints/setting';
import { settingBool } from '@/lib/audit-budget';
import { cn } from '@/lib/utils';

/**
 * hlwy 基准对照的选择区：选基准、选样本量、看这一单的账、导入旧文件。
 *
 * 这里刻意没有任何「开始」按钮——同一页只留一个提交动作（在页面底部），
 * 否则切了模式以后旧按钮还亮着，点下去就按老规矩跑一轮多探针。
 *
 * 开关写字面量 `model_audit_hlwy_enabled`，与页面上其它几个键同一套写法：
 * 键由后端同期实现，本次不动 setting.ts 的常量表。
 */

const KEY_HLWY_ENABLED = 'model_audit_hlwy_enabled';

const SAMPLE_OPTIONS: readonly HlwySampleCount[] = [100, 200];

export type HlwyModePanelProps = {
    baselineId: string;
    onBaselineChange: (id: string) => void;
    samples: HlwySampleCount;
    onSamplesChange: (samples: HlwySampleCount) => void;
    plan: HlwyPlanResponse | null;
    planLoading: boolean;
    planError: string;
};

export function HlwyModePanel({
    baselineId,
    onBaselineChange,
    samples,
    onSamplesChange,
    plan,
    planLoading,
    planError,
}: HlwyModePanelProps) {
    const { data: catalog, isLoading: catalogLoading, error: catalogError, refetch: refetchCatalog } =
        useHlwyBaselines();
    const importBaseline = useImportHlwyBaseline();
    const { data: settings, isLoading: settingsLoading } = useSettingList();
    const setSetting = useSetSetting();
    // 开关改的是服务端行为，而 `useHlwyPlan` 的缓存里带着上一次的 enabled。
    // settings 缓存被 useSetSetting 自己刷过了，计划缓存不会跟着失效：不刷的话
    // 打开开关后要等焦点切换/键变了才重读，按钮一直是灰的。
    const queryClient = useQueryClient();
    const [importError, setImportError] = useState('');
    const [importedTip, setImportedTip] = useState('');

    const flagRaw = settings?.find((item) => item.key === KEY_HLWY_ENABLED)?.value;
    // 默认关：这是一个新的、会真的花钱的模式，没被打开过就不该自己开着。
    const flagEnabled = settingBool(flagRaw, false);

    const baselines = catalog?.baselines ?? [];
    // 不可用的件只禁用自己：一份本地件校验没过，不该把内置件和别的健康件一起挡掉。
    const availableCount = baselines.filter((entry) => entry.unavailable !== true).length;
    const unavailableCount = baselines.length - availableCount;
    const options = useMemo<SearchableSelectOption[]>(
        () =>
            baselines.map((entry) => ({
                value: entry.id,
                label: hlwyBaselineOptionLabel(entry),
                // keywords 是搜索用的**一串文本**，不是字符串数组。
                keywords: [entry.label, entry.source_kind, entry.declared_model ?? ''].filter(Boolean).join(' '),
                // 校验没过的件照样列出来（用户要知道它存在），但选不中：
                // SearchableSelect 的 disabled 是真支持的（不触发 onValueChange）。
                disabled: entry.unavailable === true,
            })),
        [baselines],
    );
    const selected = baselines.find((entry) => entry.id === baselineId) ?? null;
    // 选中的那份恰好被标不可用（例如选完之后目录刷新）也要在页面上说清楚。
    const selectedUnavailable = selected?.unavailable === true;

    const toggleFlag = (next: boolean) =>
        setSetting.mutate(
            { key: KEY_HLWY_ENABLED, value: next ? 'true' : 'false' },
            {
                onSuccess: () => {
                    queryClient.invalidateQueries({ queryKey: ['model-audit', 'hlwy-plan'] });
                    toast.success(next ? '基准对照可提交了' : '基准对照已关闭提交');
                },
                onError: (e) =>
                    toast.error('保存失败', { description: e instanceof Error ? e.message : String(e) }),
            },
        );

    const handleFile = async (file: File | null) => {
        setImportError('');
        setImportedTip('');
        if (!file) return;
        if (file.size > HLWY_IMPORT_MAX_BYTES) {
            setImportError(`文件 ${file.size} 字节，超过 ${HLWY_IMPORT_MAX_BYTES} 字节上限`);
            return;
        }
        let parsed: unknown;
        try {
            parsed = JSON.parse(await file.text());
        } catch {
            setImportError('这不是有效的 JSON 文件');
            return;
        }
        if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
            // 数组也是 object；旧文件本来就是一个对象，数组肯定是拿错了文件。
            setImportError('文件内容不是一个 JSON 对象');
            return;
        }
        importBaseline.mutate(parsed as Record<string, unknown>, {
            onSuccess: (data) => {
                setImportedTip(`已导入：${data.baseline.label}（${data.baseline.valid_count} 个样本）`);
                onBaselineChange(data.baseline.id);
            },
            // 服务端的指责要原样摆出来：它比任何本地猜测都准确。
            onError: (e) => setImportError(e instanceof Error ? e.message : String(e)),
        });
    };

    return (
        <div className="rounded-xl border border-border bg-muted/20 p-3">
            <div className="flex flex-wrap items-center gap-2">
                <span className="text-sm font-semibold">基准对照</span>
                <span className="rounded-full bg-muted px-1.5 py-px text-[10px] text-muted-foreground">hlwy 1–355</span>
                <label className="ml-auto flex items-center gap-1.5 text-[11px] text-muted-foreground">
                    <span>{flagEnabled ? '允许提交' : '已关闭提交'}</span>
                    {/* 设置没读出来就不知道开关到底什么状态，这时不让点：改开关得写对键。 */}
                    <Switch
                        checked={flagEnabled}
                        onCheckedChange={toggleFlag}
                        disabled={settingsLoading || setSetting.isPending || !settings}
                        aria-label="允许提交新的基准对照"
                    />
                </label>
            </div>
            {(settingsLoading || !settings) && (
                <p className="mt-1 text-[11px] leading-5 text-muted-foreground">
                    {settingsLoading ? '正在读取设置…' : '没读到设置，开关先不能用'}
                </p>
            )}

            <p className="mt-1.5 text-xs leading-5 text-muted-foreground">
                同一个问题问 100 或 200 次，只统计回答里 1–355 这些数字的分布，再和基准比分布相似度。
                它不判定模型真假，也不参与上面那套风险计分。
            </p>

            {!flagEnabled && (
                <p className="mt-2 rounded-lg bg-amber-50 px-2 py-1.5 text-[11px] leading-5 text-amber-800">
                    提交开关关着：只是不能开新的对照。历史记录、导入旧基准、换基准重算照常可用。
                </p>
            )}

            <div className="mt-3 space-y-2">
                {/* 目录读不到就说读不到、并且给一条重试的路；默默留一个空选择器等于让人以为没有基准。 */}
                {catalogError && (
                    <div className="rounded-lg bg-red-50 px-2 py-1.5 text-[11px] leading-5 text-red-700">
                        <p>基准目录没读出来：{catalogError instanceof Error ? catalogError.message : String(catalogError)}</p>
                        <button type="button" className="mt-1 underline" onClick={() => void refetchCatalog()}>
                            重新读取目录
                        </button>
                    </div>
                )}
                {catalogLoading && !catalog && !catalogError && (
                    <p className="text-[11px] leading-5 text-muted-foreground">正在读取基准目录…</p>
                )}
                <div>
                    <div className="mb-1 flex items-center justify-between gap-2">
                        <span className="text-xs font-medium">主对照基准</span>
                        {/* 目录没读出来的时候不写「0 份可选」：那会读成「一份基准都没有」。 */}
                        <span className="text-[11px] text-muted-foreground">
                            {!catalog
                                ? ''
                                : unavailableCount > 0
                                  ? `${availableCount} 份可用（另 ${unavailableCount} 份暂不可用）`
                                  : `${baselines.length} 份可选`}
                        </span>
                    </div>
                    <SearchableSelect
                        value={baselineId}
                        onValueChange={onBaselineChange}
                        options={options}
                        placeholder="选一份基准"
                        searchPlaceholder="搜名称 / 来源"
                        emptyText="没有匹配的基准"
                    />
                </div>

                {/* 元数据一直摆在选择器下面，不藏在 hover 里：360px 也能一眼看到来源和日期。 */}
                <div className="flex flex-wrap gap-x-3 gap-y-1 rounded-lg bg-background/60 px-2 py-1.5 text-[11px] leading-5 text-muted-foreground">
                    <span>来源：{selected ? hlwySourceLabel(selected.source_kind) : '还没选'}</span>
                    <span>采集日期：{selected?.collected_at ? selected.collected_at.slice(0, 10) : '日期未知'}</span>
                    <span>有效样本：{selected ? `${selected.valid_count} 个` : '未知'}</span>
                    <span>声明模型：{selected?.declared_model || '未标注'}</span>
                    {/* 接口协议和题目协议是两件事：合成一项会把等题版本读成请求协议。 */}
                    <span>接口协议：{selected?.conditions?.protocol || '未标注'}</span>
                    <span>题目协议：{selected?.conditions?.protocol_id || '未标注'}</span>
                </div>

                {/* 不可用的件在看得到的地方说明原因；健康件不受影响。 */}
                {(selectedUnavailable || unavailableCount > 0) && (
                    <div className="space-y-1">
                        {selectedUnavailable && selected && (
                            <p className="rounded-lg bg-amber-50 px-2 py-1.5 text-[11px] leading-5 text-amber-800">
                                选中的「{selected.label}」现在不能用：{hlwyEntryUnavailableText(selected)}。换一份基准再提交。
                            </p>
                        )}
                        {unavailableCount > 0 && (
                            <p className="text-[11px] leading-5 text-muted-foreground">
                                目录里有 {unavailableCount} 份校验没通过，已置灰不可选；其他基准照常可选，历史记录不受影响。
                            </p>
                        )}
                    </div>
                )}

                <div>
                    <span className="mb-1 block text-xs font-medium">采样次数</span>
                    <div className="flex flex-wrap gap-2" role="group" aria-label="采样次数">
                        {SAMPLE_OPTIONS.map((option) => (
                            <button
                                key={option}
                                type="button"
                                aria-pressed={samples === option}
                                onClick={() => onSamplesChange(option)}
                                className={cn(
                                    'rounded-lg border px-2.5 py-1 text-xs transition-colors',
                                    samples === option
                                        ? 'border-primary bg-primary/10 font-medium text-primary'
                                        : 'border-border bg-background text-muted-foreground hover:bg-muted/50'
                                )}
                            >
                                {option} 次
                            </button>
                        ))}
                    </div>
                </div>

                <div className="rounded-lg bg-background/60 px-2 py-1.5 text-[11px] leading-5">
                    {planError ? (
                        <p className="text-red-600">计划没读出来：{planError}（读不到计划就不给提交）</p>
                    ) : plan ? (
                        <>
                            <div className="flex flex-wrap gap-x-3 gap-y-1 text-muted-foreground">
                                <span className="text-foreground">这一单最多 {plan.max_calls} 次真实调用</span>
                                <span>单个样本超时 {plan.sample_timeout_seconds} 秒</span>
                                <span>整单超时 {plan.timeout_seconds} 秒</span>
                                <span>{plan.enabled ? '服务端已启用' : '服务端未启用'}</span>
                                <span>题目协议 {plan.protocol_id}</span>
                            </div>
                            {plan.conditions_note && (
                                <p className="mt-1 text-muted-foreground">{plan.conditions_note}</p>
                            )}
                            <p className="mt-1 text-muted-foreground">
                                按上游正常价格计费；提交后可以离开本页，回来在「检测记录」里看。
                            </p>
                        </>
                    ) : (
                        <p className="text-muted-foreground">
                            {planLoading ? '正在读取这一单的计划…' : '还没读到计划：选好基准后会自动读取'}
                        </p>
                    )}
                </div>
            </div>

            <details className="mt-3 rounded-lg bg-background/60 px-2 py-1.5">
                <summary className="cursor-pointer list-none text-[11px] font-medium text-muted-foreground">
                    导入旧 hlwy JSON（最多 1 MiB）
                </summary>
                <div className="mt-2 space-y-1.5">
                    <input
                        type="file"
                        accept="application/json,.json"
                        className="block w-full text-[11px] file:mr-2 file:rounded-md file:border file:border-border file:bg-background file:px-2 file:py-1 file:text-[11px]"
                        onChange={(event) => {
                            const file = event.target.files?.[0] ?? null;
                            void handleFile(file);
                            // 允许重复选同一个文件（第二次改完再传要能触发 change）。
                            event.target.value = '';
                        }}
                    />
                    {importBaseline.isPending && <p className="text-[11px] text-muted-foreground">正在导入…</p>}
                    {importedTip && <p className="text-[11px] text-emerald-700">{importedTip}</p>}
                    {importError && <p className="text-[11px] text-red-600">{importError}</p>}
                    <p className="text-[11px] leading-5 text-muted-foreground">
                        文件里的 distribution 会按 rawData 重算核对，对不上就不入库。
                    </p>
                </div>
            </details>
        </div>
    );
}
