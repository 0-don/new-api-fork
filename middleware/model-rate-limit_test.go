package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScaleForNewUser(t *testing.T) {
	cases := []struct {
		name   string
		factor float64
		count  int
		want   int
	}{
		{"factor off (1.0) keeps count", 1.0, 20, 20},
		{"factor halves", 0.5, 20, 10},
		{"factor floors to 1, never 0", 0.1, 5, 1},
		{"zero count stays zero", 0.25, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setting.ModelRequestRateLimitNewUserFactor = tc.factor
			assert.Equal(t, tc.want, scaleForNewUser(tc.count))
		})
	}
}

func TestGetModelRateLimit(t *testing.T) {
	require.NoError(t, setting.UpdateModelRequestRateLimitModelsByJSONString(`{"kimi-k2.6:free":[0,20],"glm-4.5-flash:free":[5,15]}`))

	total, success, found := setting.GetModelRateLimit("kimi-k2.6:free")
	assert.True(t, found)
	assert.Equal(t, 0, total)
	assert.Equal(t, 20, success)

	_, _, found = setting.GetModelRateLimit("kimi-k2.6") // paid twin not in map
	assert.False(t, found)

	_, _, found = setting.GetModelRateLimit("gpt-5.5")
	assert.False(t, found)
}

func newCtxWithUser(createdAt int64, usedQuota int) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(string(constant.ContextKeyUserCreatedAt), createdAt)
	c.Set(string(constant.ContextKeyUserUsedQuota), usedQuota)
	return c
}

func TestIsNewUser(t *testing.T) {
	now := time.Now().Unix()
	dayOld := now - 86400
	weekOld := now - 7*86400

	t.Run("thresholds unset => never new", func(t *testing.T) {
		setting.ModelRequestRateLimitNewUserMaxAgeDays = 0
		setting.ModelRequestRateLimitNewUserMaxUsedQuota = 0
		assert.False(t, isNewUser(newCtxWithUser(dayOld, 0)))
	})

	t.Run("young account is new (age OR)", func(t *testing.T) {
		setting.ModelRequestRateLimitNewUserMaxAgeDays = 3
		setting.ModelRequestRateLimitNewUserMaxUsedQuota = 0
		assert.True(t, isNewUser(newCtxWithUser(dayOld, 1_000_000)))
		assert.False(t, isNewUser(newCtxWithUser(weekOld, 0)))
	})

	t.Run("low spend is new (quota OR), even if old", func(t *testing.T) {
		setting.ModelRequestRateLimitNewUserMaxAgeDays = 3
		setting.ModelRequestRateLimitNewUserMaxUsedQuota = 500000
		assert.True(t, isNewUser(newCtxWithUser(weekOld, 100)))
		assert.False(t, isNewUser(newCtxWithUser(weekOld, 1_000_000)))
	})
}
