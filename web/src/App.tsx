import { HashRouter, Routes, Route, Navigate } from "react-router-dom"
import Layout from "@/components/Layout"
import Scenes from "@/pages/Scenes"
import RuleSets from "@/pages/RuleSets"
import RuleSetEditor from "@/pages/RuleSetEditor"
import RuleSetNew from "@/pages/RuleSetNew"
import Facts from "@/pages/Facts"
import TestCases from "@/pages/TestCases"
import Approval from "@/pages/Approval"
import Observability from "@/pages/Observability"

function App() {
  return (
    <HashRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Navigate to="/scenes" replace />} />
          <Route path="/scenes" element={<Scenes />} />
          <Route path="/rulesets" element={<RuleSets />} />
          {/* Declared before `/rulesets/:id` for readability only - the router matches the literal
              segment on its own, so "new" never resolves as a ruleset id. */}
          <Route path="/rulesets/new" element={<RuleSetNew />} />
          <Route path="/rulesets/:id" element={<RuleSetEditor />} />
          <Route path="/facts" element={<Facts />} />
          <Route path="/testcases" element={<TestCases />} />
          <Route path="/approval" element={<Approval />} />
          <Route path="/observability" element={<Observability />} />
          <Route path="*" element={<Navigate to="/scenes" replace />} />
        </Route>
      </Routes>
    </HashRouter>
  )
}

export default App
