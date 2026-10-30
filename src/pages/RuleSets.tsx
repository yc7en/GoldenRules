import { Link } from "react-router-dom"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Async } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api } from "@/lib/api"

export default function RuleSets() {
  const state = useAsync(() => api.rulesets.list(), [])

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">规则集</h1>
        <p className="text-sm text-muted-foreground">
          规则集 = 独立规则模块；表达式 / 决策表是同一规则集的两种写法
        </p>
      </div>

      <Async
        state={state}
        loadingLabel="读取规则集"
        empty={(d) => (d.items ?? []).length === 0}
      >
        {(data) => (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>规则集 ID</TableHead>
                <TableHead>场景</TableHead>
                <TableHead>版本</TableHead>
                <TableHead>命中策略</TableHead>
                <TableHead>兜底</TableHead>
                <TableHead>规则数</TableHead>
                <TableHead>用例（已确认/总数）</TableHead>
                <TableHead>最近更新</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.items.map((rs) => {
                const tests = rs.tests ?? []
                const approved = tests.filter((t) => t.status === "approved").length
                return (
                  <TableRow key={rs.rulesetId}>
                    <TableCell className="font-medium">{rs.rulesetId}</TableCell>
                    <TableCell>
                      <Badge variant="secondary">{rs.scene}</Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{rs.version}</TableCell>
                    <TableCell>{rs.hitStrategy}</TableCell>
                    <TableCell className="text-xs">{rs.defaultDecision}</TableCell>
                    <TableCell>{rs.rules?.length ?? 0}</TableCell>
                    <TableCell>
                      <span
                        className={
                          tests.length > 0 && approved === 0
                            ? "text-amber-700"
                            : ""
                        }
                        title={
                          tests.length > 0 && approved === 0
                            ? "有未确认用例：发布门禁不会真正执行任何用例"
                            : undefined
                        }
                      >
                        {approved} / {tests.length}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {rs.updatedAt
                        ? `${formatTime(rs.updatedAt)} · ${rs.updatedBy ?? "?"}`
                        : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Link to={`/rulesets/${encodeURIComponent(rs.rulesetId)}`}>
                        <Button variant="outline" size="sm">
                          编辑
                        </Button>
                      </Link>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Async>
    </div>
  )
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(
    d.getHours()
  )}:${pad(d.getMinutes())}`
}
