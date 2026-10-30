import { useState } from "react"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Async } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api } from "@/lib/api"
import type { FactRecord } from "@/lib/api-types"

const sensitivityLabel: Record<string, string> = {
  PUBLIC: "公开",
  INTERNAL: "内部",
  PII: "个人敏感",
  SECRET: "机密",
}

const onMissingLabel: Record<string, string> = {
  ERROR: "缺失即报错",
  DEFAULT: "用默认值",
  NULL: "置空",
  DEGRADE: "降级",
}

export default function Facts() {
  const state = useAsync(() => api.facts.list(), [])
  const [q, setQ] = useState("")

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-lg font-semibold">事实目录</h1>
          <p className="text-sm text-muted-foreground">
            单一真源：字段契约登记
            {state.data && ` · 版本 ${state.data.catalogVersion}`}
            {state.data && ` · ${state.data.items.length} 个字段`}
          </p>
        </div>
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="按字段名/语义过滤"
          className="w-56 rounded-md border border-input bg-transparent px-2 py-1.5 text-sm"
        />
      </div>

      <div className="rounded-md border border-blue-300 bg-blue-50 px-3 py-2 text-xs text-blue-800">
        事实目录只存「契约」（名称/类型/语义/缺失处理/脱敏/版本），不存取数逻辑
        —— 取数归属外置 Fact Provider。本页为只读，登记与改版走
        <code className="mx-1">POST/PUT /api/v1/facts</code>。
      </div>

      <Async state={state} loadingLabel="读取事实目录" empty={(d) => d.items.length === 0}>
        {(data) => {
          const rows = data.items.filter(
            (f) =>
              !q ||
              f.fact.toLowerCase().includes(q.toLowerCase()) ||
              (f.semantics ?? "").includes(q) ||
              (f.displayName ?? "").includes(q)
          )
          if (rows.length === 0) {
            return (
              <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                没有匹配「{q}」的字段。
              </div>
            )
          }
          return (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>字段名</TableHead>
                  <TableHead>名称</TableHead>
                  <TableHead>类型</TableHead>
                  <TableHead>必填</TableHead>
                  <TableHead>可空</TableHead>
                  <TableHead>缺失处理</TableHead>
                  <TableHead>脱敏</TableHead>
                  <TableHead>版本</TableHead>
                  <TableHead>语义</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((f) => (
                  <FactRow key={f.fact} fact={f} />
                ))}
              </TableBody>
            </Table>
          )
        }}
      </Async>
    </div>
  )
}

function FactRow({ fact: f }: { fact: FactRecord }) {
  return (
    <TableRow>
      <TableCell className="font-mono text-sm">
        {f.fact}
        {f.deprecated && (
          <Badge className="ml-2 bg-muted text-muted-foreground">已废弃</Badge>
        )}
      </TableCell>
      <TableCell className="text-sm">{f.displayName ?? "—"}</TableCell>
      <TableCell>
        <Badge variant="outline">{f.type}</Badge>
        {f.enum && (
          <span className="ml-1 text-xs text-muted-foreground">
            {f.enum.join("/")}
          </span>
        )}
      </TableCell>
      <TableCell>{f.required ? "是" : "否"}</TableCell>
      <TableCell>{f.nullable ? "是" : "否"}</TableCell>
      <TableCell className="text-xs">
        {onMissingLabel[f.onMissing ?? ""] ?? f.onMissing ?? "—"}
        {f.hasDefault && f.default !== undefined && (
          <span className="ml-1 font-mono text-muted-foreground">
            ({JSON.stringify(f.default)})
          </span>
        )}
      </TableCell>
      <TableCell>
        <Badge variant="secondary">
          {sensitivityLabel[f.sensitivity ?? "PUBLIC"] ?? f.sensitivity}
        </Badge>
      </TableCell>
      <TableCell className="text-xs">{f.version}</TableCell>
      <TableCell className="max-w-[28rem] text-sm text-muted-foreground">
        {f.semantics ?? "—"}
      </TableCell>
    </TableRow>
  )
}
