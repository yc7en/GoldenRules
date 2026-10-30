import * as React from "react"
import { cn } from "@/lib/utils"

// Controlled Tabs. Previously this only rendered a single layer of divs and `TabsContent` threw away the
// value it received, so the result was "all three panels visible at once, clicking a tab only moved the
// button highlight" - it looked like the sections were missing, when in fact there were no sections. Now
// context passes the current value down and `TabsContent` only shows the matching block.

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
  // Both usages are supported: pass active explicitly (the page computes it), or pass only value (left to context).
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
  // On mismatch use hidden instead of unmounting: switching back does not lose editor state or refire requests.
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
