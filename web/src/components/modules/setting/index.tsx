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
import { useRouteModeOverrideSetting, RouteModeOverrideValue } from '@/api/endpoints/setting';

function SettingRouteModeOverride() {
    const t = useTranslations('setting');
    const { value, update, isPending } = useRouteModeOverrideSetting();

    return (
        <div className="rounded-3xl border border-border bg-card p-6">
            <div className="flex items-center justify-between gap-4">
                <div className="flex min-w-0 items-center gap-3">
                    <Zap className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('routeModeOverride.label')}</span>
                </div>
                <select
                    value={value}
                    onChange={(event) => update(event.target.value as RouteModeOverrideValue)}
                    disabled={isPending}
                    aria-label={t('routeModeOverride.label')}
                    className="h-9 w-48 shrink-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                >
                    <option value="spread">{t('routeModeOverride.spread')}</option>
                    <option value="fill_first">{t('routeModeOverride.fillFirst')}</option>
                </select>
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
                <SettingRouteModeOverride />
                {/* 系统设置内容最多，单独横跨整行（左右两边） */}
                <SettingSystem />
                {/* 指纹 Profile 管理：字段多、配合 cloak.profile_id 选用，单独横跨整行 */}
                <SettingFingerprintProfile />
            </div>
        </div>
    );
}
