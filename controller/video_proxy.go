package controller

import (
	"errors"
	"github.com/QuantumNous/new-api/i18n"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

// videoProxyError returns a standardized OpenAI-style error response.
func videoProxyError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}

func VideoProxy(c *gin.Context) {
	taskID := c.Param("task_id")
	if taskID == "" {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "task_id is required")
		return
	}

	userID := c.GetInt("id")
	task, exists, err := model.GetByTaskId(userID, taskID)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_query_task"), taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to query task")
		return
	}
	if !exists || task == nil {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "Task not found")
		return
	}

	if task.Status != model.TaskStatusSuccess {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf(i18n.Translate("ctrl.task_is_not_completed_yet_current_status"), task.Status))
		return
	}

	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_get_channel_for_task"), taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to retrieve channel information")
		return
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	var videoURL string
	proxy := channel.GetSetting().Proxy
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_create_proxy_client_for_task"), taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy client")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_create_request"), err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy request")
		return
	}

	switch channel.Type {
	case constant.ChannelTypeGemini:
		apiKey := task.PrivateData.Key
		if apiKey == "" {
			logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.missing_stored_api_key_for_gemini_task"), taskID))
			videoProxyError(c, http.StatusInternalServerError, "server_error", "API key not stored for task")
			return
		}
		videoURL, err = getGeminiVideoURL(channel, task, apiKey)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_resolve_gemini_video_url_for"), taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to resolve Gemini video URL")
			return
		}
		req.Header.Set("x-goog-api-key", apiKey)
	case constant.ChannelTypeVertexAi:
		videoURL, err = getVertexVideoURL(channel, task)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_resolve_vertex_video_url_for"), taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to resolve Vertex video URL")
			return
		}
	case constant.ChannelTypeOpenAI, constant.ChannelTypeSora:
		videoURL = fmt.Sprintf("%s/v1/videos/%s/content", baseURL, task.GetUpstreamTaskID())
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	default:
		// Video URL is stored in PrivateData.ResultURL (fallback to FailReason for old data)
		videoURL = task.GetResultURL()
	}

	videoURL = strings.TrimSpace(videoURL)
	// When the adaptor returned a data: URI, the poller stored a self-referencing
	// proxy URL in result_url (to keep the JSON status response small) and parked
	// the actual bytes in task.Data. Detect that case (URL points to this same
	// proxy endpoint) and try to extract the image directly from task.Data.
	if videoURL == "" || strings.Contains(videoURL, "/v1/videos/"+taskID+"/content") {
		// `?index=N` selects which image in a batch to return (0-based).
		// Default 0 keeps existing single-image clients working.
		idx := 0
		if raw := strings.TrimSpace(c.Query("index")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
				idx = n
			}
		}
		if dataURL := extractDataURLFromTaskData(task, idx); dataURL != "" {
			if err := writeVideoDataURL(c, dataURL); err != nil {
				logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_decode_video_data_url_for"), taskID, err.Error()))
				videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
			}
			return
		}
	}
	if videoURL == "" {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.video_url_is_empty_for_task"), taskID))
		videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
		return
	}

	if strings.HasPrefix(videoURL, "data:") {
		if err := writeVideoDataURL(c, videoURL); err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_decode_video_data_url_for"), taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
		}
		return
	}

	fetchSetting := system_setting.GetFetchSetting()
	if err := common.ValidateURLWithFetchSetting(videoURL, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, fetchSetting.ApplyIPFilterForDomain); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Video URL blocked for task %s: %v", taskID, err))
		videoProxyError(c, http.StatusForbidden, "server_error", fmt.Sprintf("request blocked: %v", err))
		return
	}

	req.URL, err = url.Parse(videoURL)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_parse_url"), videoURL, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "Failed to create proxy request")
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_fetch_video_from"), videoURL, err.Error()))
		videoProxyError(c, http.StatusBadGateway, "server_error", "Failed to fetch video content")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.upstream_returned_status_for"), resp.StatusCode, videoURL))
		videoProxyError(c, http.StatusBadGateway, "server_error",
			fmt.Sprintf(i18n.Translate("ctrl.upstream_service_returned_status"), resp.StatusCode))
		return
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Writer.Header().Set("Cache-Control", "public, max-age=86400")
	c.Writer.WriteHeader(resp.StatusCode)
	if _, err = io.Copy(c.Writer, resp.Body); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf(i18n.Translate("ctrl.failed_to_stream_video_content"), err.Error()))
	}
}

// extractDataURLFromTaskData walks the raw upstream response we cached in
// task.Data (currently used by the comfyui/runpod path) looking for an
// inlined image. Returns a data: URI ready for writeVideoDataURL or "".
//
// idx selects which image in a batch to return (0-based). Out-of-range
// requests return "" so the caller surfaces a 502 rather than silently
// substituting a different image. idx=0 preserves the original
// single-image behaviour.
//
// Known shapes:
//   - RunPod worker-comfyui: { "output": { "images": [{"data": "<b64>", "filename": "...png"}] } }
//   - Generic data URI parked in any string field
//
// We avoid a full recursive walker — too easy to grab user-prompt echoes by
// accident. The two paths above cover the cases we actually emit today.
func extractDataURLFromTaskData(task *model.Task, idx int) string {
	raw, err := task.Data.MarshalJSON()
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var envelope struct {
		Output struct {
			Images []struct {
				Data     string `json:"data"`
				Filename string `json:"filename"`
				URL      string `json:"url"`
			} `json:"images"`
		} `json:"output"`
	}
	if err := common.Unmarshal(raw, &envelope); err == nil {
		if idx < 0 || idx >= len(envelope.Output.Images) {
			return ""
		}
		img := envelope.Output.Images[idx]
		if strings.HasPrefix(img.URL, "data:") {
			return img.URL
		}
		if img.Data != "" {
			mime := "image/png"
			if strings.HasSuffix(strings.ToLower(img.Filename), ".jpg") || strings.HasSuffix(strings.ToLower(img.Filename), ".jpeg") {
				mime = "image/jpeg"
			} else if strings.HasSuffix(strings.ToLower(img.Filename), ".webp") {
				mime = "image/webp"
			}
			return "data:" + mime + ";base64," + img.Data
		}
	}
	return ""
}

func writeVideoDataURL(c *gin.Context, dataURL string) error {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return errors.New(i18n.Translate("ctrl.invalid_data_url"))
	}

	header := parts[0]
	payload := parts[1]
	if !strings.HasPrefix(header, "data:") || !strings.Contains(header, ";base64") {
		return errors.New(i18n.Translate("ctrl.unsupported_data_url"))
	}

	mimeType := strings.TrimPrefix(header, "data:")
	mimeType = strings.TrimSuffix(mimeType, ";base64")
	if mimeType == "" {
		mimeType = "video/mp4"
	}

	videoBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		videoBytes, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return err
		}
	}

	c.Writer.Header().Set("Content-Type", mimeType)
	c.Writer.Header().Set("Cache-Control", "public, max-age=86400")
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(videoBytes)
	return err
}
