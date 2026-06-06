package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
)

var freeAbuseLimiter common.InMemoryRateLimiter

const freeAbuseWindowSeconds = 60

// TrackFreeModelUsage counts a single free-model request for a user and, when the
// per-minute count exceeds the configured threshold while the user's balance is
// non-positive, auto-sets BlockFreeWhenNoQuota on the user. No-op when the global
// auto-block setting is disabled. The flag is cleared automatically on the next
// quota top-up (see model.IncreaseUserQuota).
func TrackFreeModelUsage(userId int, userQuota int) {
	setting := operation_setting.GetQuotaSetting()
	if !setting.EnableFreeAbuseAutoBlock {
		return
	}
	maxPerMin := setting.FreeAbuseMaxPerMinute
	if maxPerMin <= 0 {
		return
	}

	over := recordFreeUsage(userId, maxPerMin)
	if !over || userQuota > 0 {
		return
	}

	gopool.Go(func() {
		autoBlockUser(userId)
	})
}

// recordFreeUsage increments the per-user request counter for the current window
// and reports whether the count has exceeded maxPerMin. Uses Redis when enabled
// (cross-instance), otherwise an in-memory sliding window (single instance).
func recordFreeUsage(userId int, maxPerMin int) bool {
	if common.RedisEnabled {
		ctx := context.Background()
		key := fmt.Sprintf("freeAbuse:user:%d", userId)
		count, err := common.RDB.Incr(ctx, key).Result()
		if err != nil {
			common.SysLog("free abuse counter incr failed: " + err.Error())
			return false
		}
		if count == 1 {
			common.RDB.Expire(ctx, key, freeAbuseWindowSeconds*time.Second)
		}
		return count > int64(maxPerMin)
	}

	// In-memory fallback: Request returns false once the window is saturated.
	key := fmt.Sprintf("freeAbuse:user:%d", userId)
	freeAbuseLimiter.Init(freeAbuseWindowSeconds * time.Second)
	allowed := freeAbuseLimiter.Request(key, maxPerMin, freeAbuseWindowSeconds)
	return !allowed
}

func autoBlockUser(userId int) {
	s, err := model.GetUserSetting(userId, false)
	if err == nil && s.BlockFreeWhenNoQuota {
		return // already blocked
	}
	user, err := model.GetUserById(userId, true)
	if err != nil {
		return
	}
	ns := user.GetSetting()
	if ns.BlockFreeWhenNoQuota {
		return
	}
	ns.BlockFreeWhenNoQuota = true
	user.SetSetting(ns)
	if err := user.Update(false); err != nil {
		common.SysLog(fmt.Sprintf("failed to auto-block user %d for free-model abuse: %s", userId, err.Error()))
		return
	}
	model.RecordLog(userId, model.LogTypeManage, i18n.Translate("ctrl.auto_block_free_abuse"))
}
