package model

import (
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
)

// AffRebate records one invitation rebate: the inviter got Quota because the
// invitee's paid top-up was credited. Written in the same transaction as the
// credit, so per-invitee totals are exact.
type AffRebate struct {
	Id        int   `json:"id"`
	InviterId int   `json:"inviter_id" gorm:"index"`
	InviteeId int   `json:"invitee_id" gorm:"index"`
	Quota     int   `json:"quota"`
	CreatedAt int64 `json:"created_at" gorm:"bigint"`
}

// Invitee is what an inviter may see about someone they invited: a masked
// account name, when they signed up, whether they ever paid, and how much
// rebate they generated. No email, id or balance.
type Invitee struct {
	Account     string `json:"account"`
	CreatedAt   int64  `json:"created_at"`
	Paid        bool   `json:"paid"`
	RebateQuota int    `json:"rebate_quota"`
}

// maxInviteesListed bounds the invite list; a personal referral list longer
// than this is not something the page needs to page through.
const maxInviteesListed = 500

// GetInvitees lists users invited by inviterId, newest first.
func GetInvitees(inviterId int) ([]Invitee, error) {
	var users []struct {
		Id        int
		Username  string
		Email     string
		CreatedAt int64
	}
	if err := DB.Model(&User{}).Select("id", "username", "email", "created_at").
		Where("inviter_id = ?", inviterId).Order("id DESC").Limit(maxInviteesListed).
		Scan(&users).Error; err != nil {
		return nil, err
	}
	out := make([]Invitee, 0, len(users))
	if len(users) == 0 {
		return out, nil
	}
	ids := make([]int, len(users))
	for i, u := range users {
		ids[i] = u.Id
	}

	var paidIds []int
	if err := DB.Model(&TopUp{}).Distinct("user_id").
		Where("user_id IN ? AND status = ?", ids, common.TopUpStatusSuccess).
		Pluck("user_id", &paidIds).Error; err != nil {
		return nil, err
	}
	paid := make(map[int]bool, len(paidIds))
	for _, id := range paidIds {
		paid[id] = true
	}

	var sums []struct {
		InviteeId int
		Total     int
	}
	if err := DB.Model(&AffRebate{}).Select("invitee_id, SUM(quota) AS total").
		Where("inviter_id = ? AND invitee_id IN ?", inviterId, ids).
		Group("invitee_id").Scan(&sums).Error; err != nil {
		return nil, err
	}
	rebate := make(map[int]int, len(sums))
	for _, s := range sums {
		rebate[s.InviteeId] = s.Total
	}

	for _, u := range users {
		name := u.Email
		if name == "" {
			name = u.Username
		}
		out = append(out, Invitee{Account: MaskAccount(name), CreatedAt: u.CreatedAt, Paid: paid[u.Id], RebateQuota: rebate[u.Id]})
	}
	return out, nil
}

// MaskAccount keeps just enough of an account name for the inviter to
// recognise a friend: first two characters, then "***", and for an email the
// domain. "alice@gmail.com" -> "al***@gmail.com", "bob" -> "b***".
func MaskAccount(name string) string {
	local, domain, isEmail := strings.Cut(name, "@")
	keep := 2
	if utf8.RuneCountInString(local) <= 3 {
		keep = 1
	}
	runes := []rune(local)
	if len(runes) < keep {
		keep = len(runes)
	}
	masked := string(runes[:keep]) + "***"
	if isEmail {
		masked += "@" + domain
	}
	return masked
}
