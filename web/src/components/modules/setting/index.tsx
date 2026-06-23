'use client';

import { PageWrapper } from '@/components/common/PageWrapper';
import { SettingAppearance } from './Appearance';
import { SettingSystem } from './System';
import { SettingFingerprintProfile } from './FingerprintProfile';
import { SettingAccount } from './Account';
import { SettingInfo } from './Info';
import { SettingLLMSync } from './LLMSync';
import { SettingLog } from './Log';
import { SettingBackup } from './Backup';
import { SettingCircuitBreaker } from './CircuitBreaker';

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
                    <SettingBackup key="setting-backup" />
                    <SettingLLMSync key="setting-llmsync" />
                </PageWrapper>
                {/* 系统设置内容最多，单独横跨整行（左右两边） */}
                <SettingSystem />
                {/* 指纹 Profile 管理：字段多、配合 cloak.profile_id 选用，单独横跨整行 */}
                <SettingFingerprintProfile />
            </div>
        </div>
    );
}
