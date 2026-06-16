import { lazyWithPreload } from './lazy-with-preload';
import { lazy, ComponentType } from 'react';
import type { LucideIcon } from 'lucide-react';
import { BadgeDollarSign, Home, Radio, FolderTree, Settings, Logs, KeyRound, Users, GitBranch, MessageSquareText, FlaskConical, Database } from 'lucide-react';

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
const Channel_Module = lazyWithPreload(() => import('@/components/modules/channel').then(m => ({ default: m.Channel })));
const Model_Module = lazyWithPreload(() => import('@/components/modules/model').then(m => ({ default: m.Model })));
const ModelTest_Module = lazyWithPreload(() => import('@/components/modules/model-test').then(m => ({ default: m.ModelTest })));
const Migration_Module = lazyWithPreload(() => import('@/components/modules/migration').then(m => ({ default: m.Migration })));
const Group_Module = lazyWithPreload(() => import('@/components/modules/group').then(m => ({ default: m.Group })));
const AccessPlan_Module = lazyWithPreload(() => import('@/components/modules/access-plan').then(m => ({ default: m.AccessPlan })));
const Log_Module = lazyWithPreload(() => import('@/components/modules/log').then(m => ({ default: m.Log })));
const Setting_Module = lazyWithPreload(() => import('@/components/modules/setting').then(m => ({ default: m.Setting })));
const Key_Module = lazyWithPreload(() => import('@/components/modules/key').then(m => ({ default: m.Key })));
const Prompt_Module = lazyWithPreload(() => import('@/components/modules/prompt').then(m => ({ default: m.PromptManagement })));
const User_Module = lazyWithPreload(() => import('@/components/modules/user').then(m => ({ default: m.UserManagement })));

export const ROUTES: RouteConfig[] = [
    { id: 'home', label: 'Home', icon: Home, component: Home_Module },
    { id: 'user', label: 'Users', icon: Users, component: User_Module },
    { id: 'key', label: 'API Key', icon: KeyRound, component: Key_Module },
    { id: 'channel', label: 'Channel', icon: Radio, component: Channel_Module },
    { id: 'access-plan', label: 'Plans', icon: GitBranch, component: AccessPlan_Module },
    { id: 'group', label: 'Model Pool', icon: FolderTree, component: Group_Module },
    { id: 'model', label: 'Model', icon: BadgeDollarSign, component: Model_Module },
    { id: 'model-test', label: 'Model Test', icon: FlaskConical, component: ModelTest_Module },
    { id: 'migration', label: 'Migration', icon: Database, component: Migration_Module },
    { id: 'prompt', label: 'Prompt', icon: MessageSquareText, component: Prompt_Module },
    { id: 'log', label: 'Log', icon: Logs, component: Log_Module },
    { id: 'setting', label: 'Setting', icon: Settings, component: Setting_Module },
];

export const CONTENT_MAP = ROUTES.reduce((acc, route) => {
    acc[route.id] = route.component;
    return acc;
}, {} as Record<string, LazyComponent>);

// 模型池(group)不再出现在导航：渠道模型会自动建同名 group 兜底，普通管理流程
// 不需要手动模型池页。组件定义保留在 ROUTES 里以便兼容/直链，只是不进导航。
export const ADMIN_ROUTE_IDS = ['home', 'user', 'key', 'channel', 'access-plan', 'model', 'model-test', 'migration', 'prompt', 'log', 'setting'] as const;
export const USER_ROUTE_IDS = ['home', 'key', 'log'] as const;

export function routeIdsForRole(role?: string | null) {
    return role === 'admin' ? ADMIN_ROUTE_IDS : USER_ROUTE_IDS;
}

export function routesForRole(role?: string | null) {
    const ids = new Set<string>(routeIdsForRole(role));
    return ROUTES.filter((route) => ids.has(route.id));
}
