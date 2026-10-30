package gov

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"goldenrules/platform/packages"
	"goldenrules/platform/persist"
)

// Service 治理服务：变更单、灰度、审计。
type Service struct {
	doc *persist.Store[Doc]
	pkg *packages.Service
	now func() time.Time
}

// NewService 构造服务。`pkg` 是它推进状态/发布/回滚的目标（单向依赖：gov → packages）。
func NewService(doc *persist.Store[Doc], pkg *packages.Service) *Service {
	return &Service{doc: doc, pkg: pkg, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入时钟（测试确定性）。
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// ---------------------------------------------------------------------------
// 变更单
// ---------------------------------------------------------------------------

// SubmitInput 提交变更单的入参。
type SubmitInput struct {
	PackageID      string
	PackageVersion string
	ChangeType     string
	RiskLevel      string
	Reason         string
	Scope          string
	Submitter      string
	SubmitterType  string
	Materials      []string
}

// Submit 提交变更单。
//
// 两件事在提交时定死：矩阵算出要几个人批（固化进单据），以及硬规则拦截（agent 不得提 L4）。
// 后者必须在**提交**时拦，而不是审批时——让 agent 先把单子写进系统再被拒，单据本身就成了噪音。
func (s *Service) Submit(in SubmitInput) (Change, error) {
	if strings.TrimSpace(in.PackageID) == "" || strings.TrimSpace(in.PackageVersion) == "" {
		return Change{}, errf(CodeInvalidArgument, "缺少 packageId 或 packageVersion")
	}
	if in.SubmitterType == "" {
		in.SubmitterType = SubmitterAgent // 最小权限：未声明身份按 agent
	}
	if !validSubmitter(in.SubmitterType) {
		return Change{}, errf(CodeInvalidArgument, "未知的提交者类型: "+in.SubmitterType)
	}
	if !validLevel(in.RiskLevel) {
		return Change{}, errf(CodeInvalidArgument, "未知的风险等级: "+in.RiskLevel)
	}
	if in.ChangeType == "" {
		in.ChangeType = ChangeTypeLogic
	}
	switch in.ChangeType {
	case ChangeTypeAdd, ChangeTypeThreshold, ChangeTypeLogic, ChangeTypeDelete:
	default:
		return Change{}, errf(CodeInvalidArgument, "未知的变更类型: "+in.ChangeType)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return Change{}, errf(CodeInvalidArgument, "变更原因必填（contracts/04 §5 材料清单）")
	}

	cell, ok := Lookup(in.SubmitterType, in.RiskLevel)
	if !ok {
		return Change{}, errf(CodeInvalidArgument, "矩阵里没有 "+in.SubmitterType+" × "+in.RiskLevel)
	}
	if cell.Forbidden {
		return Change{}, errf(CodeMatrixRejected, cell.Reason)
	}

	// 版本必须存在且仍是草稿：变更单是「把草稿推到 approved」的入口，不能指向已发布的包。
	v, found := s.pkg.Get(in.PackageID, in.PackageVersion)
	if !found {
		return Change{}, &packages.NotFoundError{PackageID: in.PackageID, Version: in.PackageVersion}
	}
	if v.Status != packages.StatusDraft {
		return Change{}, errf(CodeChangeStateInvalid,
			fmt.Sprintf("%s@%s 当前是 %s，变更单只能针对 draft 版本", in.PackageID, in.PackageVersion, v.Status))
	}
	// 风险等级以包内声明为准：单据上自报 L1 而包里写 L4 是最典型的降级绕过。
	if v.RiskLevel != "" && v.RiskLevel != in.RiskLevel {
		return Change{}, errf(CodeInvalidArgument,
			fmt.Sprintf("变更单风险等级 %s 与规则包声明 %s 不一致；请改包或改单据", in.RiskLevel, v.RiskLevel))
	}

	var out Change
	err := s.doc.Update(func(d *Doc) error {
		if d.Seq == nil {
			d.Seq = map[string]int{}
		}
		now := s.now()
		day := now.Format("20060102")
		d.Seq[day]++
		at := nowString(now)

		out = Change{
			ChangeID:       fmt.Sprintf("CHG-%s-%04d", day, d.Seq[day]),
			PackageID:      in.PackageID,
			PackageVersion: in.PackageVersion,
			ChangeType:     in.ChangeType,
			RiskLevel:      in.RiskLevel,
			Reason:         in.Reason,
			Scope:          in.Scope,
			Submitter:      in.Submitter,
			SubmitterType:  in.SubmitterType,
			Status:         ChangeSubmitted,
			Slots:          cell.Slots,
			SelfApproval:   cell.SelfApproval,
			Approvals:      []Approval{},
			Materials:      in.Materials,
			CreatedAt:      at,
			UpdatedAt:      at,
		}
		if len(out.Slots) == 0 {
			// 0 人的格子不存在；真出现说明矩阵配错了，宁可拒绝也不要「零审批直接过」。
			return errf(CodeMatrixRejected, "矩阵未给该等级配置审批位")
		}
		d.Changes = append(d.Changes, out)
		return nil
	})
	if err != nil {
		return Change{}, err
	}
	s.audit(AuditEvent{
		Action: ActionChangeSubmitted, Actor: in.Submitter, ActorType: in.SubmitterType,
		ChangeID: out.ChangeID, PackageID: out.PackageID, PackageVersion: out.PackageVersion,
		RiskLevel: out.RiskLevel, To: ChangeSubmitted, Reason: out.Reason,
		Detail: fmt.Sprintf("需 %d 人审批：%s", len(out.Slots), slotLabels(out.Slots)),
	})
	return out, nil
}

// Approve 一位审批人占用一个待填审批位。
//
// 全部占满 → 变更单 approved，并**推进规则包版本** draft → approved（`contracts/04` §6）。
func (s *Service) Approve(changeID, actor, actorType, role, comment string) (Change, error) {
	if !validRole(role) {
		return Change{}, errf(CodeInvalidArgument, "未知的审批角色: "+role)
	}
	if actorType == SubmitterAgent {
		return Change{}, errf(CodeAgentCannotApprove, "agent 不参与审批（A-8：AI 只提草稿）")
	}

	var out Change
	err := s.doc.Update(func(d *Doc) error {
		c, err := findChange(d, changeID)
		if err != nil {
			return err
		}
		if c.Status != ChangeSubmitted {
			return errf(CodeChangeStateInvalid,
				fmt.Sprintf("变更单当前是 %s，只有 submitted 可以审批", c.Status))
		}
		for _, a := range c.Approvals {
			if a.Actor == actor {
				return errf(CodeChangeStateInvalid, actor+" 已经批过这张单子")
			}
		}

		slot := -1
		for i, sl := range c.Slots {
			if slotUsed(c.Approvals, i) {
				continue
			}
			if sl.Accepts(role) {
				slot = i
				break
			}
		}
		if slot < 0 {
			return errf(CodeRoleNotRequired,
				fmt.Sprintf("角色 %s 不在待批审批位内（待批：%s）", role, slotLabels(c.Remaining())))
		}

		self := actor == c.Submitter
		if self && !c.SelfApproval {
			return errf(CodeSelfApproval,
				fmt.Sprintf("%s 是提交者本人，%s 等级不允许自审（contracts/04 §3/§4）", actor, c.RiskLevel))
		}

		now := nowString(s.now())
		c.Approvals = append(c.Approvals, Approval{
			Actor: actor, Role: role, Slot: slot, Self: self,
			At: now, Comment: comment, RiskLevel: c.RiskLevel,
		})
		c.UpdatedAt = now
		if len(c.Remaining()) == 0 {
			c.Status = ChangeApproved
			// 变更单通过 = 版本可发布。这一步放在同一个事务里：单据 approved 而版本还 draft 的中间态
			// 会让「批了却发不出去」，运维只能手工改库。
			if _, err := s.pkg.Approve(c.PackageID, c.PackageVersion, actor); err != nil {
				var se *packages.StateError
				if !errors.As(err, &se) || se.Code != packages.CodeBadTransition {
					return err
				}
				// 版本已经是 approved/published（重复提交场景）不阻断审批收尾。
			}
		}
		out = *c
		return nil
	})
	if err != nil {
		return Change{}, err
	}

	ev := AuditEvent{
		Action: ActionChangeApproved, Actor: actor, ActorType: actorType,
		ChangeID: out.ChangeID, PackageID: out.PackageID, PackageVersion: out.PackageVersion,
		RiskLevel: out.RiskLevel,
	}
	if len(out.Approvals) > 0 {
		last := out.Approvals[len(out.Approvals)-1]
		ev.From, ev.To = "submitted", "approval"
		ev.Detail = fmt.Sprintf("角色 %s 占用审批位 %d（自审=%v）", last.Role, last.Slot, last.Self)
		if comment != "" {
			ev.Detail += "；意见：" + comment
		}
	}
	if out.Status == ChangeApproved {
		ev.To = ChangeApproved
		ev.Detail += "；已满票，版本推进到 approved"
	}
	s.audit(ev)
	return out, nil
}

// Reject 否决变更单。否决是终态：同一张单子不能「否了再批」，要改就重开一张。
func (s *Service) Reject(changeID, actor, actorType, role, comment string) (Change, error) {
	if strings.TrimSpace(comment) == "" {
		return Change{}, errf(CodeInvalidArgument, "否决必须写理由（contracts/04 §9 留存要求）")
	}
	var out Change
	err := s.doc.Update(func(d *Doc) error {
		c, err := findChange(d, changeID)
		if err != nil {
			return err
		}
		if c.Status != ChangeSubmitted {
			return errf(CodeChangeStateInvalid,
				fmt.Sprintf("变更单当前是 %s，只有 submitted 可否决", c.Status))
		}
		now := nowString(s.now())
		c.Status = ChangeRejected
		c.Rejection = &Rejection{Actor: actor, Role: role, At: now, Comment: comment}
		c.UpdatedAt = now
		out = *c
		return nil
	})
	if err != nil {
		return Change{}, err
	}
	s.audit(AuditEvent{
		Action: ActionChangeRejected, Actor: actor, ActorType: actorType,
		ChangeID: out.ChangeID, PackageID: out.PackageID, PackageVersion: out.PackageVersion,
		RiskLevel: out.RiskLevel, From: ChangeSubmitted, To: ChangeRejected, Reason: comment,
	})
	return out, nil
}

// PermitPublish 发布前的治理检查（`contracts/04` §4 职责分离 + A-8）。
//
// 返回的变更单供调用方记审计。这里**不改状态**：发布成功后才由 `MarkPublished` 收尾，
// 避免「检查通过但发布失败」留下 published 的错误记录。
func (s *Service) PermitPublish(changeID, packageID, version, actor, actorType string) (Change, error) {
	if actorType == SubmitterAgent {
		return Change{}, errf(CodeAgentCannotPublish, "agent 永远不能发布（A-8），只能提草稿")
	}
	if strings.TrimSpace(changeID) == "" {
		return Change{}, errf(CodeChangeRequired, "发布必须挂一张已批准的变更单（contracts/04 §6）")
	}
	var c Change
	var found bool
	s.doc.View(func(d *Doc) {
		for _, it := range d.Changes {
			if it.ChangeID == changeID {
				c, found = it, true
				return
			}
		}
	})
	if !found {
		return Change{}, &NotFoundError{ChangeID: changeID}
	}
	if c.PackageID != packageID || c.PackageVersion != version {
		return Change{}, errf(CodePackageMismatch,
			fmt.Sprintf("变更单对应 %s@%s，与发布目标 %s@%s 不一致", c.PackageID, c.PackageVersion, packageID, version))
	}
	if c.Status != ChangeApproved && c.Status != ChangePublished {
		return Change{}, errf(CodeChangeNotApproved,
			fmt.Sprintf("变更单状态是 %s，未批准不能发布", c.Status))
	}
	if actor == c.Submitter {
		return Change{}, errf(CodeSoDSubmitPublish, "提交者不得发布自己提交的变更（contracts/04 §4 编辑与发布分离）")
	}
	for _, a := range c.Approvals {
		if a.Actor == actor {
			return Change{}, errf(CodeSoDApprovePublish, "审批人不得发布同一变更（contracts/04 §4 审批与发布分离）")
		}
	}
	return c, nil
}

// MarkPublished 发布成功后收尾：变更单 → published，并审计「谁把哪个包从哪个版本换成哪个版本」。
func (s *Service) MarkPublished(changeID string, v packages.Version, actor, actorType, from, validFrom, validUntil string) {
	if changeID == "" {
		return
	}
	_ = s.doc.Update(func(d *Doc) error {
		c, err := findChange(d, changeID)
		if err != nil {
			return err
		}
		now := nowString(s.now())
		c.Status = ChangePublished
		c.PublishedAt = now
		c.PublishedBy = actor
		c.UpdatedAt = now
		return nil
	})
	detail := "生效时间：发布即生效"
	if validFrom != "" {
		detail = "生效时间：" + validFrom
	}
	if validUntil != "" {
		detail += "；失效时间：" + validUntil
	}
	s.audit(AuditEvent{
		Action: ActionPackagePublished, Actor: actor, ActorType: actorType,
		ChangeID: changeID, PackageID: v.PackageID, PackageVersion: v.PackageVersion,
		From: from, To: v.PackageVersion, Detail: detail,
	})
}

// Rollback 回滚到旧版本并留痕（`contracts/06` §6 + `contracts/04` §9）。
//
// 回滚是「发布动作」，所以同样受 A-8 与职责分离约束：agent 不能回滚，审批人不能回滚自己批的包
// （否则「批错 → 自己回滚 → 无人发现」可以闭环）。
func (s *Service) Rollback(packageID, toVersion, actor, actorType, reason string) (packages.Version, error) {
	if actorType == SubmitterAgent {
		return packages.Version{}, errf(CodeAgentCannotPublish, "agent 不能执行回滚（A-8）")
	}
	if strings.TrimSpace(reason) == "" {
		return packages.Version{}, errf(CodeInvalidArgument, "回滚必须写原因（contracts/06 §6 可审计）")
	}
	prev, _ := s.pkg.Active(packageID)
	if prev.PackageVersion != "" && prev.PackageVersion == toVersion {
		return packages.Version{}, errf(CodeInvalidArgument, toVersion+" 已经是当前生效版本")
	}
	v, err := s.pkg.Rollback(packageID, toVersion, actor, reason)
	if err != nil {
		return packages.Version{}, err
	}
	s.audit(AuditEvent{
		Action: ActionPackageRolledBack, Actor: actor, ActorType: actorType,
		PackageID: packageID, PackageVersion: toVersion,
		From: prev.PackageVersion, To: toVersion, Reason: reason,
	})
	return v, nil
}

// ---------------------------------------------------------------------------
// 灰度
// ---------------------------------------------------------------------------

// GrayInput 调整灰度放量。
type GrayInput struct {
	PackageID  string
	Version    string
	Mode       string
	Percentage int
	GrayTags   []string
	Reason     string
	Actor      string
	ActorType  string
}

// SetGray 调整灰度；返回配置与提示（提示不阻断，例如 L3/L4 建议先影子）。
//
// 三条硬约束来自 `contracts/06` §5：
//   - shadow 不改变线上决策 → percentage 必须为 0；
//   - conditional 必须有 grayTag，否则等于全量；
//   - L4 禁止一步到 100%（`contracts/04` §6）：必须先有中间档。
func (s *Service) SetGray(in GrayInput) (Gray, []string, error) {
	// 放量是发布动作的一部分（`contracts/04` §4「发布人」职责），agent 不能做。
	if in.ActorType == SubmitterAgent {
		return Gray{}, nil, errf(CodeAgentCannotPublish, "agent 不能调整灰度放量（A-8）")
	}
	switch in.Mode {
	case GrayShadow, GrayPercentage, GrayConditional:
	default:
		return Gray{}, nil, errf(CodeGrayInvalid, "灰度形态只能是 shadow / percentage / conditional")
	}
	if in.Percentage < 0 || in.Percentage > 100 {
		return Gray{}, nil, errf(CodeGrayInvalid, "放量比例必须在 0~100 之间")
	}
	if in.Mode == GrayShadow && in.Percentage != 0 {
		return Gray{}, nil, errf(CodeGrayInvalid, "shadow 模式不改变线上决策，放量比例必须为 0")
	}
	if in.Mode == GrayPercentage && in.Percentage == 0 {
		return Gray{}, nil, errf(CodeGrayInvalid, "percentage 模式的放量比例不能为 0（要停就用 shadow 或回滚）")
	}
	if in.Mode == GrayConditional && len(in.GrayTags) == 0 {
		return Gray{}, nil, errf(CodeGrayInvalid, "conditional 模式必须给 grayTags，否则等价于全量")
	}

	v, ok := s.pkg.Get(in.PackageID, in.Version)
	if !ok {
		return Gray{}, nil, &packages.NotFoundError{PackageID: in.PackageID, Version: in.Version}
	}
	if v.Status != packages.StatusPublished {
		return Gray{}, nil, errf(CodeGrayInvalid,
			fmt.Sprintf("%s@%s 当前是 %s，只有已发布版本才能配灰度", in.PackageID, in.Version, v.Status))
	}

	var (
		out      Gray
		warns    []string
		prior    int
		prevText string
	)
	err := s.doc.Update(func(d *Doc) error {
		if d.Grays == nil {
			d.Grays = map[string]Gray{}
		}
		cur := d.Grays[in.PackageID]
		prior = cur.PeakPercentage
		if cur.Mode == "" {
			prevText = "未配置"
		} else {
			prevText = fmt.Sprintf("%s %d%%", cur.Mode, cur.Percentage)
		}

		level := v.RiskLevel
		if level == "" {
			level = LevelL1
		}
		if (level == LevelL3 || level == LevelL4) && in.Mode == GrayPercentage && in.Percentage == 100 && prior < 50 {
			return errf(CodeGrayStepTooLarge,
				fmt.Sprintf("%s 等级不允许一步到 100%%（已放量峰值 %d%%），必须先有中间档（contracts/04 §6）", level, prior))
		}
		if (level == LevelL3 || level == LevelL4) && in.Mode == GrayPercentage && prior == 0 && in.Percentage > 10 {
			return errf(CodeGrayStepTooLarge,
				fmt.Sprintf("%s 等级首档放量不得超过 10%%（本次 %d%%）", level, in.Percentage))
		}

		now := nowString(s.now())
		if cur.Mode != in.Mode && cur.Mode != "" {
			// 形态切换是一次新配置，不是「调比例」：历史仍保留，便于「影子里跑过没有」这类审计问题。
			warns = append(warns, fmt.Sprintf("灰度形态发生切换：%s → %s", cur.Mode, in.Mode))
		}
		out = Gray{
			PackageID:      in.PackageID,
			Version:        in.Version,
			Mode:           in.Mode,
			Percentage:     in.Percentage,
			GrayTags:       in.GrayTags,
			PeakPercentage: prior,
			UpdatedAt:      now,
			UpdatedBy:      in.Actor,
			History:        cur.History,
		}
		if in.Percentage > out.PeakPercentage {
			out.PeakPercentage = in.Percentage
		}
		out.History = append(out.History, GrayStep{
			At: now, By: in.Actor, Mode: in.Mode, Percentage: in.Percentage, Reason: in.Reason,
		})
		d.Grays[in.PackageID] = out
		return nil
	})
	if err != nil {
		return Gray{}, nil, err
	}
	if v.RiskLevel == LevelL3 || v.RiskLevel == LevelL4 {
		if in.Mode != GrayShadow && prior == 0 {
			warns = append(warns, fmt.Sprintf("%s 变更建议先影子运行（contracts/04 §6），本次直接放量 %d%%", v.RiskLevel, in.Percentage))
		}
	}
	s.audit(AuditEvent{
		Action: ActionGrayUpdated, Actor: in.Actor, ActorType: in.ActorType,
		PackageID: in.PackageID, PackageVersion: in.Version,
		From: prevText, To: fmt.Sprintf("%s %d%%", in.Mode, in.Percentage),
		RiskLevel: v.RiskLevel, Reason: in.Reason,
	})
	return out, warns, nil
}

// Gray 读灰度配置。
func (s *Service) Gray(packageID string) (Gray, bool) {
	var out Gray
	var ok bool
	s.doc.View(func(d *Doc) {
		out, ok = d.Grays[packageID]
	})
	return out, ok
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// List 列变更单（按创建时间倒序）。
func (s *Service) List(status, packageID string) []Change {
	var out []Change
	s.doc.View(func(d *Doc) {
		for _, c := range d.Changes {
			if status != "" && c.Status != status {
				continue
			}
			if packageID != "" && c.PackageID != packageID {
				continue
			}
			out = append(out, c)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Get 取变更单。
func (s *Service) Get(changeID string) (Change, bool) {
	var out Change
	var ok bool
	s.doc.View(func(d *Doc) {
		for _, c := range d.Changes {
			if c.ChangeID == changeID {
				out, ok = c, true
				return
			}
		}
	})
	return out, ok
}

// Audits 审计事件查询（按时间正序，便于还原一条时间线）。
func (s *Service) Audits(changeID, packageID string) []AuditEvent {
	var out []AuditEvent
	s.doc.View(func(d *Doc) {
		for _, e := range d.Audits {
			if changeID != "" && e.ChangeID != changeID {
				continue
			}
			if packageID != "" && e.PackageID != packageID {
				continue
			}
			out = append(out, e)
		}
	})
	return out
}

// Record 由其他域（如 obs 的日志访问/回放）写入一条治理事件。
//
// 审计事件流只有一个出口：跨域动作不自己写库，而是回到 gov，
// 否则「审批/发布」与「日志访问/回放」会变成两套互相对不上的时间线。
func (s *Service) Record(e AuditEvent) { s.audit(e) }

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// audit 追加一条审计事件。审计写失败不能拖垮业务动作——它已经在事务外，
// 这里只保证「不因审计失败而让已经生效的发布看起来失败」。
func (s *Service) audit(e AuditEvent) {
	_ = s.doc.Update(func(d *Doc) error {
		now := s.now()
		e.At = nowString(now)
		e.EventID = fmt.Sprintf("AUD-%s-%s", now.Format("20060102"), strconv.FormatInt(now.UnixNano(), 36))
		d.Audits = append(d.Audits, e)
		return nil
	})
}

func findChange(d *Doc, changeID string) (*Change, error) {
	for i := range d.Changes {
		if d.Changes[i].ChangeID == changeID {
			return &d.Changes[i], nil
		}
	}
	return nil, &NotFoundError{ChangeID: changeID}
}

func slotUsed(approvals []Approval, slot int) bool {
	for _, a := range approvals {
		if a.Slot == slot {
			return true
		}
	}
	return false
}

func slotLabels(slots []Slot) string {
	if len(slots) == 0 {
		return "无"
	}
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.Label+"("+strings.Join(s.Roles, "|")+")")
	}
	return strings.Join(out, " / ")
}
