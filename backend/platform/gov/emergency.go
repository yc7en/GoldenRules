package gov

import (
	"fmt"
	"strings"
	"time"

	"goldenrules/platform/packages"
)

// Emergency channel (`contracts/04` §7).
//
// When a production incident needs an immediate fix, "the releaser publishes directly" is allowed, but the price must be booked explicitly:
//
//	① a makeup review (post-hoc approval) is required within 24h; an overdue review is marked Late;
//	② the whole flow is audited and flagged EMERGENCY;
//	③ only L3 and below may use it; an emergency L4 change requires two-person authorization (the releaser + 1 owner).
//
// Deliberately, **no** backdoor is opened here: A-8 is not skipped (an agent never publishes), auditing is not skipped,
// and "the change request must exist" is not skipped. Emergency only compresses "the time spent waiting for approval", never "who is accountable".
const (
	CodeEmergencyLevelInvalid   = "GOV_EMERGENCY_LEVEL_INVALID"
	CodeEmergencySecondRequired = "GOV_EMERGENCY_SECOND_REQUIRED"
	CodeEmergencySecondInvalid  = "GOV_EMERGENCY_SECOND_INVALID"
	CodeEmergencyStateInvalid   = "GOV_EMERGENCY_STATE_INVALID"
	CodeMakeupReviewRequired    = "GOV_MAKEUP_REVIEW_REQUIRED"
	CodeMakeupReviewDuplicate   = "GOV_MAKEUP_REVIEW_DUPLICATE"
	CodeMakeupReviewSelf        = "GOV_MAKEUP_REVIEW_SELF"
)

// Makeup review window (`contracts/04` §7).
const makeupReviewWindow = 24 * time.Hour

// EmergencyInput is the input for an emergency publish.
type EmergencyInput struct {
	ChangeID   string
	Actor      string
	ActorType  string
	Reason     string
	ValidFrom  string
	ValidUntil string
	// SecondActor is required for L4: the co-authorizing owner (must be another person, and not an agent)
	SecondActor     string
	SecondActorType string
}

// EmergencyPublish publishes through the emergency channel: it skips waiting for approval, but records in full "who triggered it, who co-authorized it".
//
// If the version is still draft, it is first pushed to approved and then published — this does not bypass the state machine; it **merges the two steps
// into one operation on an audited path**, keeping the state machine continuous for every other reader.
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

	// Level threshold: L4 requires two-person authorization (§7). This is the only "extra threshold" of the emergency channel,
	// and the only thing it must never compress — the more urgent a high-risk change, the more it needs a second pair of eyes.
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

	// Version advance: draft → approved → published in one go, but every step really happens (other readers observe a continuous state)
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
	// The publish itself also records a regular event, so the "publish history" has no hole just because the emergency channel was used.
	s.audit(AuditEvent{
		Action: ActionPackagePublished, Actor: in.Actor, ActorType: in.ActorType,
		ChangeID: out.ChangeID, PackageID: published.PackageID, PackageVersion: published.PackageVersion,
		To: published.PackageVersion, Detail: "紧急通道（EMERGENCY）",
	})
	return out, nil
}

// MakeupReview is the post-hoc review after an emergency publish (`contracts/04` §7).
//
// An overdue makeup review is **not refused**, only marked `Late`: refusing would leave "a change that already shipped" permanently unreviewed,
// which is worse than being late. What compliance wants is "was it reviewed, and when", not a record that is permanently stuck.
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
			// Publishing yourself and reviewing yourself = no review. This is where the emergency channel most easily degrades into a rubber stamp.
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

// PendingMakeupReviews lists change requests that were emergency-published but not yet makeup-reviewed (the overdue patrol entry point).
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
