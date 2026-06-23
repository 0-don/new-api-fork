package service

import (
	"github.com/QuantumNous/new-api/i18n"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

func translatef(key string, fallback string, args ...any) string {
	template := i18n.Translate(key)
	if template == "" || template == key {
		template = fallback
	}
	return fmt.Sprintf(template, args...)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(translatef("svc.channel_error_occurred_preparing_to_disable_reason", "Channel '%s' (#%d) error occurred, preparing to disable. Reason: %s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(translatef("svc.channel_auto_disable_not_enabled_skipping_disable", "Channel '%s' (#%d) auto-disable not enabled, skipping disable", channelError.ChannelName, channelError.ChannelId))
		return
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	if success && operation_setting.GetMonitorSetting().ChannelStatusNotifyEnabled {
		subject := translatef("svc.channel_has_been_disabled", "Channel '%s' (#%d) has been disabled", channelError.ChannelName, channelError.ChannelId)
		content := translatef("svc.channel_has_been_disabled_reason", "Channel '%s' (#%d) has been disabled, reason: %s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success && operation_setting.GetMonitorSetting().ChannelStatusNotifyEnabled {
		subject := translatef("svc.channel_has_been_enabled", "Channel '%s' (#%d) has been enabled", channelName, channelId)
		content := translatef("svc.channel_has_been_enabled_dc21", "Channel '%s' (#%d) has been enabled", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	// Content/policy-driven rejections (safety, content_filter, invalid_argument,
	// etc.) are deterministic, not per-channel faults. Skipping retry without
	// skipping disable would still auto-ban a healthy channel on a bad request.
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	// A clean test is the strongest recovery signal. But a test that fails with
	// an error that is not the channel's fault must not pin it disabled forever:
	// deterministic request rejections (invalid_argument, quota, context overflow)
	// and our own infra faults (distributor "no available channel", DB errors)
	// would otherwise create a re-enable deadlock - the channel never recovers
	// because every test cycle hits an error unrelated to its health.
	if newAPIError != nil && !errorIsChannelFault(newAPIError) {
		return false
	}
	return true
}

// errorIsChannelFault reports whether a test error genuinely indicates the
// channel/upstream is unhealthy (so it should stay disabled), as opposed to a
// deterministic request rejection or a local routing/DB fault that says nothing
// about channel health. Inverse drives the ShouldEnableChannel recovery gate.
func errorIsChannelFault(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	// Explicit channel-tagged errors (channel:invalid_key, channel:aws_client_error,
	// channel:response_time_exceeded, ...) are real channel faults.
	if types.IsChannelError(err) {
		return true
	}
	// Deterministic content/policy/quota/context rejections: same outcome on any
	// channel, so they do not signal this channel is broken.
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) {
		return false
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	// Local infrastructure faults, not upstream channel health.
	switch err.GetErrorCode() {
	case types.ErrorCodeQueryDataError,
		types.ErrorCodeUpdateDataError,
		types.ErrorCodeGetChannelFailed:
		return false
	}
	return true
}
