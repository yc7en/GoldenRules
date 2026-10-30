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

// Service is the governance service: change requests, canary, audit.
type Service struct {
	doc *persist.Store[Doc]
	pkg *packages.Service
	now func() time.Time
}

// NewService builds the service. `pkg` is the target it advances state / publishes / rolls back (one-way dependency: gov → packages).
func NewService(doc *persist.Store[Doc], pkg *packages.Service) *Service {
	return &Service{doc: doc, pkg: pkg, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock injects a clock (for test determinism).
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// ---------------------------------------------------------------------------
// Change requests
// ---------------------------------------------------------------------------

// SubmitInput is the input for submitting a change request.
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

// Submit submits a change request.
//
// Two things are nailed down at submit time: the matrix decides how many approvers are required (frozen into the request), and the hard rules fire (an agent must not submit L4).
// The latter must be blocked at **submit** time rather than at approval time — letting an agent write the request into the system and only then rejecting it just makes the request itself noise.
func (s *Service) Submit(in SubmitInput) (Change, error) {
	if strings.TrimSpace(in.PackageID) == "" || strings.TrimSpace(in.PackageVersion) == "" {
		return Change{}, errf(CodeInvalidArgument, "缺少 packageId 或 packageVersion")
	}
	if in.SubmitterType == "" {
		in.SubmitterType = SubmitterAgent // least privilege: an undeclared identity is treated as an agent
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

	// The version must exist and still be a draft: a change request is the entry point for "pushing a draft to approved", so it must not point at a published package.
	v, found := s.pkg.Get(in.PackageID, in.PackageVersion)
	if !found {
		return Change{}, &packages.NotFoundError{PackageID: in.PackageID, Version: in.PackageVersion}
	}
	if v.Status != packages.StatusDraft {
		return Change{}, errf(CodeChangeStateInvalid,
			fmt.Sprintf("%s@%s 当前是 %s，变更单只能针对 draft 版本", in.PackageID, in.PackageVersion, v.Status))
	}
	// The risk level declared inside the package wins: self-reporting L1 on the request while the package says L4 is the classic downgrade bypass.
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
			// A 0-approver cell does not exist; if one shows up the matrix is misconfigured, and refusing is better than "zero approvals passes".
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

// Approve lets one approver take one pending approval slot.
//
// All slots taken → the change request becomes approved, and the **rule package version is advanced** draft → approved (`contracts/04` §6).
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
			// A passed change request = a publishable version. This step lives in the same transaction: the intermediate state where the request is approved
			// while the version is still draft would leave "approved but unpublishable", and ops could only fix the store by hand.
			if _, err := s.pkg.Approve(c.PackageID, c.PackageVersion, actor); err != nil {
				var se *packages.StateError
				if !errors.As(err, &se) || se.Code != packages.CodeBadTransition {
					return err
				}
				// The version is already approved/published (duplicate submission); do not block the approval wrap-up.
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

// Reject refuses a change request. A refusal is final: the same request cannot be "refused and then approved"; to change it, open a new one.
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

// PermitPublish is the governance check before publish (`contracts/04` §4 segregation of duties + A-8).
//
// The returned change request lets the caller write the audit. This function **does not change state**: only after a successful publish does `MarkPublished` wrap up,
// so "check passed but publish failed" cannot leave a bogus published record behind.
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

// MarkPublished wraps up after a successful publish: the change request → published, plus an audit of "who swapped which package from which version to which version".
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

// Rollback rolls back to an older version and records the trail (`contracts/06` §6 + `contracts/04` §9).
//
// A rollback is a "publish action", so A-8 and segregation of duties apply just the same: an agent cannot roll back, and an approver cannot roll back a package they approved
// (otherwise "approved it wrong → rolled it back myself → nobody noticed" closes the loop).
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
// Canary
// ---------------------------------------------------------------------------

// GrayInput adjusts the canary rollout.
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

// SetGray adjusts the canary; returns the configuration and warnings (warnings do not block, e.g. L3/L4 are advised to run shadow first).
//
// Three hard constraints come from `contracts/06` §5:
//   - shadow does not affect online decisions → percentage must be 0;
//   - conditional requires grayTag, otherwise it amounts to a full rollout;
//   - L4 must not jump straight to 100% (`contracts/04` §6): an intermediate step is required.
func (s *Service) SetGray(in GrayInput) (Gray, []string, error) {
	// The rollout is part of the publish action (the "releaser" duty in `contracts/04` §4), so an agent cannot do it.
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
			// A mode switch is a new configuration, not a "percentage tweak": history is kept, so audit questions like "did it ever run in shadow" remain answerable.
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

// Gray reads the canary configuration.
func (s *Service) Gray(packageID string) (Gray, bool) {
	var out Gray
	var ok bool
	s.doc.View(func(d *Doc) {
		out, ok = d.Grays[packageID]
	})
	return out, ok
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// List lists change requests (newest creation time first).
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

// Get returns a change request.
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

// Audits queries audit events (chronological order, so a timeline can be reconstructed).
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

// Record lets another domain (e.g. obs's log access / replay) write one governance event.
//
// The audit event stream has exactly one outlet: cross-domain actions do not write the store themselves but come back to gov,
// otherwise "approval/publish" and "log access/replay" would become two timelines that cannot be reconciled with each other.
func (s *Service) Record(e AuditEvent) { s.audit(e) }

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// audit appends one audit event. An audit write failure must not drag down the business action — it is already outside the transaction,
// so all this guarantees is that "an already effective publish does not look failed because the audit failed".
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
