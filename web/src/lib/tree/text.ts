import type { ExprNode } from "@/lib/api-types"

/**
 * Expression -> readable text. **Display only**: this never evaluates anything, because evaluation
 * must stay in the engine - a second evaluator in the UI is how "the picture says one thing, the
 * engine does another" starts.
 */
export function exprToText(e: ExprNode): string {
  if (e === null || typeof e !== "object") return JSON.stringify(e)
  const obj = e as {
    op?: string
    args?: ExprNode[]
    fact?: string
    type?: string
    value?: unknown
  }
  if (obj.fact !== undefined) return obj.fact
  if (obj.type !== undefined) return JSON.stringify(obj.value)
  if (obj.op) {
    const args = (obj.args ?? []).map(exprToText)
    switch (obj.op) {
      case "and":
        return args.join(" && ")
      case "or":
        return args.join(" || ")
      case "not":
        return `!(${args[0]})`
      case "eq":
        return `${args[0]} == ${args[1]}`
      case "ne":
        return `${args[0]} != ${args[1]}`
      case "gt":
        return `${args[0]} > ${args[1]}`
      case "gte":
        return `${args[0]} >= ${args[1]}`
      case "lt":
        return `${args[0]} < ${args[1]}`
      case "lte":
        return `${args[0]} <= ${args[1]}`
      case "in":
        return `${args[0]} in ${args[1]}`
      case "notIn":
        return `${args[0]} not in ${args[1]}`
      default:
        return `${obj.op}(${args.join(", ")})`
    }
  }
  return JSON.stringify(e)
}
