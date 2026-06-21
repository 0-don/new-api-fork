package setting

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

var ModelRequestRateLimitEnabled = false
var ModelRequestRateLimitDurationMinutes = 1
var ModelRequestRateLimitCount = 0
var ModelRequestRateLimitSuccessCount = 1000
var ModelRequestRateLimitGroup = map[string][2]int{}
var ModelRequestRateLimitMutex sync.RWMutex

// Per-model limits keyed by exact model name (the sync pre-expands globs to exact
// `:free` names). Value is [total, success] per the shared duration window.
var ModelRequestRateLimitModels = map[string][2]int{}
var ModelRequestRateLimitModelsMutex sync.RWMutex

// New-user throttle: when a user counts as "new", per-model counts are multiplied by
// the factor. Factor 1 (or thresholds 0) disables the throttle.
var ModelRequestRateLimitNewUserFactor = 1.0
var ModelRequestRateLimitNewUserMaxAgeDays = 0
var ModelRequestRateLimitNewUserMaxUsedQuota = 0

func ModelRequestRateLimitGroup2JSONString() string {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	jsonBytes, err := json.Marshal(ModelRequestRateLimitGroup)
	if err != nil {
		common.SysLog("error marshalling model ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateModelRequestRateLimitGroupByJSONString(jsonStr string) error {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	ModelRequestRateLimitGroup = make(map[string][2]int)
	return json.Unmarshal([]byte(jsonStr), &ModelRequestRateLimitGroup)
}

func GetGroupRateLimit(group string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	if ModelRequestRateLimitGroup == nil {
		return 0, 0, false
	}

	limits, found := ModelRequestRateLimitGroup[group]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func CheckModelRequestRateLimitGroup(jsonStr string) error {
	checkModelRequestRateLimitGroup := make(map[string][2]int)
	err := json.Unmarshal([]byte(jsonStr), &checkModelRequestRateLimitGroup)
	if err != nil {
		return err
	}
	for group, limits := range checkModelRequestRateLimitGroup {
		if limits[0] < 0 || limits[1] < 1 {
			return fmt.Errorf(common.Translate("setting.group_has_negative_rate_limit_values"), group, limits[0], limits[1])
		}
		if limits[0] > math.MaxInt32 || limits[1] > math.MaxInt32 {
			return fmt.Errorf(common.Translate("setting.group_has_max_rate_limits_value_2147483647"), group, limits[0], limits[1])
		}
	}

	return nil
}

func ModelRequestRateLimitModels2JSONString() string {
	ModelRequestRateLimitModelsMutex.RLock()
	defer ModelRequestRateLimitModelsMutex.RUnlock()

	jsonBytes, err := json.Marshal(ModelRequestRateLimitModels)
	if err != nil {
		common.SysLog("error marshalling model rate limit models: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateModelRequestRateLimitModelsByJSONString(jsonStr string) error {
	ModelRequestRateLimitModelsMutex.Lock()
	defer ModelRequestRateLimitModelsMutex.Unlock()

	ModelRequestRateLimitModels = make(map[string][2]int)
	return json.Unmarshal([]byte(jsonStr), &ModelRequestRateLimitModels)
}

func GetModelRateLimit(model string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitModelsMutex.RLock()
	defer ModelRequestRateLimitModelsMutex.RUnlock()

	if ModelRequestRateLimitModels == nil {
		return 0, 0, false
	}
	limits, found := ModelRequestRateLimitModels[model]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func HasModelRateLimits() bool {
	ModelRequestRateLimitModelsMutex.RLock()
	defer ModelRequestRateLimitModelsMutex.RUnlock()
	return len(ModelRequestRateLimitModels) > 0
}

func CheckModelRequestRateLimitModels(jsonStr string) error {
	checkModels := make(map[string][2]int)
	err := json.Unmarshal([]byte(jsonStr), &checkModels)
	if err != nil {
		return err
	}
	for model, limits := range checkModels {
		if limits[0] < 0 || limits[1] < 1 {
			return fmt.Errorf(common.Translate("setting.group_has_negative_rate_limit_values"), model, limits[0], limits[1])
		}
		if limits[0] > math.MaxInt32 || limits[1] > math.MaxInt32 {
			return fmt.Errorf(common.Translate("setting.group_has_max_rate_limits_value_2147483647"), model, limits[0], limits[1])
		}
	}
	return nil
}
