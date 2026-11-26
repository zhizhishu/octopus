import { lazyWithPreload } from './lazy-with-preload';
import { lazy, ComponentType } from 'react';
import type { LucideIcon } from 'lucide-react';
import { BadgeDollarSign, ChartColumn, Home, Radio, Settings, Logs, KeyRound, Users, GitBranch, MessageSquareText, Database, Shield, EyeOff, LayoutDashboard, Plug } from 'lucide-react';

export type LazyComponent = ReturnType<typeof lazy> & {
    preload: () => Promise<{ default: ComponentType<Record<string, never>> }>
};

export interface RouteConfig {
    id: string;
    label: string;
    icon: LucideIcon;
    component: LazyComponent;
}

const Home_Module = lazyWithPreload(() => import('@/components/modules/home').then(m => ({ default: m.Home })));
const Dashboard_Module = lazyWithPreload(() => import('@/components/modules/dashboard').then(m => ({ default: m.Dashboard })));
const Channel_Module = lazyWithPreload(() => import('@/components/modules/channel').then(m => ({ default: m.Channel })));
const Redact_Module = lazyWithPreload(() => import('@/components/modules/redact').then(m => ({ default: m.Redact })));
const Model_Module = lazyWithPreload(() => import('@/components/modules/model').then(m => ({ default: m.Model })));
const Migration_Module = lazyWithPreload(() => import('@/components/modules/migration').then(m => ({ default: m.Migration })));
const AccessPlan_Module = lazyWithPreload(() => import('@/components/modules/access-plan').then(m => ({ default: m.AccessPlan })));
const Log_Module = lazyWithPreload(() => import('@/components/modules/log').then(m => ({ default: m.Log })));
const Setting_Module = lazyWithPreload(() => import('@/components/modules/setting').then(m => ({ default: m.Setting })));
const Key_Module = lazyWithPreload(() => import('@/components/modules/key').then(m => ({ default: m.Key })));
const Prompt_Module = lazyWithPreload(() => import('@/components/modules/prompt').then(m => ({ default: m.PromptManagement })));
const Audit_Module = lazyWithPreload(() => import('@/components/modules/model-audit').then(m => ({ default: m.ModelAudit })));
const User_Module = lazyWithPreload(() => import('@/components/modules/user').then(m => ({ default: m.UserManagement })));
const MCP_Module = lazyWithPreload(() => import('@/components/modules/mcp').then(m => ({ default: m.MCP })));
const Usage_Module = lazyWithPreload(() => import('@/components/modules/usage').then(m => ({ default: m.Usage })));

export const ROUTES: RouteConfig[] = [
    { id: 'home', label: 'Home', icon: Home, component: Home_Module },
    { id: 'dashboard', label: 'Dashboard', icon: LayoutDashboard, component: Dashboard_Module },
    { id: 'usage', label: 'Usage', icon: ChartColumn, component: Usage_Module },
    { id: 'user', label: 'Users', icon: Users, component: User_Module },
    { id: 'key', label: 'API Key', icon: KeyRound, component: Key_Module },
    { id: 'access-plan', label: 'Plans', icon: GitBranch, component: AccessPlan_Module },
    { id: 'model', label: 'Model', icon: BadgeDollarSign, component: Model_Module },
    { id: 'channel', label: 'Channel', icon: Radio, component: Channel_Module },
    { id: 'redact', label: 'Redact', icon: EyeOff, component: Redact_Module },
    { id: 'migration', label: 'Migration', icon: Database, component: Migration_Module },
    { id: 'mcp', label: 'MCP', icon: Plug, component: MCP_Module },
    { id: 'prompt', label: 'Prompt', icon: MessageSquareText, component: Prompt_Module },
    { id: 'audit', label: 'Audit', icon: Shield, component: Audit_Module },
    { id: 'log', label: 'Log', icon: Logs, component: Log_Module },
    { id: 'setting', label: 'Setting', icon: Settings, component: Setting_Module },
];

export const CONTENT_MAP = ROUTES.reduce((acc, route) => {
    acc[route.id] = route.component;
    return acc;
}, {} as Record<string, LazyComponent>);

// 'dashboard'（仪表盘）只给管理员：它的接口是全站口径、AdminOnly。
export const ADMIN_ROUTE_IDS = ['home', 'dashboard', 'usage', 'user', 'key', 'access-plan', 'model', 'channel', 'redact', 'migration', 'mcp', 'prompt', 'audit', 'log', 'setting'] as const;
export const USER_ROUTE_IDS = ['home', 'usage', 'key', 'log', 'mcp'] as const;

export function routeIdsForRole(role?: string | null) {
    return role === 'admin' ? ADMIN_ROUTE_IDS : USER_ROUTE_IDS;
}

// 按角色出路由，**顺序以 routeIdsForRole 为准**（不是 ROUTES 的声明顺序）：
// ROUTES 是「有哪些路由」的注册表，18bd520f 为了管理员侧栏分组重排过它（mcp 排到了 log
// 前面），但普通用户手机底栏的顺序是 USER_ROUTE_IDS 定的——home,usage,key,log,mcp。
// 直接用 ROUTES.filter 会把底栏变成 home,usage,key,mcp,log（日志和 MCP 被对调）。
// 管理员侧栏不受影响：navbar 的 navGroupsForRole 分组渲染按 ADMIN_NAV_GROUPS 里各组的
// routeIds 逐个取，只把本结果当 id -> 路由的查表用。
export function routesForRole(role?: string | null) {
    const byId = new Map<string, RouteConfig>();
    for (const route of ROUTES) byId.set(route.id, route);
    const ordered: RouteConfig[] = [];
    for (const id of routeIdsForRole(role)) {
        const route = byId.get(id);
        if (route) ordered.push(route);
    }
    return ordered;
}

// 管理员侧栏按业务分组渲染：组序即 ADMIN_ROUTE_IDS 的分组重排，
// 各组 id 合集与原权限集合完全一致（一个 id 不多不少）。
export interface NavGroupConfig { labelKey: string; routeIds: readonly string[] }
export const ADMIN_NAV_GROUPS: readonly NavGroupConfig[] = [
    { labelKey: 'groupOverview', routeIds: ['home', 'dashboard', 'usage'] },
    { labelKey: 'groupAccess', routeIds: ['user', 'key', 'access-plan', 'model'] },
    { labelKey: 'groupUpstream', routeIds: ['channel', 'redact', 'migration', 'mcp'] },
    { labelKey: 'groupControl', routeIds: ['prompt', 'audit', 'log'] },
    { labelKey: 'groupSetting', routeIds: ['setting'] },
];
export function navGroupsForRole(role?: string | null): readonly NavGroupConfig[] | null { return role === 'admin' ? ADMIN_NAV_GROUPS : null; }
