package gov

import (
	"fmt"
	"strings"
	"time"

	"goldenrules/platform/packages"
)

// 紧急通道（`contracts/04` §7）。
//
// 线上事故需要立即修复时，允许「发布人直接发布」，但代价必须显式记账：
//
//	① 24h 内必须补审（事后审批），超期补审会被标记 Late；
//	② 全程留痕并标记 EMERGENCY；
//	③ 仅 L3 及以下可用；L4 紧急变更必须双人授权（发布人 + 1 名负责人）。
//
// 这里刻意**不**给紧急通道开任何后门：不跳过 A-8（agent 永不发布）、不跳过审计、
// 不跳过「变更单必须存在」。紧急只压缩「等审批的时间」，不压缩「谁来负责」。
const (
	CodeEmergencyLevelInvalid   = "GOV_EMERGENCY_LEVEL_INVALID"
	CodeEmergencySecondRequired = "GOV_EMERGENCY_SECOND_REQUIRED"
	CodeEmergencySecondInvalid  = "GOV_EMERGENCY_SECOND_INVALID"
	CodeEmergencyStateInvalid   = "GOV_EMERGENCY_STATE_INVALID"
	CodeMakeupReviewRequired    = "GOV_MAKEUP_REVIEW_REQUIRED"
	CodeMakeupReviewDuplicate   = "GOV_MAKEUP_REVIEW_DUPLICATE"
	CodeMakeupReviewSelf        = "GOV_MAKEUP_REVIEW_SELF"
)

// 补审窗口（`contracts/04` §7）。
const makeupReviewWindow = 24 * time.Hour

// EmergencyInput 紧急发布入参。
type EmergencyInput struct {
	ChangeID   string
	Actor      string
	ActorType  string
	Reason     string
	ValidFrom  string
	ValidUntil string
	// SecondActor L4 必需：共同授权的负责人（必须是另一个人，且不是 agent）
	SecondActor     string
	SecondActorType string
}

// EmergencyPublish 走紧急通道发布：跳过等待审批，但把「谁按的、谁共同授权」记全。
//
// 版本状态如果是 draft，会先推到 approved 再发布——不是绕过状态机，而是**在一条被审计的路径上
// 把这两步合成一次操作**，保证状态机对其他读者仍然是连续的。
func (s *Service) EmergencyPublish(in EmergencyInput) (Change, error) {
	if in.ActorType == SubmitterAgent {
		return Change{}, errf(CodeAgentCannotPublish, "agent 永远不能发布（A-8），紧急通道也不例外")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return Change{}, errf(CodeInvalidArgument, "紧急发布必须写原因（contracts/04 §7 全程留痕）")
	}

	var snapshot Change
	var found bool
	s.doc.View(func(d *Doc) {
		for _, it := range d.Changes {
			if it.ChangeID == in.ChangeID {
				snapshot, found = it, true
				return
			}
		}
	})
	if !found {
		return Change{}, &NotFoundError{ChangeID: in.ChangeID}
	}
	if snapshot.Status != ChangeSubmitted && snapshot.Status != ChangeApproved {
		return Change{}, errf(CodeEmergencyStateInvalid,
			fmt.Sprintf("变更单状态是 %s，只有 submitted/approved 可走紧急通道", snapshot.Status))
	}

	// 等级门槛：L4 必须双人授权（§7）。这是紧急通道唯一的「额外门槛」，
	// 也是它唯一不能压缩的东西——越紧急的高风险变更越需要第二双眼睛。
	if snapshot.RiskLevel == LevelL4 {
		if strings.TrimSpace(in.SecondActor) == "" {
			return Change{}, errf(CodeEmergencySecondRequired,
				"L4 紧急变更需要双人授权（发布人 + 1 名负责人，contracts/04 §7）")
		}
		if in.SecondActor == in.Actor {
			return Change{}, errf(CodeEmergencySecondInvalid, "共同授权人不能是发布人本人")
		}
		if in.SecondActorType == SubmitterAgent {
			return Change{}, errf(CodeEmergencySecondInvalid, "agent 不能作为共同授权人（A-8）")
		}
	}

	// 版本推进：draft → approved → published 一次做完，但每一步都真实发生（可被其它读者观察到连续状态）
	v, ok := s.pkg.Get(snapshot.PackageID, snapshot.PackageVersion)
	if !ok {
		return Change{}, &packages.NotFoundError{PackageID: snapshot.PackageID, Version: snapshot.PackageVersion}
	}
	if v.Status == packages.StatusDraft {
		if _, err := s.pkg.Approve(snapshot.PackageID, snapshot.PackageVersion, in.Actor); err != nil {
			return Change{}, err
		}
	}

	published, err := s.pkg.Publish(snapshot.PackageID, snapshot.PackageVersion, in.Actor, in.ValidFrom, in.ValidUntil)
	if err != nil {
		return Change{}, err
	}

	var out Change
	now := s.now()
	due := now.Add(makeupReviewWindow)
	err = s.doc.Update(func(d *Doc) error {
		c, err := findChange(d, in.ChangeID)
		if err != nil {
			return err
		}
		at := nowString(now)
		c.Status = ChangePublished
		c.PublishedAt = at
		c.PublishedBy = in.Actor
		c.UpdatedAt = at
		c.Emergency = true
		c.EmergencyBy = in.Actor
		c.EmergencySecondBy = in.SecondActor
		c.EmergencyAt = at
		c.EmergencyReason = in.Reason
		c.MakeupReviewDueAt = nowString(due)
		out = *c
		return nil
	})
	if err != nil {
		return Change{}, err
	}

	detail := fmt.Sprintf("紧急发布：%s 内需补审（截止 %s）", makeupReviewWindow, out.MakeupReviewDueAt)
	if in.SecondActor != "" {
		detail += "；双人授权：" + in.Actor + " + " + in.SecondActor
	}
	s.audit(AuditEvent{
		Action: ActionChangeEmergency, Actor: in.Actor, ActorType: in.ActorType,
		ChangeID: out.ChangeID, PackageID: out.PackageID, PackageVersion: out.PackageVersion,
		RiskLevel: out.RiskLevel, To: ChangePublished, Reason: in.Reason, Detail: detail,
	})
	// 发布动作本身也要留一条常规事件，保证「发布史」不因为走了紧急通道而出现空洞。
	s.audit(AuditEvent{
		Action: ActionPackagePublished, Actor: in.Actor, ActorType: in.ActorType,
		ChangeID: out.ChangeID, PackageID: published.PackageID, PackageVersion: published.PackageVersion,
		To: published.PackageVersion, Detail: "紧急通道（EMERGENCY）",
	})
	return out, nil
}

// MakeupReview 紧急发布后的事后补审（`contracts/04` §7）。
//
// 超期补审**不拒绝**，只标记 `Late`：拒绝会让「已经发出去的变更」永远处于未补审状态，
// 那比迟到更糟。合规上要的是「有没有补、什么时候补的」，不是一个卡死的记录。
func (s *Service) MakeupReview(changeID, actor, actorType, role, comment string) (Change, error) {
	if !validRole(role) {
		return Change{}, errf(CodeInvalidArgument, "未知的审批角色: "+role)
	}
	if actorType == SubmitterAgent {
		return Change{}, errf(CodeAgentCannotApprove, "agent 不参与审批（A-8），补审同样不行")
	}
	if strings.TrimSpace(comment) == "" {
		return Change{}, errf(CodeInvalidArgument, "补审必须写意见（contracts/04 §9 留存要求）")
	}

	var out Change
	now := s.now()
	err := s.doc.Update(func(d *Doc) error {
		c, err := findChange(d, changeID)
		if err != nil {
			return err
		}
		if !c.Emergency {
			return errf(CodeMakeupReviewRequired, "该变更单不是紧急发布，无需补审")
		}
		if len(c.MakeupReviews) > 0 {
			return errf(CodeMakeupReviewDuplicate, "该紧急变更已补审过")
		}
		if actor == c.EmergencyBy {
			// 自己发、自己补审 = 没有补审。这是紧急通道最容易退化成橡皮图章的地方。
			return errf(CodeMakeupReviewSelf, "补审人不能是紧急发布人本人（contracts/04 §4 职责分离）")
		}
		late := false
		if c.MakeupReviewDueAt != "" {
			if due, e := time.Parse(time.RFC3339, c.MakeupReviewDueAt); e == nil && now.After(due) {
				late = true
			}
		}
		at := nowString(now)
		c.MakeupReviews = append(c.MakeupReviews, MakeupReview{
			Actor: actor, ActorType: actorType, Role: role, At: at, Late: late, Comment: comment,
		})
		c.UpdatedAt = at
		out = *c
		return nil
	})
	if err != nil {
		return Change{}, err
	}

	review := out.MakeupReviews[len(out.MakeupReviews)-1]
	detail := "按时补审"
	if review.Late {
		detail = "超期补审（截止 " + out.MakeupReviewDueAt + "）"
	}
	s.audit(AuditEvent{
		Action: ActionChangeMakeupReviewed, Actor: actor, ActorType: actorType,
		ChangeID: out.ChangeID, PackageID: out.PackageID, PackageVersion: out.PackageVersion,
		RiskLevel: out.RiskLevel, Reason: comment, Detail: detail,
	})
	return out, nil
}

// PendingMakeupReviews 列出「紧急发布但还没补审」的变更单（超时巡检入口）。
func (s *Service) PendingMakeupReviews() []Change {
	var out []Change
	s.doc.View(func(d *Doc) {
		for _, c := range d.Changes {
			if c.MakeupReviewMissing() {
				out = append(out, c)
			}
		}
	})
	return out
}
