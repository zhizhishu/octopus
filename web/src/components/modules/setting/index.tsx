'use client';

import { useTranslations } from 'next-intl';
import { Zap } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { SettingAppearance } from './Appearance';
import { SettingSystem } from './System';
import { SettingFingerprintProfile } from './FingerprintProfile';
import { SettingAccount } from './Account';
import { SettingAccessToken } from './AccessToken';
import { SettingInfo } from './Info';
import { SettingLLMSync } from './LLMSync';
import { SettingLog } from './Log';
import { SettingBackup } from './Backup';
import { SettingCircuitBreaker } from './CircuitBreaker';

// 「路由默认模式」（setting route_mode_override）的编辑入口已收拢到「方案 → 无限画布」
// 工具栏（用户 2026-09-30 定），这里不再放第二个可编辑副本：两个入口的说法不一致会误导，
// 而且本页一加载就会把历史空值固化写回，等于「打开设置页就悄悄改了路由默认」。
function SettingRouteModeMoved() {
    const t = useTranslations('setting');

    return (
        <div className="rounded-3xl border border-border bg-card p-6">
            <div className="flex items-start gap-3">
                <Zap className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                    <div className="min-w-0 text-sm font-medium">{t('routeModeOverride.label')}</div>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('routeModeOverride.description')}</p>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('routeModeOverride.movedHint')}</p>
                </div>
            </div>
        </div>
    );
}

export function Setting() {
    return (
        <div className="h-full min-h-0 overflow-y-auto overscroll-contain rounded-t-3xl">
            <div className="space-y-4 pb-24 md:pb-4">
                {/* 配对两列网格：成对卡片并排对齐；窄屏自动堆叠为单列 */}
                <PageWrapper className="grid grid-cols-1 items-start gap-4 md:grid-cols-2">
                    <SettingInfo key="setting-info" />
                    <SettingLog key="setting-log" />
                    <SettingAppearance key="setting-appearance" />
                    <SettingCircuitBreaker key="setting-circuit-breaker" />
                    <SettingAccount key="setting-account" />
                    <SettingAccessToken key="setting-access-token" />
                    <SettingBackup key="setting-backup" />
                    <SettingLLMSync key="setting-llmsync" />
                </PageWrapper>
                <SettingRouteModeMoved />
                {/* 系统设置内容最多，单独横跨整行（左右两边） */}
                <SettingSystem />
                {/* 指纹 Profile 管理：字段多、配合 cloak.profile_id 选用，单独横跨整行 */}
                <SettingFingerprintProfile />
            </div>
        </div>
    );
}
