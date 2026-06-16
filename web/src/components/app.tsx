
'use client';

import { Suspense, useState, useEffect, useRef } from 'react';
import { motion, AnimatePresence } from "motion/react"
import { useAuth } from '@/api/endpoints/user';
import { LoginForm } from '@/components/modules/login';
import { APIKeyDashboard } from '@/components/modules/apikey-dashboard';
import { ContentLoader } from '@/route/content-loader';
import { NavBar } from './modules/navbar/navbar';
import { useNavStore } from './modules/navbar/nav-store';
import { useTranslations } from 'next-intl'
import Logo, { LOGO_DRAW_END_MS } from '@/components/modules/logo';
import { Toolbar } from '@/components/modules/toolbar';
import { CommandPalette, CommandSearchButton } from '@/components/modules/command/command-palette';
import { ENTRANCE_VARIANTS } from '@/lib/animations/fluid-transitions';
import { useQueryClient } from '@tanstack/react-query';
import { CONTENT_MAP, routeIdsForRole } from '@/route';
import { apiClient } from '@/api/client';
import { logger } from '@/lib/logger';
import { Button } from '@/components/ui/button';

function timeout(ms: number) {
    return new Promise<void>((resolve) => setTimeout(resolve, ms));
}

export function AppContainer() {
    const { isAuthenticated, isAPIKeyAuth, isLoading: authLoading, user, isAdmin } = useAuth();
    const { activeItem, direction, setActiveItem } = useNavStore();
    const t = useTranslations('navbar');
    const loginT = useTranslations('login.button');
    const queryClient = useQueryClient();
    const [showLogin, setShowLogin] = useState(false);

    // Logo 动画完成状态
    const [logoAnimationComplete, setLogoAnimationComplete] = useState(false);
    const [bootstrapComplete, setBootstrapComplete] = useState(false);
    const bootstrapStartedRef = useRef(false);

    // 首屏最早的 server-rendered loader：一旦客户端开始渲染，就淡出移除
    useEffect(() => {
        const el = document.getElementById('initial-loader');
        if (!el) return;

        el.classList.add('octo-hide');
        const timer = setTimeout(() => el.remove(), 220);
        return () => clearTimeout(timer);
    }, []);

    useEffect(() => {
        const timer = setTimeout(() => setLogoAnimationComplete(true), LOGO_DRAW_END_MS);
        return () => clearTimeout(timer);
    }, []);

    useEffect(() => {
        if (!isAuthenticated || isAPIKeyAuth) return;
        const allowed = routeIdsForRole(user?.role);
        if (!(allowed as readonly string[]).includes(activeItem)) {
            setActiveItem('home');
        }
    }, [activeItem, isAPIKeyAuth, isAuthenticated, setActiveItem, user?.role]);

    useEffect(() => {
        if (authLoading) return;
        if (!isAuthenticated) {
            setBootstrapComplete(true);
            return;
        }

        if (bootstrapStartedRef.current) return;
        bootstrapStartedRef.current = true;

        let cancelled = false;

        (async () => {
            try {
                const prefetches: Array<Promise<unknown>> = [];

                // API Key 认证模式：预取 dashboard stats
                if (isAPIKeyAuth) {
                    prefetches.push(
                        queryClient.prefetchQuery({
                            queryKey: ['apikey', 'dashboard', 'stats'],
                            queryFn: async () => apiClient.get('/api/v1/apikey/stats'),
                        })
                    );
                } else {
                    // 普通用户认证模式：预取对应页面数据
                    const allowed = routeIdsForRole(user?.role);
                    const safeActiveItem = (allowed as readonly string[]).includes(activeItem) ? activeItem : 'home';
                    const component = CONTENT_MAP[safeActiveItem];
                    if (component?.preload) {
                        prefetches.push(component.preload());
                    }

                    switch (safeActiveItem) {
                        case 'home': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['stats', 'total'],
                                    queryFn: async () => apiClient.get('/api/v1/stats/total'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['stats', 'daily'],
                                    queryFn: async () => apiClient.get('/api/v1/stats/daily'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['stats', 'hourly'],
                                    queryFn: async () => apiClient.get('/api/v1/stats/hourly'),
                                })
                            );
                            if (isAdmin) {
                                prefetches.push(
                                    queryClient.prefetchQuery({
                                        queryKey: ['channels', 'list'],
                                        queryFn: async () => apiClient.get('/api/v1/channel/list'),
                                    })
                                );
                            }
                            break;
                        }
                        case 'user': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['users', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/user/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['redeem', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/redeem/list'),
                                })
                            );
                            break;
                        }
                        case 'key': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['apikeys', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/apikey/list'),
                                })
                            );
                            break;
                        }
                        case 'prompt': {
                            if (isAdmin) {
                                prefetches.push(
                                    queryClient.prefetchQuery({
                                        queryKey: ['settings', 'list'],
                                        queryFn: async () => apiClient.get('/api/v1/setting/list'),
                                    })
                                );
                                prefetches.push(
                                    queryClient.prefetchQuery({
                                        queryKey: ['channels', 'list'],
                                        queryFn: async () => apiClient.get('/api/v1/channel/list'),
                                    })
                                );
                                prefetches.push(
                                    queryClient.prefetchQuery({
                                        queryKey: ['access-plans', 'list'],
                                        queryFn: async () => apiClient.get('/api/v1/access-plan/list'),
                                    })
                                );
                            }
                            break;
                        }
                        case 'channel': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['channels', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/channel/list'),
                                })
                            );
                            break;
                        }
                        case 'group': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['groups', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/group/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['models', 'channel'],
                                    queryFn: async () => apiClient.get('/api/v1/model/channel'),
                                })
                            );
                            break;
                        }
                        case 'access-plan': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['access-plans', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/access-plan/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['channels', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/channel/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['models', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/model/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['models', 'channel'],
                                    queryFn: async () => apiClient.get('/api/v1/model/channel'),
                                })
                            );
                            break;
                        }
                        case 'model': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['models', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/model/list'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['models', 'last-update-time'],
                                    queryFn: async () => apiClient.get('/api/v1/model/last-update-time'),
                                })
                            );
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['settings', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/setting/list'),
                                })
                            );
                            break;
                        }
                        case 'migration': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['migration', 'newapi', 'jobs'],
                                    queryFn: async () => apiClient.get('/api/v1/migration/newapi/jobs'),
                                })
                            );
                            break;
                        }
                        case 'setting': {
                            prefetches.push(
                                queryClient.prefetchQuery({
                                    queryKey: ['settings', 'list'],
                                    queryFn: async () => apiClient.get('/api/v1/setting/list'),
                                })
                            );
                            break;
                        }
                        default:
                            break;
                    }
                }

                await Promise.race([
                    Promise.allSettled(prefetches),
                    timeout(5000),
                ]);
            } catch (e) {
                logger.warn('bootstrap prefetch failed:', e);
            } finally {
                if (!cancelled) setBootstrapComplete(true);
            }
        })();

        return () => {
            cancelled = true;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [activeItem, authLoading, isAdmin, isAuthenticated, user?.role]);

    // 加载状态
    const isLoading =
        authLoading ||
        !logoAnimationComplete ||
        (isAuthenticated && !bootstrapComplete);

    // 加载页面
    if (isLoading) {
        return (
            <div className="min-h-screen flex items-center justify-center bg-background">
                <Logo size={120} animate />
            </div>
        );
    }

    // API Key 认证模式 - 显示 API Key Dashboard
    if (isAPIKeyAuth) {
        return (
            <AnimatePresence mode="wait">
                <APIKeyDashboard key="apikey-dashboard" />
            </AnimatePresence>
        );
    }

    if (!isAuthenticated && showLogin) {
        return (
            <AnimatePresence mode="wait">
                <LoginForm key="login" onLoginSuccess={() => setShowLogin(false)} />
            </AnimatePresence>
        );
    }

    const allowed = routeIdsForRole(user?.role);
    const activeRoute = isAuthenticated
        ? ((allowed as readonly string[]).includes(activeItem) ? activeItem : 'home')
        : 'home';
    const activeRouteTitle = t(activeRoute);

    // 主界面
    return (
        <motion.div
            key={isAuthenticated ? 'main-app' : 'public-home'}
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            transition={{ duration: 0.3 }}
            className={`mx-auto flex h-dvh w-full max-w-6xl min-w-0 flex-col overflow-hidden px-3 sm:px-4 md:px-6 ${isAuthenticated ? 'md:grid md:grid-cols-[auto_1fr] md:gap-6' : ''}`}
        >
            {isAuthenticated && <NavBar activeItem={activeItem} setActiveItem={setActiveItem} />}
            {isAuthenticated && <CommandPalette />}
            <main className="flex min-h-0 w-full min-w-0 flex-1 flex-col">
                <header className="my-4 flex flex-none items-center gap-x-2 px-1 sm:px-2 md:my-6">
                    <Logo size={48} />
                    <div className="flex-1 overflow-hidden">
                        <motion.div
                            key={activeRoute}
                            custom={direction}
                            variants={{
                                initial: (direction: number) => ({
                                    y: 32 * direction,
                                    opacity: 0
                                }),
                                animate: {
                                    y: 0,
                                    opacity: 1
                                },
                            }}
                            initial="initial"
                            animate="animate"
                            transition={{ duration: 0.3 }}
                            className="flex min-w-0 items-center"
                        >
                            <span className="mt-1 truncate text-2xl font-bold md:text-3xl">{activeRouteTitle}</span>
                        </motion.div>
                    </div>
                    <div className="ml-auto flex shrink-0 items-center gap-1">
                        {isAuthenticated ? (
                            <>
                                <CommandSearchButton />
                                <Toolbar />
                            </>
                        ) : (
                            <Button variant="outline" size="sm" className="rounded-xl" onClick={() => setShowLogin(true)}>
                                {loginT('submit')}
                            </Button>
                        )}
                    </div>
                </header>
                <motion.div
                    key={activeRoute}
                    variants={ENTRANCE_VARIANTS.content}
                    initial="initial"
                    animate="animate"
                    transition={{ duration: 0.25 }}
                    className="h-full min-h-0 flex-1"
                >
                    <Suspense fallback={<div className="flex h-full min-h-64 items-center justify-center text-sm text-muted-foreground">加载中...</div>}>
                        <ContentLoader activeRoute={activeRoute} />
                    </Suspense>
                </motion.div>
            </main>
        </motion.div>
    );
}
