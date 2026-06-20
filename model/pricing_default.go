package model

import (
	"sort"
	"strings"
)

// 简化的供应商映射规则
var defaultVendorRules = map[string]string{
	"gpt":       "OpenAI",
	"dall-e":    "OpenAI",
	"whisper":   "OpenAI",
	"o1":        "OpenAI",
	"o3":        "OpenAI",
	"claude":    "Anthropic",
	"gemini":    "Google",
	"gemma":     "Google",
	"moonshot":  "Moonshot",
	"kimi":      "Moonshot",
	"chatglm":   "Zhipu",
	"glm-":      "Zhipu",
	"qwen":      "Alibaba",
	"deepseek":  "DeepSeek",
	"abab":      "MiniMax",
	"minimax":   "MiniMax",
	"ernie":     "Baidu",
	"spark":     "iFlytek",
	"hunyuan":   "Tencent",
	"command":   "Cohere",
	"cohere":    "Cohere",
	"@cf/":      "Cloudflare",
	"360":       "360",
	"yi":        "01.AI",
	"jina":      "Jina",
	"mistral":   "Mistral",
	"codestral": "Mistral",
	"devstral":  "Mistral",
	"pixtral":   "Mistral",
	"magistral": "Mistral",
	"grok":      "xAI",
	"llama":     "Meta",
	"nemotron":  "Nvidia",
	"nvidia":    "Nvidia",
	"nex-n":     "Nex AGI",
	"step-":     "StepFun",
	"doubao":    "ByteDance",
	"kling":     "Kuaishou",
	"jimeng":    "Jimeng",
	"vidu":      "Vidu",
}

// defaultVendorPatterns is defaultVendorRules keyed longest-first so a specific
// substring (e.g. "minimax") wins over a shorter accidental one. Map iteration
// order is random, so callers that need a deterministic single match use this.
var defaultVendorPatterns = buildSortedVendorPatterns()

func buildSortedVendorPatterns() []string {
	patterns := make([]string, 0, len(defaultVendorRules))
	for pattern := range defaultVendorRules {
		patterns = append(patterns, pattern)
	}
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return patterns[i] < patterns[j]
	})
	return patterns
}

// ResolveDefaultVendor pattern-matches a model name to a vendor name + icon,
// the same rules sync uses when a channel carries no explicit vendor. Returns
// empty strings when nothing matches. Used by rankings to label models whose
// models-table row has vendor_id=0 (would otherwise render as "Unknown").
func ResolveDefaultVendor(modelName string) (vendor string, icon string) {
	modelLower := strings.ToLower(modelName)
	for _, pattern := range defaultVendorPatterns {
		if strings.Contains(modelLower, pattern) {
			name := defaultVendorRules[pattern]
			return name, getDefaultVendorIcon(name)
		}
	}
	return "", ""
}

// 供应商默认图标映射
var defaultVendorIcons = map[string]string{
	"OpenAI":     "OpenAI",
	"Anthropic":  "Claude.Color",
	"Google":     "Gemini.Color",
	"Moonshot":   "Moonshot",
	"Zhipu":      "Zhipu.Color",
	"Alibaba":    "Qwen.Color",
	"DeepSeek":   "DeepSeek.Color",
	"MiniMax":    "Minimax.Color",
	"Baidu":      "Wenxin.Color",
	"iFlytek":    "Spark.Color",
	"Tencent":    "Hunyuan.Color",
	"Cohere":     "Cohere.Color",
	"Cloudflare": "Cloudflare.Color",
	"360":        "Ai360.Color",
	"01.AI":      "Yi.Color",
	"Jina":       "Jina",
	"Mistral":    "Mistral.Color",
	"Nvidia":     "Nvidia.Color",
	"StepFun":    "Stepfun.Color",
	"xAI":        "XAI",
	"Meta":       "Ollama",
	"ByteDance":  "Doubao.Color",
	"Kuaishou":   "Kling.Color",
	"Jimeng":     "Jimeng.Color",
	"Vidu":       "Vidu",
	"Microsoft":  "AzureAI",
	"Azure":      "AzureAI",
}

// initDefaultVendorMapping 简化的默认供应商映射
func initDefaultVendorMapping(metaMap map[string]*Model, vendorMap map[int]*Vendor, enableAbilities []AbilityWithChannel) {
	for _, ability := range enableAbilities {
		modelName := ability.Model
		if _, exists := metaMap[modelName]; exists {
			continue
		}

		// 匹配供应商
		vendorID := 0
		modelLower := strings.ToLower(modelName)
		for pattern, vendorName := range defaultVendorRules {
			if strings.Contains(modelLower, pattern) {
				vendorID = getOrCreateVendor(vendorName, vendorMap)
				break
			}
		}

		// 创建模型元数据
		metaMap[modelName] = &Model{
			ModelName: modelName,
			VendorID:  vendorID,
			Status:    1,
			NameRule:  NameRuleExact,
		}
	}
}

// 查找或创建供应商
func getOrCreateVendor(vendorName string, vendorMap map[int]*Vendor) int {
	// 查找现有供应商
	for id, vendor := range vendorMap {
		if vendor.Name == vendorName {
			return id
		}
	}

	// 创建新供应商
	newVendor := &Vendor{
		Name:   vendorName,
		Status: 1,
		Icon:   getDefaultVendorIcon(vendorName),
	}

	if err := newVendor.Insert(); err != nil {
		return 0
	}

	vendorMap[newVendor.Id] = newVendor
	return newVendor.Id
}

// 获取供应商默认图标
func getDefaultVendorIcon(vendorName string) string {
	if icon, exists := defaultVendorIcons[vendorName]; exists {
		return icon
	}
	return ""
}
