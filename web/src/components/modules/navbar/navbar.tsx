"use client"

import { Fragment, useEffect, useRef } from "react"
import { motion } from "motion/react"
import { cn } from "@/lib/utils"
import { useNavStore, type NavItem } from "./nav-store"
import { navGroupsForRole, routesForRole } from "@/route/config"
import { usePreload } from "@/route/use-preload"
import { ENTRANCE_VARIANTS } from "@/lib/animations/fluid-transitions"
import { useAuthStore } from "@/api/endpoints/user"
import { useTranslations } from "next-intl"

interface NavBarProps {
    activeItem?: NavItem;
    setActiveItem?: (item: NavItem) => void;
}

export function NavBar({ activeItem: controlledActiveItem, setActiveItem: controlledSetActiveItem }: NavBarProps = {}) {
    const { activeItem: storeActiveItem, setActiveItem: storeSetActiveItem } = useNavStore()
    const activeItem = controlledActiveItem ?? storeActiveItem
    const setActiveItem = controlledSetActiveItem ?? storeSetActiveItem
    const { preload } = usePreload()
    const role = useAuthStore((state) => state.user?.role)
    const routes = routesForRole(role)
    const groups = navGroupsForRole(role)
    const t = useTranslations('navbar')
    const navRef = useRef<HTMLElement>(null)

    // 手机底栏放不下全部图标、会横向滚动：切页后把当前项滚进可视区，免得高亮的那一项被挡在屏幕外。
    useEffect(() => {
        const nav = navRef.current
        if (!nav || nav.scrollWidth <= nav.clientWidth) return
        const active = nav.querySelector<HTMLElement>('[aria-current="page"]')
        active?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
    }, [activeItem])
    const handleRouteSelect = (item: NavItem) => {
        void preload(item).catch(() => undefined).finally(() => setActiveItem(item))
    }

    // 管理员桌面侧栏按组渲染：先给组名行（仅桌面显示），再渲染组内路由；
    // 非管理员保持原有扁平列表。组内顺序 = config 组内顺序，从 routesForRole 结果按 id 对齐取，
    // 查不到的 id 直接跳过；routes 为空时各组无人渲染，不会崩。
    const sections: { labelKey: string | null; routes: typeof routes }[] = groups
        ? groups.map((group) => ({
              labelKey: group.labelKey,
              routes: group.routeIds.flatMap((id) => {
                  const route = routes.find((item) => item.id === id)
                  return route ? [route] : []
              }),
          }))
        : [{ labelKey: null, routes }]

    return (
        <div className="relative z-50 md:min-h-screen">
            <motion.nav
                ref={navRef}
                aria-label="Main Navigation"
                className={cn(
                    "fixed bottom-[calc(1rem+env(safe-area-inset-bottom))] left-1/2 flex max-w-[calc(100vw-1rem)] -translate-x-1/2 items-center gap-1 overflow-x-auto p-2",
                    "md:sticky md:top-30 md:left-auto md:bottom-auto md:max-w-none md:translate-x-0 md:flex-col md:items-stretch md:gap-0.5 md:max-h-[calc(100vh-9rem)] md:overflow-y-auto md:p-3",
                    "bg-sidebar/95 text-sidebar-foreground border border-sidebar-border rounded-3xl backdrop-blur-xl supports-[backdrop-filter]:bg-sidebar/80",
                    "custom-shadow"
                )}
                variants={ENTRANCE_VARIANTS.navbar}
                initial="initial"
                animate="animate"
            >
                {sections.map((section, sectionIndex) => (
                    <Fragment key={section.labelKey ?? `flat-${sectionIndex}`}>
                        {section.labelKey && (
                            <div className="hidden px-3 pt-3 pb-1 text-[10px] text-muted-foreground/60 md:block">
                                {t(section.labelKey)}
                            </div>
                        )}
                        {section.routes.map((route, index) => {
                            const flatIndex =
                                sections.slice(0, sectionIndex).reduce((sum, s) => sum + s.routes.length, 0) + index
                            const isActive = activeItem === route.id
                            const label = t(route.id)
                            return (
                                <motion.button
                                    key={route.id}
                                    type="button"
                                    onClick={() => handleRouteSelect(route.id as NavItem)}
                                    onMouseEnter={() => preload(route.id)}
                                    aria-label={label}
                                    aria-current={isActive ? 'page' : undefined}
                                    title={label}
                                    className={cn(
                                        "relative z-20 flex h-11 w-11 shrink-0 items-center justify-center gap-2 overflow-hidden rounded-2xl p-0 text-left md:h-auto md:w-36 md:justify-start md:px-3 md:py-1.5",
                                        isActive ? "text-sidebar-primary-foreground" : "text-sidebar-foreground/60 hover:bg-sidebar-accent"
                                    )}
                                    initial={{ opacity: 0, scale: 0.8 }}
                                    animate={{
                                        opacity: 1,
                                        scale: 1,
                                        transition: {
                                            delay: flatIndex * 0.05,
                                            duration: 0.3,
                                        }
                                    }}
                                    whileHover={{ scale: 1.05, zIndex: 30 }}
                                    whileTap={{ scale: 0.95 }}
                                >
                                    {isActive && (
                                        <motion.div
                                            layoutId="navbar-indicator"
                                            className="absolute inset-0 bg-sidebar-primary rounded-2xl z-0"
                                            transition={{ type: "spring", stiffness: 300, damping: 30 }}
                                        />
                                    )}
                                    <span className="relative z-10">
                                        <route.icon strokeWidth={2} />
                                    </span>
                                    <span className="relative z-10 hidden min-w-0 whitespace-nowrap text-sm font-medium md:block">
                                        {label}
                                    </span>
                                </motion.button>
                            )
                        })}
                    </Fragment>
                ))}
            </motion.nav>
        </div>
    )
}
