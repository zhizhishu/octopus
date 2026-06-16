"use client"

import { motion } from "motion/react"
import { cn } from "@/lib/utils"
import { useNavStore, type NavItem } from "./nav-store"
import { routesForRole } from "@/route/config"
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
    const t = useTranslations('navbar')
    const handleRouteSelect = (item: NavItem) => {
        void preload(item).catch(() => undefined).finally(() => setActiveItem(item))
    }

    return (
        <div className="relative z-50 md:min-h-screen">
            <motion.nav
                aria-label="Main Navigation"
                className={cn(
                    "fixed bottom-[calc(1rem+env(safe-area-inset-bottom))] left-1/2 flex max-w-[calc(100vw-1rem)] -translate-x-1/2 items-center gap-1 overflow-x-auto p-2",
                    "md:sticky md:top-30 md:left-auto md:bottom-auto md:max-w-none md:translate-x-0 md:flex-col md:items-stretch md:gap-2 md:overflow-visible md:p-3",
                    "bg-sidebar text-sidebar-foreground border border-sidebar-border rounded-3xl",
                    "custom-shadow"
                )}
                variants={ENTRANCE_VARIANTS.navbar}
                initial="initial"
                animate="animate"
            >
                {routes.map((route, index) => {
                    const isActive = activeItem === route.id
                    const label = t(route.id)
                    return (
                        <motion.button
                            key={route.id}
                            type="button"
                            onClick={() => handleRouteSelect(route.id as NavItem)}
                            onMouseEnter={() => preload(route.id)}
                            aria-label={label}
                            title={label}
                            className={cn(
                                "relative z-20 flex h-10 w-10 shrink-0 items-center justify-center gap-2 overflow-hidden rounded-2xl p-0 text-left md:h-auto md:w-32 md:justify-start md:px-3 md:py-2.5",
                                isActive ? "text-sidebar-primary-foreground" : "text-sidebar-foreground/60 hover:bg-sidebar-accent"
                            )}
                            initial={{ opacity: 0, scale: 0.8 }}
                            animate={{
                                opacity: 1,
                                scale: 1,
                                transition: {
                                    delay: index * 0.05,
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
                            <span className="relative z-10 hidden min-w-0 truncate text-sm font-medium md:block">
                                {label}
                            </span>
                        </motion.button>
                    )
                })}
            </motion.nav>
        </div>
    )
}
