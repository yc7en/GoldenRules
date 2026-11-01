import { Handle, Position, type Node, type NodeProps } from "@xyflow/react"
import { Badge } from "@/components/ui/badge"
import { condToText } from "@/lib/tree/model"
import type { TreeData } from "@/lib/tree/graph"
import { cn } from "@/lib/utils"

/**
 * Canvas nodes.
 *
 * They are read-only cards; editing happens in the inspector panel next to the canvas. Three dropdowns
 * do not fit inside a 248px node without truncating either the fact name or the value, and a node that
 * is being edited while the user pans is a reliable way to lose keystrokes.
 */

function Ports({ rtl }: { rtl: boolean }) {
  return (
    <>
      <Handle
        type="target"
        position={rtl ? Position.Right : Position.Left}
        className="!size-1.5 !border-0 !bg-muted-foreground/50"
      />
      <Handle
        type="source"
        position={rtl ? Position.Left : Position.Right}
        className="!size-1.5 !border-0 !bg-muted-foreground/50"
      />
    </>
  )
}

const shell =
  "w-full rounded-lg border bg-card px-3 py-2 shadow-sm transition-[opacity,box-shadow]"

function stateClass(data: TreeData, selected?: boolean) {
  return cn(
    shell,
    selected && "ring-2 ring-primary ring-offset-1",
    data.hit && "border-emerald-500 bg-emerald-50",
    data.dim && "opacity-35"
  )
}

export function CondNode({ data, selected }: NodeProps<Node<TreeData>>) {
  const cond = data.cond
  if (!cond) return null
  return (
    <div className={stateClass(data, selected)} style={{ width: 248 }}>
      <Ports rtl={!!data.rtl} />
      <div className="truncate text-[11px] text-muted-foreground">
        {data.factLabel ?? cond.fact}
      </div>
      <div className="mt-0.5 truncate font-mono text-xs" dir="ltr">
        {condToText(cond)}
      </div>
      <div className="mt-1 flex items-center justify-between text-[10px] text-muted-foreground">
        <span dir="ltr">{cond.fact}</span>
        {data.hit && <span className="text-emerald-700">●</span>}
      </div>
    </div>
  )
}

export function LeafNode({ data, selected }: NodeProps<Node<TreeData>>) {
  const then = data.then
  return (
    <div className={stateClass(data, selected)} style={{ width: 232 }}>
      <Ports rtl={!!data.rtl} />
      <div className="flex items-center justify-between gap-2">
        <Badge
          className={cn(
            "text-[10px]",
            then?.decision === "APPROVE"
              ? "bg-emerald-100 text-emerald-700"
              : then?.decision === "REJECT"
                ? "bg-red-100 text-red-700"
                : "bg-amber-100 text-amber-700"
          )}
        >
          {then?.decision ?? "—"}
        </Badge>
        <span className="truncate font-mono text-[10px] text-muted-foreground" dir="ltr">
          {data.ruleId ?? ""}
        </span>
      </div>
      <div className="mt-1 truncate font-mono text-xs" dir="ltr">
        {then?.reasonCode ?? "—"}
      </div>
      {then?.assign && then.assign.length > 0 && (
        <div className="mt-1 truncate text-[10px] text-muted-foreground" dir="ltr">
          {then.assign.map((a) => `${a.var}`).join(", ")}
        </div>
      )}
    </div>
  )
}

export function PlaceholderNode({ data }: NodeProps<Node<TreeData>>) {
  const onAdd = data.onAdd as
    | ((parentId: string | null, branch: "yes" | "no", kind: "cond" | "leaf") => void)
    | undefined
  return (
    <div
      className={cn(
        "flex w-[200px] flex-col items-center gap-1 rounded-lg border border-dashed border-muted-foreground/40 bg-transparent px-2 py-1.5",
        data.dim && "opacity-35"
      )}
    >
      <Ports rtl={!!data.rtl} />
      {onAdd && data.editable ? (
        <div className="flex items-center gap-1">
          <button
            type="button"
            className="nodrag rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-muted hover:text-foreground"
            onClick={() => onAdd(data.parentId ?? null, (data.branch as "yes" | "no") ?? "yes", "cond")}
          >
            + {data.addCondLabel as string}
          </button>
          <span className="text-muted-foreground/40">|</span>
          <button
            type="button"
            className="nodrag rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-muted hover:text-foreground"
            onClick={() => onAdd(data.parentId ?? null, (data.branch as "yes" | "no") ?? "yes", "leaf")}
          >
            + {data.addLeafLabel as string}
          </button>
        </div>
      ) : (
        <span className="text-[11px] text-muted-foreground">{data.title as string}</span>
      )}
    </div>
  )
}

export function EntryNode({ data }: NodeProps<Node<TreeData>>) {
  return (
    <div className="rounded-full border bg-card px-4 py-2 text-xs font-medium shadow-sm">
      <Ports rtl={!!data.rtl} />
      {data.text as string}
    </div>
  )
}

export function AggregateNode({ data }: NodeProps<Node<TreeData>>) {
  return (
    <div className="rounded-full border bg-muted px-4 py-2 text-xs font-medium">
      <Ports rtl={!!data.rtl} />
      {data.text as string}
    </div>
  )
}

export function RuleNode({ data, selected }: NodeProps<Node<TreeData>>) {
  return (
    <div className={stateClass(data, selected)} style={{ width: 290 }}>
      <Ports rtl={!!data.rtl} />
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-xs font-medium" dir="ltr">
          {data.ruleId as string}
        </span>
        {data.hit && <span className="text-[10px] text-emerald-700">●</span>}
      </div>
      {/* `break-words`, not `break-all`: the expression is ASCII identifiers and operators, and
          breaking mid-identifier ("request.amo / unt") reads as a rendering fault. */}
      <div
        className="mt-0.5 line-clamp-3 font-mono text-[10px] break-words text-muted-foreground"
        dir="ltr"
      >
        {data.text as string}
      </div>
    </div>
  )
}

export const nodeTypes = {
  cond: CondNode,
  leaf: LeafNode,
  placeholder: PlaceholderNode,
  entry: EntryNode,
  rule: RuleNode,
  aggregate: AggregateNode,
}
