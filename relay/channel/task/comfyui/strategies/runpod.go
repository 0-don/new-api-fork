package strategies

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// RunPod speaks RunPod serverless's /run + /status API. The endpoint id is the
// per-deployment identifier you get from the RunPod dashboard; the worker behind
// it is expected to be runpod/worker-comfyui (or compatible) which accepts a
// patched ComfyUI prompt graph at input.workflow.
// Submission base URL: https://api.runpod.ai
// Submit:  POST {base}/v2/{endpoint}/run                -> { id, status }
// Status:  GET  {base}/v2/{endpoint}/status/{id}        -> { id, status, output }
type RunPod struct {
	APIKey   string
	Endpoint string // e.g. "abc123xyz"
}

func (r *RunPod) Name() string { return "runpod" }

func (r *RunPod) endpointSegment() string {
	return strings.Trim(r.Endpoint, "/")
}

func (r *RunPod) base(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return "https://api.runpod.ai"
	}
	return strings.TrimRight(baseURL, "/")
}

func (r *RunPod) BuildSubmitRequest(baseURL string, patchedWorkflow []byte, extras *SubmitExtras) (string, []byte, map[string]string, error) {
	if r.endpointSegment() == "" {
		return "", nil, nil, fmt.Errorf("comfyui/runpod: missing endpoint id (set channel `app` field)")
	}
	var graph map[string]any
	if err := common.Unmarshal(patchedWorkflow, &graph); err != nil {
		return "", nil, nil, fmt.Errorf("comfyui/runpod: parse workflow: %w", err)
	}
	input := map[string]any{
		"workflow": graph,
	}
	// worker-comfyui accepts an optional `input.images: [{name, image}]`
	// array; each entry is written to ComfyUI's input directory before the
	// workflow runs, so LoadImage nodes can reference it by name. Used for
	// reference-image composition (Flux 2 compose).
	if extras != nil && len(extras.Images) > 0 {
		input["images"] = extras.Images
	}
	body := map[string]any{
		"input": input,
	}
	raw, err := common.Marshal(body)
	if err != nil {
		return "", nil, nil, err
	}
	url := fmt.Sprintf("%s/v2/%s/run", r.base(baseURL), r.endpointSegment())
	headers := map[string]string{
		"Authorization": "Bearer " + r.APIKey,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
	}
	return url, raw, headers, nil
}

type runpodSubmitResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (r *RunPod) ParseSubmitResponse(body []byte) (string, error) {
	var resp runpodSubmitResp
	if err := common.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("comfyui/runpod: parse submit: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("comfyui/runpod: empty id (resp=%s)", string(body))
	}
	return resp.ID, nil
}

func (r *RunPod) StatusURL(baseURL, upstreamTaskID string) string {
	return fmt.Sprintf("%s/v2/%s/status/%s", r.base(baseURL), r.endpointSegment(), upstreamTaskID)
}

type runpodStatusResp struct {
	ID     string          `json:"id"`
	Status json.RawMessage `json:"status"` // usually a string (IN_QUEUE/IN_PROGRESS/COMPLETED/FAILED/CANCELLED/TIMED_OUT) but RunPod's error responses sometimes set it to an HTTP status int
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// statusString tolerates both `"COMPLETED"` and `404` shapes for the status
// field. Anything we can't parse as a known string is treated as an error
// from upstream and surfaced as TaskStatusFailure by the switch.
func (r runpodStatusResp) statusString() string {
	if len(r.Status) == 0 {
		return ""
	}
	var s string
	if err := common.Unmarshal(r.Status, &s); err == nil {
		return s
	}
	return ""
}

func (r *RunPod) ParseStatusResponse(body []byte) (*relaycommon.TaskInfo, error) {
	var resp runpodStatusResp
	if err := common.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("comfyui/runpod: parse status: %w", err)
	}
	info := &relaycommon.TaskInfo{}
	switch strings.ToUpper(resp.statusString()) {
	case "IN_QUEUE":
		info.Status = model.TaskStatusQueued
	case "IN_PROGRESS":
		info.Status = model.TaskStatusInProgress
	case "COMPLETED":
		info.Status = model.TaskStatusSuccess
		if urls := allRunPodImages(resp.Output); len(urls) > 0 {
			info.Urls = urls
		}
	case "FAILED", "CANCELLED", "TIMED_OUT":
		info.Status = model.TaskStatusFailure
		info.Reason = resp.Error
	default:
		info.Status = model.TaskStatusInProgress
	}
	return info, nil
}

// allRunPodImages walks the runpod-workers/worker-comfyui output shape and
// returns every image as a URL or data:image/png;base64,... URI. The worker
// emits either {"images":[{"url": "..."}]} (when S3 output is configured) or
// {"images":[{"data": "<base64>"}]} (default). Batch_size>1 in the workflow
// produces N entries here; callers that only want one image read [0].
func allRunPodImages(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := common.Unmarshal(raw, &out); err != nil {
		return nil
	}
	imgs, ok := out["images"].([]any)
	if !ok {
		return nil
	}
	urls := make([]string, 0, len(imgs))
	for _, it := range imgs {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if u, _ := m["url"].(string); u != "" {
			urls = append(urls, u)
			continue
		}
		if d, _ := m["data"].(string); d != "" {
			urls = append(urls, "data:image/png;base64,"+d)
		}
	}
	return urls
}
