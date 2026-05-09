package comfyui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/comfyui/strategies"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// batchSizeMax caps the OpenAI-style `n` we will plumb into a workflow's
// batch_size input. Templates can request fewer via their own defaults but
// requests beyond this cap get clamped down so a runaway client can't OOM
// the worker by asking for hundreds of variations in a single call.
const batchSizeMax = 8

// TaskAdaptor speaks the ComfyUI workflow contract on top of any provider that
// runs ComfyUI behind an async API. Provider-specific request envelope and
// status polling live in strategies/.
type TaskAdaptor struct {
	taskcommon.BaseBilling

	channelType int
	apiKey      string
	baseURL     string
	templates   *Templates
	strategy    Strategy

	// Filled during BuildRequestBody and consumed by BuildRequestURL/Header
	// inside the same request lifetime.
	pendingURL     string
	pendingHeaders map[string]string
}

// ============================
// Init / Validate
// ============================

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.channelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	// Reuse ValidateBasicTaskRequest so the request lands in context like every
	// other task adapter. Action is always "generate" for image gen.
	if err := relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate); err != nil {
		return err
	}
	// Resolve templates + strategy now so config errors fail fast at submit time
	// rather than mid-build. The same instances are reused across the request.
	if err := a.loadTemplates(c); err != nil {
		return service.TaskErrorWrapperLocal(err, "comfyui_invalid_templates", http.StatusBadRequest)
	}
	if err := a.resolveStrategy(); err != nil {
		return service.TaskErrorWrapperLocal(err, "comfyui_unknown_provider", http.StatusBadRequest)
	}
	return nil
}

// ============================
// Request build
// ============================

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	// BuildRequestBody runs first inside RelayTaskSubmit and stashes the
	// provider-specific URL on the adaptor instance for this request.
	if a.pendingURL == "" {
		return "", errors.New("comfyui: request url not prepared (BuildRequestBody must run first)")
	}
	return a.pendingURL, nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	for k, v := range a.pendingHeaders {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	taskReq, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, err
	}
	submit, err := decodeSubmit(taskReq)
	if err != nil {
		return nil, err
	}
	tpl, ok := a.templates.Templates[info.UpstreamModelName]
	if !ok {
		return nil, fmt.Errorf("comfyui: no workflow template for model %q (configure on channel)", info.UpstreamModelName)
	}
	patched, extras, err := patchWorkflow(tpl, submit, taskReq.Size)
	if err != nil {
		return nil, err
	}
	url, body, headers, err := a.strategy.BuildSubmitRequest(a.baseURL, patched, extras)
	if err != nil {
		return nil, err
	}
	a.pendingURL = url
	a.pendingHeaders = headers
	return bytes.NewReader(body), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// ============================
// Response handling
// ============================

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}
	if resp.StatusCode >= 400 {
		taskErr = service.TaskErrorWrapperLocal(fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(body, 400)), "upstream_error", resp.StatusCode)
		return
	}
	upstreamID, err := a.strategy.ParseSubmitResponse(body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "parse_submit_failed", http.StatusInternalServerError)
		return
	}
	// Mirror kling: return an OpenAI-shape envelope so client polling has a
	// stable id reference. Image responses currently lack a dedicated DTO so we
	// expose a small generic body keyed off the public task id.
	c.JSON(http.StatusOK, gin.H{
		"id":         info.PublicTaskID,
		"task_id":    info.PublicTaskID,
		"created_at": time.Now().Unix(),
		"model":      info.OriginModelName,
		"status":     "submitted",
	})
	return upstreamID, body, nil
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, errors.New("comfyui: invalid task_id")
	}
	// Polling-time strategy build: only api key + provider/app are required.
	// The poller forwards the channel's `workflow_templates` JSON via the
	// body map (relay/service/task_polling.go) and we extract provider+app
	// from it. Falls back to body["provider"]/["app"] for direct callers
	// that already know them.
	provider, _ := body["provider"].(string)
	app, _ := body["app"].(string)
	if (provider == "" || app == "") && body["workflow_templates"] != nil {
		var raw string
		switch v := body["workflow_templates"].(type) {
		case string:
			raw = v
		case *string:
			if v != nil {
				raw = *v
			}
		}
		if raw != "" {
			var tpl Templates
			if err := common.Unmarshal([]byte(raw), &tpl); err == nil {
				if provider == "" {
					provider = tpl.Provider
				}
				if app == "" {
					app = tpl.App
				}
			}
		}
	}
	strat, err := buildStrategy(provider, key, app)
	if err != nil {
		return nil, err
	}
	url := strat.StatusURL(baseURL, taskID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authHeaderFor(provider, key))
	req.Header.Set("Accept", "application/json")
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(body []byte) (*relaycommon.TaskInfo, error) {
	if a.strategy != nil {
		return a.strategy.ParseStatusResponse(body)
	}
	// Polling loop runs through a fresh adapter without going through Init/Validate;
	// fall back to runpod which is the v1 default.
	fallback := &strategies.RunPod{}
	return fallback.ParseStatusResponse(body)
}

// ============================
// Misc adapter contract
// ============================

func (a *TaskAdaptor) GetModelList() []string {
	if a.templates == nil {
		return nil
	}
	out := make([]string, 0, len(a.templates.Templates))
	for k := range a.templates.Templates {
		out = append(out, k)
	}
	return out
}

func (a *TaskAdaptor) GetChannelName() string { return "comfyui" }

// EstimateBilling intentionally returns nil for ComfyUI templates.
//
// All comfyui models are billed per-call via ModelPrice + quotaType=1
// (see new-api-sync/src/core/sync/pipeline/index.ts). The task billing
// pipeline multiplies OtherRatios into the per-call quota in
// relay_task.go (`info.PriceData.Quota = int(quota * ra)` for every
// non-1.0 ratio). Returning {"steps": 20, "pixels": 1} would multiply
// the per-call price by 20x.
//
// If a future ComfyUI deployment wants ratio-based pricing (model_price
// = 0, model_ratio > 0), branch on info.PriceData.UsePrice and return
// the steps/pixels metrics here. For now every template carries an
// explicit USD price and OtherRatios should stay empty.
func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	// Per-call ComfyUI templates are billed once per submit. With OpenAI-style
	// batch (`metadata.n`) we render N variations from one upstream call, so
	// multiply the per-call price by N. The pipeline at relay_task.go:201
	// folds OtherRatios into PriceData.Quota for non-TaskPricePatches models,
	// so returning {"batch_size": N} charges N x base.
	taskReq, err := relaycommon.GetTaskRequest(c)
	if err != nil || taskReq.Metadata == nil {
		return nil
	}
	n := metadataInt(taskReq.Metadata, "n")
	// metadata.extra.n is the alternate location (lives alongside loras /
	// seed / steps / cfg). Take whichever is larger so a caller can pass it
	// through whichever bucket matches their SDK style.
	if extra, ok := taskReq.Metadata["extra"].(map[string]any); ok {
		if en := metadataInt(extra, "n"); en > n {
			n = en
		}
	}
	if n <= 1 {
		return nil
	}
	if n > batchSizeMax {
		n = batchSizeMax
	}
	return map[string]float64{"batch_size": float64(n)}
}

// ============================
// Helpers — templates + strategy
// ============================

func (a *TaskAdaptor) loadTemplates(c *gin.Context) error {
	raw, _ := c.Get(string(constant.ContextKeyChannelWorkflowTemplates))
	rawStr, _ := raw.(string)
	if strings.TrimSpace(rawStr) == "" {
		return errors.New("channel has no workflow_templates configured")
	}
	tpls := &Templates{}
	if err := common.UnmarshalJsonStr(rawStr, tpls); err != nil {
		return fmt.Errorf("invalid workflow_templates JSON: %w", err)
	}
	if len(tpls.Templates) == 0 {
		return errors.New("workflow_templates.templates is empty")
	}
	a.templates = tpls
	return nil
}

func (a *TaskAdaptor) resolveStrategy() error {
	provider := "runpod"
	app := ""
	if a.templates != nil {
		if strings.TrimSpace(a.templates.Provider) != "" {
			provider = a.templates.Provider
		}
		app = a.templates.App
	}
	s, err := buildStrategy(provider, a.apiKey, app)
	if err != nil {
		return err
	}
	a.strategy = s
	return nil
}

// buildStrategy is the only place new providers get registered. Adding a new
// strategy means importing it here and wiring its case.
func buildStrategy(provider, apiKey, app string) (Strategy, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "runpod":
		return &strategies.RunPod{APIKey: apiKey, Endpoint: app}, nil
	}
	return nil, fmt.Errorf("comfyui: unknown provider %q", provider)
}

func authHeaderFor(provider, key string) string {
	return "Bearer " + key
}

// ============================
// Helpers — workflow patching
// ============================

func decodeSubmit(req relaycommon.TaskSubmitReq) (*SubmitRequest, error) {
	out := &SubmitRequest{
		Prompt: req.Prompt,
		Size:   req.Size,
		N:      1,
	}
	if req.Metadata != nil {
		// negative_prompt, n, and extra block all come through metadata since
		// TaskSubmitReq's static schema is video-shaped (no `n` field).
		if v, ok := req.Metadata["negative_prompt"].(string); ok {
			out.NegativePrompt = v
		}
		if n := metadataInt(req.Metadata, "n"); n > 0 {
			out.N = n
		}
		if extra, ok := req.Metadata["extra"]; ok {
			b, err := common.Marshal(extra)
			if err == nil {
				_ = common.Unmarshal(b, &out.Extra)
			}
		}
		// metadata.extra.n is the alternate location for `n`; let it win when
		// it's larger so a caller passing it alongside loras/seed/steps in the
		// extra block isn't silently overridden by an absent metadata.n.
		if out.Extra != nil && out.Extra.N != nil && *out.Extra.N > out.N {
			out.N = *out.Extra.N
		}
	}
	return out, nil
}

func patchWorkflow(tpl Template, submit *SubmitRequest, size string) ([]byte, *strategies.SubmitExtras, error) {
	var graph map[string]json.RawMessage
	if err := common.Unmarshal(tpl.Workflow, &graph); err != nil {
		return nil, nil, fmt.Errorf("comfyui: parse template workflow: %w", err)
	}
	extras := &strategies.SubmitExtras{}

	setNodeInput := func(nodeID, key string, value any) error {
		nodeRaw, ok := graph[nodeID]
		if !ok {
			return fmt.Errorf("template references missing node %q", nodeID)
		}
		var node map[string]any
		if err := common.Unmarshal(nodeRaw, &node); err != nil {
			return err
		}
		inputs, _ := node["inputs"].(map[string]any)
		if inputs == nil {
			inputs = map[string]any{}
		}
		inputs[key] = value
		node["inputs"] = inputs
		nb, err := common.Marshal(node)
		if err != nil {
			return err
		}
		graph[nodeID] = nb
		return nil
	}

	// Inject named params from the request. Parse → mutate → re-marshal.
	for name, p := range tpl.Params {
		var value any
		switch name {
		case "prompt":
			// Prepend `(embedding:<filename>:<weight>) ` tokens before
			// patching CLIPTextEncode. The filename MUST include the file
			// extension because the ComfyUI tokenizer errors on
			// `(embedding:Name:1.2)` but accepts
			// `(embedding:Name.safetensors:1.2)`. Empty / zero-weight
			// entries are skipped.
			value = applyEmbeddings(submit.Prompt, submit.Extra)
		case "negative_prompt_with_embeddings":
			// Escape hatch for templates wanting embeddings injected into
			// the NEGATIVE prompt (e.g. EasyNegative). Same rewriter as
			// the positive case.
			value = applyEmbeddings(submit.NegativePrompt, submit.Extra)
		case "negative_prompt":
			value = submit.NegativePrompt
		case "seed":
			if submit.Extra != nil && submit.Extra.Seed != nil {
				value = *submit.Extra.Seed
			} else if p.AutoRandom {
				value = randomSeed()
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "steps":
			if submit.Extra != nil && submit.Extra.Steps != nil {
				value = *submit.Extra.Steps
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "cfg":
			if submit.Extra != nil && submit.Extra.CFG != nil {
				value = *submit.Extra.CFG
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "width":
			w, _ := parseSize(size)
			if w > 0 {
				value = w
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "height":
			_, h := parseSize(size)
			if h > 0 {
				value = h
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "hires_denoise":
			if submit.Extra != nil && submit.Extra.Hires != nil && submit.Extra.Hires.Denoise != nil {
				value = *submit.Extra.Hires.Denoise
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "hires_upscale":
			if submit.Extra != nil && submit.Extra.Hires != nil && submit.Extra.Hires.UpscaleBy != nil {
				value = *submit.Extra.Hires.UpscaleBy
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "sampler":
			if submit.Extra != nil && submit.Extra.Sampler != nil {
				value = *submit.Extra.Sampler
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "scheduler":
			if submit.Extra != nil && submit.Extra.Scheduler != nil {
				value = *submit.Extra.Scheduler
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "denoise":
			if submit.Extra != nil && submit.Extra.Denoise != nil {
				value = *submit.Extra.Denoise
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "batch_size":
			// OpenAI-style `n` from the top-level request maps to a workflow
			// node's `batch_size` input (typically EmptyLatentImage or
			// EmptyFlux2LatentImage). Templates opt-in by declaring this
			// param. Capped at batchSizeMax to avoid wild requests OOMing
			// the worker.
			n := submit.N
			if n <= 0 {
				if p.Default != nil {
					value = p.Default
				} else {
					value = 1
				}
			} else {
				if n > batchSizeMax {
					n = batchSizeMax
				}
				value = n
			}
		case "init_image_url":
			// Studio sub-mode: Img2Img / Upscale / ADetailer / Inpaint feed
			// the user's source image into a LoadImage node. The strategy
			// rehosts the URL into extras.Images so the worker can read it
			// by filename — same pipeline references use.
			if submit.Extra != nil && submit.Extra.InitImageURL != nil && *submit.Extra.InitImageURL != "" {
				filename, err := registerExtraImage(extras, *submit.Extra.InitImageURL, "init")
				if err != nil {
					return nil, nil, err
				}
				value = filename
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "mask_url":
			// Inpaint sub-pill brush mask. Same pre-upload as init_image_url.
			if submit.Extra != nil && submit.Extra.MaskURL != nil && *submit.Extra.MaskURL != "" {
				filename, err := registerExtraImage(extras, *submit.Extra.MaskURL, "mask")
				if err != nil {
					return nil, nil, err
				}
				value = filename
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "upscaler":
			if submit.Extra != nil && submit.Extra.Upscaler != nil {
				value = *submit.Extra.Upscaler
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "upscaler_scale_by":
			// Pre-computed by the caller: desired_multiplier / native_scale.
			// Falls back to template default (typically 0.25 = 1x final for
			// a 4x ESRGAN model).
			if submit.Extra != nil && submit.Extra.UpscalerScaleBy != nil {
				value = *submit.Extra.UpscalerScaleBy
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "upscaler_multiplier":
			// Observability only — adapter doesn't use this directly since
			// upscaler_scale_by carries the pre-computed value. Templates
			// that want to log the multiplier can wire this to a metadata
			// node. Most templates skip it.
			if submit.Extra != nil && submit.Extra.UpscalerMultiplier != nil {
				value = *submit.Extra.UpscalerMultiplier
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "hires_steps":
			if submit.Extra != nil && submit.Extra.HiresSteps != nil {
				value = *submit.Extra.HiresSteps
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "control_net_model":
			// kind -> filename map. Templates carry a default placeholder
			// (typically the depth file); the adapter picks the right one
			// when ControlNet is active.
			if submit.Extra != nil && submit.Extra.ControlNet != nil {
				if fn := controlNetFilenameForKind(submit.Extra.ControlNet.Kind); fn != "" {
					value = fn
				} else if p.Default != nil {
					value = p.Default
				} else {
					continue
				}
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "control_net_strength":
			// Default 0 (no-op) when ControlNet absent. ControlNetApplyAdvanced
			// passes the conditioning through unchanged at strength 0.
			if submit.Extra != nil && submit.Extra.ControlNet != nil {
				value = submit.Extra.ControlNet.Weight
			} else if p.Default != nil {
				value = p.Default
			} else {
				value = 0.0
			}
		case "control_net_image":
			// Rehost the ControlNet reference image into extras.Images so
			// the worker can read it by filename — same pattern as init/
			// mask/references. Skips upload when ControlNet absent.
			if submit.Extra != nil && submit.Extra.ControlNet != nil && submit.Extra.ControlNet.ImageURL != "" {
				filename, err := registerExtraImage(extras, submit.Extra.ControlNet.ImageURL, "controlnet")
				if err != nil {
					return nil, nil, err
				}
				value = filename
			} else if p.Default != nil {
				value = p.Default
			} else {
				// No image uploaded; emit an empty filename so the LoadImage
				// node fails fast rather than silently using a stale path.
				value = ""
			}
		case "layer_diffusion_weight":
			// Templates carry LayeredDiffusionApply with default weight 0
			// (no-op). Non-zero weight patches the model so the sampler
			// produces transparent-PNG latents — paired template MUST
			// route SaveImage through LayeredDiffusionDecodeRGBA when this
			// is non-zero, else the decoded image is garbage.
			if submit.Extra != nil && submit.Extra.LayerDiffusion != nil {
				value = submit.Extra.LayerDiffusion.Weight
			} else if p.Default != nil {
				value = p.Default
			} else {
				value = 0.0
			}
		case "clip_skip":
			// ComfyUI CLIPSetLastLayer.stop_at_clip_layer convention: -1 =
			// no skip (default), -2 = skip last 1 layer, etc. Form sends
			// positive values, adapter negates.
			if submit.Extra != nil && submit.Extra.ClipSkip != nil {
				skip := *submit.Extra.ClipSkip
				if skip <= 0 {
					skip = 1
				}
				value = -skip
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "ensd":
			// Eta Noise Seed Delta. Inspire KSampler maps this via
			// variation_seed = seed + ensd. Forward the raw integer to
			// whichever template node declares the `ensd` param.
			if submit.Extra != nil && submit.Extra.ENSD != nil {
				value = *submit.Extra.ENSD
			} else if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		case "adetailer_yolo_model", "adetailer_prompt", "adetailer_negative_prompt",
			"adetailer_steps", "adetailer_confidence", "adetailer_mask_blur",
			"adetailer_denoise", "adetailer_inpaint_only_masked":
			// Flatten the nested submit.Extra.Adetailer pointer into the
			// matching scalar template input. Falls back to template
			// default when absent.
			value = adetailerParamValue(submit, name, p)
			if value == nil {
				continue
			}
		default:
			// Unknown param name in template — fall back to default if present.
			if p.Default != nil {
				value = p.Default
			} else {
				continue
			}
		}
		// Multi-node fanout: width/height on Flux 2 must hit both
		// EmptyFlux2LatentImage and Flux2Scheduler simultaneously, so the
		// template lists every target node and we write the same value to
		// each. Falls back to the single-node path when Nodes is empty.
		targets := p.Nodes
		if len(targets) == 0 {
			targets = []string{p.Node}
		}
		for _, nodeID := range targets {
			if err := setNodeInput(nodeID, p.Input, value); err != nil {
				return nil, nil, err
			}
		}
	}

	// LoRA chain: clone the LoraLoader template node once per requested LoRA,
	// chain model+clip outputs head-to-tail, rewrite downstream references to
	// the original loader's outputs to point at the last clone. With zero
	// LoRAs requested, drop the loader entirely and pass through its upstream
	// model/clip refs.
	if tpl.LoraChain != nil {
		if err := applyLoraChain(graph, tpl.LoraChain, loraList(submit)); err != nil {
			return nil, nil, err
		}
	}

	// Reference chain: clone the LoadImage / scale / encode / ref-latent
	// template group once per requested reference. References URLs are
	// fetched server-side, base64-encoded, and registered as extras.Images
	// (the strategy lifts these into worker-comfyui's input.images so
	// LoadImage nodes can read them by filename).
	if tpl.References != nil {
		if err := applyReferenceChain(graph, tpl.References, referenceList(submit), extras); err != nil {
			return nil, nil, err
		}
	}

	// Re-marshal the graph back to a single JSON object that the strategy will
	// embed in its provider envelope.
	out, err := common.Marshal(graph)
	if err != nil {
		return nil, nil, err
	}
	return out, extras, nil
}

// loraList returns the user's requested LoRAs (possibly empty).
func loraList(submit *SubmitRequest) []LoraSpec {
	if submit == nil || submit.Extra == nil {
		return nil
	}
	return submit.Extra.Loras
}

// referenceList returns the user's requested reference images (possibly empty).
func referenceList(submit *SubmitRequest) []ReferenceSpec {
	if submit == nil || submit.Extra == nil {
		return nil
	}
	return submit.Extra.References
}

// applyLoraChain rewrites graph in-place to thread N LoraLoader clones into
// the position of the template loader node. See LoraChainSpec doc for shape.
func applyLoraChain(
	graph map[string]json.RawMessage,
	chain *LoraChainSpec,
	loras []LoraSpec,
) error {
	loaderID := chain.Node
	loaderRaw, ok := graph[loaderID]
	if !ok {
		return fmt.Errorf("comfyui: lora_chain references missing node %q", loaderID)
	}
	var loaderTemplate map[string]any
	if err := common.Unmarshal(loaderRaw, &loaderTemplate); err != nil {
		return fmt.Errorf("comfyui: parse loader node %q: %w", loaderID, err)
	}
	loaderInputs, _ := loaderTemplate["inputs"].(map[string]any)
	if loaderInputs == nil {
		loaderInputs = map[string]any{}
	}

	modelInputKey := chain.ModelInput
	if modelInputKey == "" {
		modelInputKey = "model"
	}
	clipInputKey := chain.ClipInput
	if clipInputKey == "" {
		clipInputKey = "clip"
	}
	modelOutIdx := 0
	if chain.ModelOutputIndex != nil {
		modelOutIdx = *chain.ModelOutputIndex
	}
	clipOutIdx := 1
	if chain.ClipOutputIndex != nil {
		clipOutIdx = *chain.ClipOutputIndex
	}

	upstreamModel := loaderInputs[modelInputKey]
	upstreamClip := loaderInputs[clipInputKey]

	// Decide the new node id every reference to (loaderID, modelOutIdx) and
	// (loaderID, clipOutIdx) should rewrite to. With zero LoRAs we collapse
	// the chain — model rewrites to the loader's upstream model ref, clip
	// rewrites to the loader's upstream clip ref. With one or more LoRAs we
	// rewrite to the tail clone's outputs.
	var modelTail any
	var clipTail any
	if len(loras) == 0 {
		modelTail = upstreamModel
		clipTail = upstreamClip
		// Drop the original loader; nothing else references it once
		// downstream rewriting is done.
		delete(graph, loaderID)
	} else {
		// Build clones loaderID__0 .. loaderID__{N-1}, chained.
		var prevID string
		for i, lora := range loras {
			cloneID := fmt.Sprintf("%s__%d", loaderID, i)
			clone, err := deepCopyNode(loaderTemplate)
			if err != nil {
				return fmt.Errorf("comfyui: clone loader node %q: %w", loaderID, err)
			}
			cloneInputs, _ := clone["inputs"].(map[string]any)
			if cloneInputs == nil {
				cloneInputs = map[string]any{}
			}
			// Wire model+clip: first clone takes the loader's original
			// upstream refs; subsequent clones take the prior clone's
			// outputs.
			if i == 0 {
				cloneInputs[modelInputKey] = upstreamModel
				cloneInputs[clipInputKey] = upstreamClip
			} else {
				cloneInputs[modelInputKey] = []any{prevID, modelOutIdx}
				cloneInputs[clipInputKey] = []any{prevID, clipOutIdx}
			}
			// Patch the lora identifier (URL preferred when available).
			nameOrURL := lora.URL
			if nameOrURL == "" {
				nameOrURL = lora.Name
			}
			if nameOrURL != "" {
				idInput := chain.Input
				if chain.URLInput != "" && lora.URL != "" {
					idInput = chain.URLInput
				}
				cloneInputs[idInput] = nameOrURL
			}
			// Patch weight knobs. ClipWeightInput defaults to WeightInput.
			weight := lora.Weight
			if weight == 0 {
				weight = 1.0
			}
			if chain.WeightInput != "" {
				cloneInputs[chain.WeightInput] = weight
			}
			clipWeightInput := chain.ClipWeightInput
			if clipWeightInput == "" {
				clipWeightInput = chain.WeightInput
			}
			if clipWeightInput != "" {
				cloneInputs[clipWeightInput] = weight
			}
			clone["inputs"] = cloneInputs
			cloneRaw, err := common.Marshal(clone)
			if err != nil {
				return err
			}
			graph[cloneID] = cloneRaw
			prevID = cloneID
		}
		modelTail = []any{prevID, modelOutIdx}
		clipTail = []any{prevID, clipOutIdx}
		// Remove the original template loader; replaced by clones.
		delete(graph, loaderID)
	}

	// Rewrite every input across the graph that referenced
	// (loaderID, modelOutIdx) -> modelTail and (loaderID, clipOutIdx) -> clipTail.
	for nodeID, raw := range graph {
		var node map[string]any
		if err := common.Unmarshal(raw, &node); err != nil {
			return err
		}
		inputs, _ := node["inputs"].(map[string]any)
		if inputs == nil {
			continue
		}
		mutated := false
		for k, v := range inputs {
			arr, ok := v.([]any)
			if !ok || len(arr) != 2 {
				continue
			}
			refID, _ := arr[0].(string)
			refIdx := toInt(arr[1])
			if refID != loaderID {
				continue
			}
			if refIdx == modelOutIdx {
				inputs[k] = modelTail
				mutated = true
			} else if refIdx == clipOutIdx {
				inputs[k] = clipTail
				mutated = true
			}
		}
		if mutated {
			node["inputs"] = inputs
			rebuilt, err := common.Marshal(node)
			if err != nil {
				return err
			}
			graph[nodeID] = rebuilt
		}
	}
	return nil
}

// applyReferenceChain rewrites graph in-place to thread N reference-image
// quadruples (LoadImage -> ImageScaleToTotalPixels -> VAEEncode ->
// ReferenceLatent) into the position of the template's placeholder
// quadruple, chaining each ReferenceLatent.conditioning head-to-tail. The
// downstream consumer (e.g. BasicGuider.conditioning) is rewired from the
// original ref node to the tail clone. See ReferenceChainSpec doc for the
// expected template layout.
func applyReferenceChain(
	graph map[string]json.RawMessage,
	chain *ReferenceChainSpec,
	refs []ReferenceSpec,
	extras *strategies.SubmitExtras,
) error {
	loaderID := chain.LoaderNode
	scaleID := chain.ScaleNode
	encodeID := chain.EncodeNode
	refID := chain.RefNode

	for _, id := range []string{loaderID, scaleID, encodeID, refID} {
		if _, ok := graph[id]; !ok {
			return fmt.Errorf("comfyui: references chain missing node %q", id)
		}
	}

	loaderInput := chain.LoaderInput
	if loaderInput == "" {
		loaderInput = "image"
	}
	condInput := chain.RefConditioningInput
	if condInput == "" {
		condInput = "conditioning"
	}
	latentInput := chain.RefLatentInput
	if latentInput == "" {
		latentInput = "latent"
	}

	if chain.MaxReferences > 0 && len(refs) > chain.MaxReferences {
		refs = refs[:chain.MaxReferences]
	}

	// Parse the four template nodes once.
	var loaderTpl, scaleTpl, encodeTpl, refTpl map[string]any
	for id, dst := range map[string]*map[string]any{
		loaderID: &loaderTpl,
		scaleID:  &scaleTpl,
		encodeID: &encodeTpl,
		refID:    &refTpl,
	} {
		if err := common.Unmarshal(graph[id], dst); err != nil {
			return fmt.Errorf("comfyui: parse references node %q: %w", id, err)
		}
	}

	scaleTplInputs, _ := scaleTpl["inputs"].(map[string]any)
	encodeTplInputs, _ := encodeTpl["inputs"].(map[string]any)
	refTplInputs, _ := refTpl["inputs"].(map[string]any)

	// Snapshot the original RefNode's upstream conditioning ref. With 0
	// references we use this to rewire the consumer; with N>=1 references
	// the head clone's `conditioning` input takes this same ref.
	upstreamCond, hasUpstreamCond := refTplInputs[condInput]

	// With zero references: rewire every reference to (refID, 0) across the
	// graph to point at upstreamCond instead, then delete the four
	// placeholder nodes.
	if len(refs) == 0 {
		if !hasUpstreamCond {
			return fmt.Errorf("comfyui: references zero case needs %q upstream on %q", condInput, refID)
		}
		if err := rewriteRefsTo(graph, refID, 0, upstreamCond); err != nil {
			return err
		}
		delete(graph, loaderID)
		delete(graph, scaleID)
		delete(graph, encodeID)
		delete(graph, refID)
		return nil
	}

	// With N >= 1 references: clone each placeholder per reference, chain
	// conditioning head-to-tail, rewire downstream consumers to the tail.
	var prevRefCloneID string
	for i, ref := range refs {
		loadCloneID := fmt.Sprintf("%s__%d", loaderID, i)
		scaleCloneID := fmt.Sprintf("%s__%d", scaleID, i)
		encodeCloneID := fmt.Sprintf("%s__%d", encodeID, i)
		refCloneID := fmt.Sprintf("%s__%d", refID, i)

		// LoadImage clone: fetch the reference URL server-side, base64-
		// encode it, register as an entry in extras.Images so the strategy
		// pre-uploads it to ComfyUI's input/ directory under the chosen
		// filename. The LoadImage node's `image` input then receives the
		// filename (NOT the URL — worker-comfyui's LoadImage validates
		// against local files, not URLs).
		filename, b64, ferr := fetchAndEncodeReference(ref.URL, i)
		if ferr != nil {
			return fmt.Errorf("comfyui: fetch reference %d (%s): %w", i, ref.URL, ferr)
		}
		extras.Images = append(extras.Images, map[string]any{
			"name":  filename,
			"image": b64,
		})

		loadClone, err := deepCopyNode(loaderTpl)
		if err != nil {
			return fmt.Errorf("comfyui: clone loader %q: %w", loaderID, err)
		}
		loadInputs, _ := loadClone["inputs"].(map[string]any)
		if loadInputs == nil {
			loadInputs = map[string]any{}
		}
		loadInputs[loaderInput] = filename
		loadClone["inputs"] = loadInputs

		// ImageScaleToTotalPixels clone: rewire image -> load clone.
		scaleClone, err := deepCopyNode(scaleTpl)
		if err != nil {
			return fmt.Errorf("comfyui: clone scale %q: %w", scaleID, err)
		}
		scaleCloneInputs, _ := scaleClone["inputs"].(map[string]any)
		if scaleCloneInputs == nil {
			scaleCloneInputs = map[string]any{}
		}
		rewireUpstream(scaleCloneInputs, scaleTplInputs, loaderID, []any{loadCloneID, 0}, "image")
		scaleClone["inputs"] = scaleCloneInputs

		// VAEEncode clone: rewire pixels -> scale clone, preserve vae ref.
		encodeClone, err := deepCopyNode(encodeTpl)
		if err != nil {
			return fmt.Errorf("comfyui: clone encode %q: %w", encodeID, err)
		}
		encodeCloneInputs, _ := encodeClone["inputs"].(map[string]any)
		if encodeCloneInputs == nil {
			encodeCloneInputs = map[string]any{}
		}
		rewireUpstream(encodeCloneInputs, encodeTplInputs, scaleID, []any{scaleCloneID, 0}, "pixels")
		encodeClone["inputs"] = encodeCloneInputs

		// ReferenceLatent clone: rewire latent -> encode clone, chain
		// conditioning head-to-tail (head = upstream, rest = previous clone).
		refClone, err := deepCopyNode(refTpl)
		if err != nil {
			return fmt.Errorf("comfyui: clone ref-latent %q: %w", refID, err)
		}
		refCloneInputs, _ := refClone["inputs"].(map[string]any)
		if refCloneInputs == nil {
			refCloneInputs = map[string]any{}
		}
		refCloneInputs[latentInput] = []any{encodeCloneID, 0}
		if i == 0 {
			if hasUpstreamCond {
				refCloneInputs[condInput] = upstreamCond
			}
		} else {
			refCloneInputs[condInput] = []any{prevRefCloneID, 0}
		}
		if chain.WeightInput != "" {
			weight := ref.Weight
			if weight == 0 {
				weight = 1.0
			}
			refCloneInputs[chain.WeightInput] = weight
		}
		refClone["inputs"] = refCloneInputs

		// Persist the four clones.
		for id, node := range map[string]map[string]any{
			loadCloneID:   loadClone,
			scaleCloneID:  scaleClone,
			encodeCloneID: encodeClone,
			refCloneID:    refClone,
		} {
			b, err := common.Marshal(node)
			if err != nil {
				return err
			}
			graph[id] = b
		}

		prevRefCloneID = refCloneID
	}

	// Rewire every downstream input that pointed at the original (refID, 0)
	// to point at the tail clone's output instead.
	if err := rewriteRefsTo(graph, refID, 0, []any{prevRefCloneID, 0}); err != nil {
		return err
	}

	// Drop the placeholder quadruple.
	delete(graph, loaderID)
	delete(graph, scaleID)
	delete(graph, encodeID)
	delete(graph, refID)

	return nil
}

// fetchAndEncodeReference does an HTTPS GET on the reference URL, validates
// the response shape (200, image/* Content-Type, body under the size cap),
// and returns the filename it should be uploaded under (used in
// extras.Images and as the LoadImage.image input value) plus the base64
// payload. The size cap is intentionally tight: worker-comfyui's /run
// endpoint accepts payloads up to ~10MB, and a typical compose request
// chains 4-6 references plus the workflow JSON, so each reference must
// stay well under that.
func fetchAndEncodeReference(url string, idx int) (filename string, b64Data string, err error) {
	if strings.TrimSpace(url) == "" {
		return "", "", errors.New("empty reference url")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "new-api-comfyui-adaptor")
	req.Header.Set("Accept", "image/*")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	const maxBytes = 1<<20 + 1<<19 // 1.5 MB per reference; 6 refs * 1.5 = 9 MB room under the 10 MB cap
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", "", err
	}
	if len(body) >= maxBytes {
		return "", "", fmt.Errorf("reference exceeds %d byte cap", maxBytes-1)
	}
	contentType := resp.Header.Get("Content-Type")
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if !strings.HasPrefix(contentType, "image/") {
		return "", "", fmt.Errorf("non-image content-type %q", contentType)
	}
	ext := "png"
	switch contentType {
	case "image/jpeg", "image/jpg":
		ext = "jpg"
	case "image/webp":
		ext = "webp"
	case "image/gif":
		ext = "gif"
	case "image/png":
		ext = "png"
	}
	return fmt.Sprintf("ref_%d.%s", idx, ext), base64.StdEncoding.EncodeToString(body), nil
}

// registerExtraImage fetches a single URL, base64-encodes the bytes, and
// registers it in extras.Images under a unique filename (prefix + extension
// from the response Content-Type). Returns the filename that the caller
// should write into the corresponding LoadImage node's `image` input. Used
// by the init_image_url / mask_url / control_net image flows — same
// pipeline as the reference chain but for single-image params.
func registerExtraImage(extras *strategies.SubmitExtras, url, prefix string) (string, error) {
	if extras == nil {
		return "", errors.New("nil extras")
	}
	if strings.TrimSpace(url) == "" {
		return "", errors.New("empty image url")
	}
	idx := len(extras.Images)
	filename, b64, err := fetchAndEncodeReference(url, idx)
	if err != nil {
		return "", err
	}
	// Prefix the auto-generated `ref_N.ext` with a semantic tag so the
	// worker logs distinguish init / mask / control_net images.
	if prefix != "" {
		filename = prefix + "_" + filename
	}
	extras.Images = append(extras.Images, map[string]any{
		"name":  filename,
		"image": b64,
	})
	return filename, nil
}

// applyEmbeddings prepends `(embedding:<filename>:<weight>) ` tokens for
// every embedding spec on submit.Extra to the given base prompt. Returns
// the rewritten prompt. Skips entries with empty filename or zero weight.
// The filename MUST include the file extension because the ComfyUI
// tokenizer rejects weighted bare-name embeddings.
func applyEmbeddings(basePrompt string, extra *SubmitExtra) string {
	if extra == nil || len(extra.Embeddings) == 0 {
		return basePrompt
	}
	var b strings.Builder
	for _, e := range extra.Embeddings {
		if strings.TrimSpace(e.Filename) == "" || e.Weight == 0 {
			continue
		}
		fmt.Fprintf(&b, "(embedding:%s:%.2f) ", e.Filename, e.Weight)
	}
	b.WriteString(basePrompt)
	return b.String()
}

// controlNetFilenameForKind maps a UI kind ("depth" / "canny" / "openpose")
// to the on-volume SDXL ControlNet checkpoint filename. Falls back to the
// empty string for unknown kinds; the switch case falls through to the
// template default in that path.
func controlNetFilenameForKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "depth":
		return "control-depth-sdxl.safetensors"
	case "canny":
		return "control-canny-sdxl.safetensors"
	case "openpose":
		return "control-openpose-sdxl.safetensors"
	}
	return ""
}

// adetailerParamValue looks up the right scalar from submit.Extra.Adetailer
// for one of the flat `adetailer_*` template param names. Returns nil to
// signal "no value submitted, no template default — skip this param".
func adetailerParamValue(submit *SubmitRequest, name string, p Param) any {
	a := submit.Extra
	var v any
	if a != nil && a.Adetailer != nil {
		switch name {
		case "adetailer_yolo_model":
			if a.Adetailer.YoloModel != nil {
				v = *a.Adetailer.YoloModel
			}
		case "adetailer_prompt":
			if a.Adetailer.Prompt != nil {
				v = *a.Adetailer.Prompt
			}
		case "adetailer_negative_prompt":
			if a.Adetailer.NegativePrompt != nil {
				v = *a.Adetailer.NegativePrompt
			}
		case "adetailer_steps":
			if a.Adetailer.Steps != nil {
				v = *a.Adetailer.Steps
			}
		case "adetailer_confidence":
			if a.Adetailer.Confidence != nil {
				v = *a.Adetailer.Confidence
			}
		case "adetailer_mask_blur":
			if a.Adetailer.MaskBlur != nil {
				v = *a.Adetailer.MaskBlur
			}
		case "adetailer_denoise":
			if a.Adetailer.Denoise != nil {
				v = *a.Adetailer.Denoise
			}
		case "adetailer_inpaint_only_masked":
			if a.Adetailer.InpaintOnlyMasked != nil {
				v = *a.Adetailer.InpaintOnlyMasked
			}
		}
	}
	if v != nil {
		return v
	}
	if p.Default != nil {
		return p.Default
	}
	return nil
}

// rewriteRefsTo rewrites every input across `graph` that references
// (oldNodeID, oldOutputIdx) to `newRef`. Used by applyReferenceChain to
// re-point the consumer (BasicGuider.conditioning) from the placeholder
// ReferenceLatent to either the tail clone (N>=1 refs) or the upstream
// conditioning ref (N=0 refs).
func rewriteRefsTo(graph map[string]json.RawMessage, oldNodeID string, oldOutputIdx int, newRef any) error {
	for nodeID, raw := range graph {
		if nodeID == oldNodeID {
			continue
		}
		var node map[string]any
		if err := common.Unmarshal(raw, &node); err != nil {
			return err
		}
		inputs, _ := node["inputs"].(map[string]any)
		if inputs == nil {
			continue
		}
		mutated := false
		for k, v := range inputs {
			arr, ok := v.([]any)
			if !ok || len(arr) != 2 {
				continue
			}
			refID, _ := arr[0].(string)
			refIdx := toInt(arr[1])
			if refID == oldNodeID && refIdx == oldOutputIdx {
				inputs[k] = newRef
				mutated = true
			}
		}
		if mutated {
			node["inputs"] = inputs
			rebuilt, err := common.Marshal(node)
			if err != nil {
				return err
			}
			graph[nodeID] = rebuilt
		}
	}
	return nil
}

// rewireUpstream finds the first input in srcInputs that references upstreamID
// (any output index) and writes the new ref under the same key into dstInputs.
// If no such input exists, it falls back to writing under fallbackKey. Used by
// applyReferenceChain to preserve template-author key choice without forcing a
// hardcoded "image"/"pixels"/"latent" naming convention.
func rewireUpstream(dstInputs, srcInputs map[string]any, upstreamID string, newRef any, fallbackKey string) {
	for k, v := range srcInputs {
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			continue
		}
		refID, _ := arr[0].(string)
		if refID == upstreamID {
			dstInputs[k] = newRef
			return
		}
	}
	dstInputs[fallbackKey] = newRef
}

func deepCopyNode(node map[string]any) (map[string]any, error) {
	raw, err := common.Marshal(node)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := common.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// toInt accepts JSON-decoded numeric values which may be float64 (default)
// or int. Returns -1 on type mismatch so callers don't accidentally match
// real index 0.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return -1
}

// metadataInt pulls an integer out of the loosely-typed metadata map. JSON
// numbers decode to float64 by default, but tolerate int / json.Number /
// string-with-digits too so client SDKs can hand us whatever shape they
// happen to encode `n` as. Returns 0 when missing or unparseable.
func metadataInt(meta map[string]any, key string) int {
	v, ok := meta[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

func randomSeed() int64 {
	// 53-bit positive integer fits in JSON Number safely.
	return rand.Int63n(1<<53 - 1)
}

func parseSize(size string) (int, int) {
	if size == "" {
		return 0, 0
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return 0, 0
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0
	}
	return w, h
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

