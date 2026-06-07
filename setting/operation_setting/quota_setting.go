package operation_setting

import "github.com/QuantumNous/new-api/setting/config"

type QuotaSetting struct {
	EnableFreeModelPreConsume bool `json:"enable_free_model_pre_consume"` // 是否对免费模型启用预消耗
	EnableFreeAbuseAutoBlock  bool `json:"enable_free_abuse_auto_block"`  // 检测到免费模型滥用时自动封禁
	FreeAbuseMaxPerMinute     int  `json:"free_abuse_max_per_minute"`     // 自动封禁前每分钟允许的免费模型请求数
	ChargeOnError             bool `json:"charge_on_error"`               // 请求失败时是否仍然扣费（不退还预扣额度）
}

// 默认配置
var quotaSetting = QuotaSetting{
	EnableFreeModelPreConsume: true,
	EnableFreeAbuseAutoBlock:  false,
	FreeAbuseMaxPerMinute:     5,
	ChargeOnError:             false,
}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("quota_setting", &quotaSetting)
}

func GetQuotaSetting() *QuotaSetting {
	return &quotaSetting
}
