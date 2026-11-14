"use client"

import * as React from "react"
import * as SwitchPrimitive from "@radix-ui/react-switch"

import { cn } from "@/lib/utils"

function Switch({
  className,
  ...props
}: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer group/switch relative inline-flex h-[1.15rem] w-8 shrink-0 items-center rounded-full outline-none transition-all focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50 max-sm:h-11 max-sm:w-11",
        className
      )}
      {...props}
    >
      {/* 画出来的轨道（非交互）：桌面铺满 Root（约32x18），手机居中同一块轨道，
          Root 本体扩展为 44x44 的真实命中区。data-state 样式经由 group/switch 传递。 */}
      <span
        aria-hidden
        className={cn(
          "pointer-events-none absolute inset-0 rounded-full border border-transparent shadow-xs transition-all group-data-[state=checked]/switch:bg-primary group-data-[state=unchecked]/switch:bg-input dark:group-data-[state=unchecked]/switch:bg-input/80",
          "max-sm:inset-auto max-sm:left-1/2 max-sm:top-1/2 max-sm:h-[1.15rem] max-sm:w-8 max-sm:-translate-x-1/2 max-sm:-translate-y-1/2"
        )}
      />
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className={cn(
          "bg-background dark:data-[state=unchecked]:bg-foreground dark:data-[state=checked]:bg-primary-foreground pointer-events-none relative z-10 block size-4 rounded-full ring-0 transition-transform data-[state=checked]:translate-x-[calc(100%-2px)] data-[state=unchecked]:translate-x-0",
          "max-sm:absolute max-sm:left-1.5 max-sm:top-1/2 max-sm:-translate-y-1/2"
        )}
      />
    </SwitchPrimitive.Root>
  )
}

export { Switch }
