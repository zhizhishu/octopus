'use client';

import { useEffect, useState, type ReactNode } from 'react';
import { useTranslations } from 'next-intl';
import { Fingerprint, LifeBuoy, Monitor, ScrollText, User, UserPlus, Zap, type LucideIcon } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { PillTabButton } from '@/components/common/PillTabButton';
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
import { SettingUserAccessAlert } from './UserAccessAlert';
import {
    DEFAULT_SETTING_TAB,
    SETTING_TAB_CARD_COUNTS,
    SETTING_TAB_IDS,
    clearSettingTabHash,
    parseSettingTabHash,
    writeSettingTabHash,
    type SettingTabId,
} from './tabs';

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

const TAB_ICONS: Record<SettingTabId, LucideIcon> = {
    general: User,
    relay: LifeBuoy,
    identity: Fingerprint,
    users: UserPlus,
    system: Monitor,
    logs: ScrollText,
};

const TWO_COLUMNS = 'grid grid-cols-1 items-start gap-4 md:grid-cols-2';

// 每个标签一组「行」：行内两列，窄屏自动单列；需要横跨整行的卡片单独占一行。
// 卡片数要和 tabs.ts 的 SETTING_TAB_CARD_COUNTS 对得上（有单测核对）。
function renderTabRows(tab: SettingTabId): ReactNode[] {
    switch (tab) {
        case 'general':
            return [
                <div key="general" className={TWO_COLUMNS}>
                    <div className="grid min-w-0 gap-4">
                        <SettingInfo />
                        <SettingAccount />
                    </div>
                    <div className="grid min-w-0 gap-4">
                        <SettingAppearance />
                        <SettingAccessToken />
                    </div>
                </div>,
            ];
        case 'relay':
            return [
                <div key="breaker" className={TWO_COLUMNS}>
                    <SettingCircuitBreaker />
                    <SettingLog section="rescue" />
                </div>,
                <SettingSystem key="stream" section="stream" />,
                <div key="errors" className={TWO_COLUMNS}>
                    <SettingSystem section="upstreamError" />
                    <SettingRouteModeMoved />
                </div>,
            ];
        case 'identity':
            return [
                <div key="headers" className={TWO_COLUMNS}>
                    <SettingSystem section="claude" />
                    <SettingSystem section="codex" />
                </div>,
                <SettingSystem key="cache" section="cache" />,
                <SettingSystem key="beta" section="claudeBetaStrip" />,
                <SettingFingerprintProfile key="profiles" />,
            ];
        case 'users':
            return [
                <div key="users" className={TWO_COLUMNS}>
                    <SettingSystem section="registration" />
                    <SettingSystem section="checkIn" />
                </div>,
                <SettingUserAccessAlert key="access" />,
            ];
        case 'system':
            return [
                <div key="system" className={TWO_COLUMNS}>
                    <SettingSystem section="network" />
                    <div className="grid min-w-0 gap-4">
                        <SettingLLMSync />
                        <SettingSystem section="stats" />
                    </div>
                </div>,
            ];
        case 'logs':
            return [
                <div key="logs" className={TWO_COLUMNS}>
                    <SettingLog section="log" />
                    <SettingBackup />
                </div>,
            ];
    }
}

function readInitialTab(): SettingTabId {
    if (typeof window === 'undefined') return DEFAULT_SETTING_TAB;
    return parseSettingTabHash(window.location.hash);
}

export function Setting() {
    const t = useTranslations('setting');
    const [tab, setTab] = useState<SettingTabId>(readInitialTab);

    // 地址栏 hash 是标签的「外部入口」：别人分享的 #relay 链接、手动改地址都能跳到对应标签。
    useEffect(() => {
        const onHashChange = () => setTab(parseSettingTabHash(window.location.hash));
        window.addEventListener('hashchange', onHashChange);
        return () => {
            window.removeEventListener('hashchange', onHashChange);
            clearSettingTabHash(window.location, window.history);
        };
    }, []);

    // 窄屏下标签栏横向滚动：当前标签（含从 #hash 直接打开的）要滚进可视区，不然看不出选中了哪个。
    useEffect(() => {
        if (typeof document === 'undefined') return;
        document.getElementById(`setting-tab-${tab}`)?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }, [tab]);

    const selectTab = (next: SettingTabId) => {
        setTab(next);
        writeSettingTabHash(window.location, window.history, next);
    };

    return (
        <div className="flex h-full min-h-0 flex-col gap-3">
            <div className="shrink-0">
                <div className="overflow-x-auto pb-1">
                    <div
                        role="tablist"
                        aria-label={t('tabs.ariaLabel')}
                        className="flex w-max min-w-full items-center gap-1 rounded-lg border border-border bg-card px-1.5 py-1 shadow-xs"
                    >
                        {SETTING_TAB_IDS.map((id) => {
                            const Icon = TAB_ICONS[id];
                            return (
                                <PillTabButton
                                    key={id}
                                    role="tab"
                                    id={`setting-tab-${id}`}
                                    controls={`setting-panel-${id}`}
                                    icon={<Icon className="size-3.5 shrink-0" />}
                                    label={t(`tabs.${id}.label`)}
                                    count={SETTING_TAB_CARD_COUNTS[id]}
                                    active={tab === id}
                                    onClick={() => selectTab(id)}
                                />
                            );
                        })}
                    </div>
                </div>
                <p className="mt-1 px-1 text-xs text-muted-foreground">{t(`tabs.${tab}.description`)}</p>
            </div>

            <div
                role="tabpanel"
                id={`setting-panel-${tab}`}
                aria-labelledby={`setting-tab-${tab}`}
                className="min-h-0 flex-1 overflow-y-auto overscroll-contain rounded-t-3xl"
            >
                <PageWrapper key={tab} className="space-y-4 pb-24 md:pb-4">
                    {renderTabRows(tab)}
                </PageWrapper>
            </div>
        </div>
    );
}
