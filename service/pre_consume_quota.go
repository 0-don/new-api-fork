package service

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// effectiveGroupRatio resolves the group ratio that actually applies to this
// request, honoring auto-group failover and per-user-group special ratios. This
// is the same resolution billing settlement uses.
func effectiveGroupRatio(c *gin.Context, relayInfo *relaycommon.RelayInfo) float64 {
	usingGroup := relayInfo.UsingGroup
	if autoGroup, exists := common.GetContextKey(c, constant.ContextKeyAutoGroup); exists {
		if g, ok := autoGroup.(string); ok {
			usingGroup = g
		}
	}
	groupRatio := ratio_setting.GetGroupRatio(usingGroup)
	if userGroupRatio, ok := ratio_setting.GetGroupGroupRatio(relayInfo.UserGroup, usingGroup); ok {
		groupRatio = userGroupRatio
	}
	return groupRatio
}

// requestChargesQuota reports whether this request lands on a PAID group, i.e.
// the owner intends to charge for it. A group ratio > 0 signals a paid request
// even when the model's base ratio/price happens to be 0 (e.g. a paid model
// priced entirely via per-channel group markups: modelRatio 0 * groupRatio 0.24
// still bills 0 per token, but is NOT a free model). Used to gate zero-balance
// users out of paid models while still allowing genuinely free ones (groupRatio
// == 0), which is the whole point of a quota=0 free-only token.
func requestChargesQuota(c *gin.Context, relayInfo *relaycommon.RelayInfo) bool {
	return effectiveGroupRatio(c, relayInfo) > 0
}

// tokenBlockedOnPaidGroup reports whether a quota-limited token with no remaining
// quota is hitting a PAID group. Such a token must never reach a paid model even
// when the per-token cost rounds to 0 (e.g. a model whose base ratio is 0 but the
// group adds a markup) and even when the owning user still has wallet balance:
// a quota=0 token is a free-only key by definition. Unlimited tokens are exempt.
func tokenBlockedOnPaidGroup(c *gin.Context, relayInfo *relaycommon.RelayInfo) bool {
	if relayInfo.TokenUnlimited {
		return false
	}
	if c.GetInt("token_quota") > 0 {
		return false
	}
	return requestChargesQuota(c, relayInfo)
}

// cameFromFreeFailover reports whether an auto-token request has fallen through
// its free groups and is now sitting on a PAID group. Free groups are ordered
// first in AutoGroups, so reaching a ratio>0 group on an "auto" token means the
// free providers for this model were exhausted (429) and failover advanced to a
// paid one. Used to explain a $0-balance error instead of a bare "insufficient
// balance".
func cameFromFreeFailover(c *gin.Context, relayInfo *relaycommon.RelayInfo) bool {
	if relayInfo.TokenGroup != "auto" {
		return false
	}
	return effectiveGroupRatio(c, relayInfo) > 0
}

func ReturnPreConsumedQuota(c *gin.Context, relayInfo *relaycommon.RelayInfo) {
	if relayInfo.FinalPreConsumedQuota != 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费额度 %s", relayInfo.UserId, logger.FormatQuota(relayInfo.FinalPreConsumedQuota)))
		gopool.Go(func() {
			relayInfoCopy := *relayInfo

			err := PostConsumeQuota(&relayInfoCopy, -relayInfoCopy.FinalPreConsumedQuota, 0, false)
			if err != nil {
				common.SysLog("error return pre-consumed quota: " + err.Error())
			}
		})
	}
}

// PreConsumeQuota checks if the user has enough quota to pre-consume.
// It returns the pre-consumed quota if successful, or an error if not.
func PreConsumeQuota(c *gin.Context, preConsumedQuota int, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
	if err != nil {
		return types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
	}
	// A quota=0 (non-unlimited) token is a free-only key: block it from any paid
	// group even when the user still has wallet balance and the per-token cost
	// rounds to 0 (model base ratio 0 * group markup).
	if tokenBlockedOnPaidGroup(c, relayInfo) {
		if cameFromFreeFailover(c, relayInfo) {
			return types.NewErrorWithStatusCode(fmt.Errorf(i18n.T(c, "svc.free_tier_exhausted_paid_fallback")), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		return types.NewErrorWithStatusCode(fmt.Errorf("令牌额度不足，无法访问付费模型"), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	// Only a genuinely free request (paid group ratio == 0) may proceed on a
	// non-positive balance. A paid group (ratio > 0) bills the user even when the
	// per-token math rounds to 0, so a zero-balance token must be blocked there.
	if userQuota <= 0 && requestChargesQuota(c, relayInfo) {
		if cameFromFreeFailover(c, relayInfo) {
			return types.NewErrorWithStatusCode(fmt.Errorf(i18n.T(c, "svc.free_tier_exhausted_paid_fallback")), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		return types.NewErrorWithStatusCode(fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	if userQuota-preConsumedQuota < 0 {
		return types.NewErrorWithStatusCode(fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}

	trustQuota := common.GetTrustQuota()

	relayInfo.UserQuota = userQuota
	if userQuota > trustQuota {
		// 用户额度充足，判断令牌额度是否充足
		if !relayInfo.TokenUnlimited {
			// 非无限令牌，判断令牌额度是否充足
			tokenQuota := c.GetInt("token_quota")
			if tokenQuota > trustQuota {
				// 令牌额度充足，信任令牌
				preConsumedQuota = 0
				logger.LogInfo(c, fmt.Sprintf("用户 %d 剩余额度 %s 且令牌 %d 额度 %d 充足, 信任且不需要预扣费", relayInfo.UserId, logger.FormatQuota(userQuota), relayInfo.TokenId, tokenQuota))
			}
		} else {
			// in this case, we do not pre-consume quota
			// because the user has enough quota
			preConsumedQuota = 0
			logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足且为无限额度令牌, 信任且不需要预扣费", relayInfo.UserId))
		}
	}

	if preConsumedQuota > 0 {
		err := PreConsumeTokenQuota(relayInfo, preConsumedQuota)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		err = model.DecreaseUserQuota(relayInfo.UserId, preConsumedQuota, false)
		if err != nil {
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		logger.LogInfo(c, fmt.Sprintf("用户 %d 预扣费 %s, 预扣费后剩余额度: %s", relayInfo.UserId, logger.FormatQuota(preConsumedQuota), logger.FormatQuota(userQuota-preConsumedQuota)))
	}
	relayInfo.FinalPreConsumedQuota = preConsumedQuota
	return nil
}
