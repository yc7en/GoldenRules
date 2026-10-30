import * as React from "react"
import { cn } from "@/lib/utils"

// 受控 Tabs。此前这里只渲染了一层 div，`TabsContent` 收到 value 直接丢掉，
// 结果是「三个面板同时显示、点 tab 只换按钮高亮」—— 看起来像分区没做，
// 实际是不分区。现在用 context 把当前值传下去，`TabsContent` 只显示匹配的那一块。

interface TabsCtxValue {
  value?: string
  setValue: (v: string) => void
}

const TabsCtx = React.createContext<TabsCtxValue>({ setValue: () => {} })

interface TabsProps
  extends Omit<React.HTMLAttributes<HTMLDivElement>, "onChange"> {
  value?: string
  defaultValue?: string
  onValueChange?: (value: string) => void
}

const Tabs = ({
  className,
  value,
  defaultValue,
  onValueChange,
  children,
  ...props
}: TabsProps) => {
  const [inner, setInner] = React.useState(defaultValue ?? "")
  const current = value !== undefined ? value : inner
  const setValue = React.useCallback(
    (v: string) => {
      if (value === undefined) setInner(v)
      onValueChange?.(v)
    },
    [value, onValueChange]
  )
  return (
    <TabsCtx.Provider value={{ value: current, setValue }}>
      <div className={cn(className)} {...props}>
        {children}
      </div>
    </TabsCtx.Provider>
  )
}

const TabsList = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    role="tablist"
    className={cn(
      "inline-flex h-9 items-center justify-center rounded-lg bg-muted p-1 text-muted-foreground",
      className
    )}
    {...props}
  />
))
TabsList.displayName = "TabsList"

const TabsTrigger = React.forwardRef<
  HTMLButtonElement,
  React.ButtonHTMLAttributes<HTMLButtonElement> & {
    active?: boolean
    value?: string
  }
>(({ className, active, value: ownValue, onClick, ...props }, ref) => {
  const ctx = React.useContext(TabsCtx)
  // 两种用法都支持：显式传 active（页面自己算），或只传 value（交给 context 管）。
  const selected = active ?? (ownValue !== undefined && ownValue === ctx.value)
  return (
    <button
      ref={ref}
      type="button"
      role="tab"
      aria-selected={selected}
      className={cn(
        "inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-sm font-medium ring-offset-background transition-all focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50",
        selected ? "bg-background text-foreground shadow" : "hover:text-foreground",
        className
      )}
      onClick={(e) => {
        onClick?.(e)
        if (ownValue !== undefined) ctx.setValue(ownValue)
      }}
      {...props}
    />
  )
})
TabsTrigger.displayName = "TabsTrigger"

const TabsContent = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement> & { value?: string }
>(({ className, value: ownValue, ...props }, ref) => {
  const ctx = React.useContext(TabsCtx)
  // 不匹配时用 hidden 而不是卸载：切回来时不丢编辑器状态、也不重跑请求。
  const hidden = ownValue !== undefined && ctx.value !== ownValue
  return (
    <div
      ref={ref}
      role="tabpanel"
      hidden={hidden}
      className={cn(
        "mt-2 ring-offset-background focus-visible:outline-none",
        className
      )}
      {...props}
    />
  )
})
TabsContent.displayName = "TabsContent"

export { Tabs, TabsList, TabsTrigger, TabsContent }
