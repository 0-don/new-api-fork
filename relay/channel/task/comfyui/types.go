package comfyui

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/relay/channel/task/comfyui/strategies"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// Templates is the per-channel JSON blob keyed by model name.
// Stored on Channel.WorkflowTemplates as raw JSON.
type Templates struct {
	Provider  string              `json:"provider"`
	App       string              `json:"app,omitempty"` // provider-specific endpoint id (RunPod serverless endpoint id, fal app slug, etc.)
	Templates map[string]Template `json:"templates"`
}

// Template binds a ComfyUI API-format workflow to a small parameter schema.
type Template struct {
	Version     string              `json:"version,omitempty"`
	Description string              `json:"description,omitempty"`
	Workflow    json.RawMessage     `json:"workflow"`
	Params      map[string]Param    `json:"params,omitempty"`
	LoraChain   *LoraChainSpec      `json:"lora_chain,omitempty"`
	References  *ReferenceChainSpec `json:"references,omitempty"`
}

// Param maps a logical request field (prompt, negative_prompt, steps, ...)
// to a node + input-key in the workflow JSON.
//
// Nodes is the multi-node variant: when non-empty, the value is written to
// every listed node under the same Input key (Node is ignored). Used for
// width / height on Flux 2 where the value must hit BOTH
// EmptyFlux2LatentImage AND Flux2Scheduler in sync, otherwise the two
// nodes disagree and the run fails or produces a stretched image.
type Param struct {
	Node       string   `json:"node"`
	Nodes      []string `json:"nodes,omitempty"`
	Input      string   `json:"input"`
	Default    any      `json:"default,omitempty"`
	AutoRandom bool     `json:"auto_random,omitempty"`
}

// LoraChainSpec describes how to inject one or more LoRAs into a chained
// LoraLoader sub-graph. The template defines a single LoraLoader node; the
// adapter clones it once per requested LoRA, chains the model+clip outputs
// head-to-tail, and rewrites every downstream reference to the original
// loader's outputs so they hit the last clone instead.
//
// Example workflow snippet (template):
//   "10": { class_type: LoraLoader, inputs: {
//     model: ["base_model_node", 0],
//     clip:  ["base_clip_node", 1],
//     lora_name: "", strength_model: 1.0, strength_clip: 1.0,
//   }}
//   "3":  { class_type: KSampler, inputs: { model: ["10", 0], ... }}
//
// With LoraChainSpec.Node = "10":
// - 0 LoRAs requested: the loader is removed; KSampler.model rewires to
//   the original ["base_model_node", 0].
// - N >= 1 LoRAs requested: clones "10__0" .. "10__{N-1}", each fed by the
//   previous, lora_name + strength patched per request. Downstream
//   references to ["10", 0] / ["10", 1] become ["10__{N-1}", 0/1].
type LoraChainSpec struct {
	Node        string `json:"node"`
	// Input is the loader node's input key for the lora identifier (name OR
	// url depending on the loader implementation).
	Input        string `json:"input"`
	WeightInput  string `json:"weight_input,omitempty"`
	URLInput     string `json:"url_input,omitempty"`
	// ClipWeightInput is the second weight knob on stock LoraLoader
	// (strength_clip). Defaults to WeightInput when unset.
	ClipWeightInput string `json:"clip_weight_input,omitempty"`
	// ModelInput / ClipInput are the loader's input keys carrying the
	// upstream model/clip references. Default to "model" / "clip" which
	// match stock ComfyUI LoraLoader.
	ModelInput string `json:"model_input,omitempty"`
	ClipInput  string `json:"clip_input,omitempty"`
	// ModelOutputIndex / ClipOutputIndex pick which output slots of
	// LoraLoader carry MODEL / CLIP. Default 0 / 1.
	ModelOutputIndex *int `json:"model_output_index,omitempty"`
	ClipOutputIndex  *int `json:"clip_output_index,omitempty"`
}

// ReferenceChainSpec describes how to inject one or more reference images into
// a Flux 2 reference-conditioning chain. The template defines one placeholder
// LoadImage / ImageScaleToTotalPixels / VAEEncode / ReferenceLatent group;
// the adapter clones this group once per requested reference and threads the
// ReferenceLatent.conditioning outputs head-to-tail (each clone's
// `conditioning` input points at the previous clone's output, the first
// clone's at the upstream FluxGuidance node). The downstream consumer (e.g.
// BasicGuider.conditioning) is rewired from the original ref node to the tail
// clone.
//
// This matches the official Comfy-Org Flux 2 example
// (comfyanonymous.github.io/ComfyUI_examples/flux2/flux2_example.png), where
// the note says: "Unbypass the ReferenceLatent nodes to give ref images.
// Chain more of them to give more images."
//
// Layout in the template:
//   "<loader>"  class_type: LoadImage              inputs: { image: "<placeholder>" }
//   "<scale>"   class_type: ImageScaleToTotalPixels inputs: { image: ["<loader>", 0], megapixels: 1.0 }
//   "<encode>"  class_type: VAEEncode               inputs: { pixels: ["<scale>", 0], vae: ["<vae>", 0] }
//   "<ref>"     class_type: ReferenceLatent         inputs: { conditioning: ["<upstream_cond>", 0], latent: ["<encode>", 0] }
//   "<consumer>" e.g. BasicGuider                   inputs: { conditioning: ["<ref>", 0], ... }
//
// With 0 references: the entire load/scale/encode/ref group is removed and
// the consumer's input is rewired to the original ref's upstream conditioning
// (whatever `<upstream_cond>` was), so the model runs as text-only.
//
// With N >= 1 references: clones "<loader>__0..N-1" (and matching scale,
// encode, ref triples). Each cloned LoadImage gets the reference URL
// patched. Each cloned ref's `conditioning` is wired to the previous clone's
// output (head: original upstream cond ref). Consumer node is rewired to the
// tail clone's ref output.
type ReferenceChainSpec struct {
	// LoaderNode is the LoadImage template node id whose `image` input gets
	// patched per reference.
	LoaderNode string `json:"loader_node"`
	// LoaderInput is the input key on the loader carrying the image
	// identifier (URL or filename). Defaults to "image".
	LoaderInput string `json:"loader_input,omitempty"`
	// ScaleNode is the ImageScaleToTotalPixels template node id. Cloned
	// per reference. Its image input is rewired to the per-clone loader.
	ScaleNode string `json:"scale_node"`
	// EncodeNode is the VAEEncode template node id. Cloned per reference.
	// Its `pixels` input is rewired to the per-clone scale node; `vae`
	// is preserved across clones so all references share the VAE.
	EncodeNode string `json:"encode_node"`
	// RefNode is the ReferenceLatent template node id. Cloned per
	// reference. `latent` is rewired to the per-clone encode; `conditioning`
	// chains head-to-tail (clone[0] takes the original upstream ref,
	// clone[i>0] takes ["RefNode__{i-1}", 0]). The consumer node's
	// reference to (RefNode, 0) is rewritten to point at the tail clone.
	RefNode string `json:"ref_node"`
	// RefConditioningInput is the input key on the ReferenceLatent node
	// for the upstream conditioning ref. Defaults to "conditioning".
	RefConditioningInput string `json:"ref_conditioning_input,omitempty"`
	// RefLatentInput is the input key on the ReferenceLatent node for the
	// per-reference latent. Defaults to "latent".
	RefLatentInput string `json:"ref_latent_input,omitempty"`
	// WeightInput is the optional input key on RefNode that takes a per-
	// reference strength. Skipped when empty (Flux 2's stock
	// ReferenceLatent has no weight knob, but downstream community variants
	// might). Defaults to "" (no weighting).
	WeightInput string `json:"weight_input,omitempty"`
	// MaxReferences caps the number of references the adapter will clone.
	// Excess entries past this cap are silently dropped. 0 means no cap.
	MaxReferences int `json:"max_references,omitempty"`
}

// SubmitRequest is the OpenAI-image-shape body unorouter sends in. Adapter parses
// from gin context.
type SubmitRequest struct {
	Prompt         string         `json:"prompt"`
	NegativePrompt string         `json:"negative_prompt,omitempty"`
	Size           string         `json:"size,omitempty"`
	N              int            `json:"n,omitempty"`
	Extra          *SubmitExtra   `json:"extra,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type SubmitExtra struct {
	Loras      []LoraSpec      `json:"loras,omitempty"`
	References []ReferenceSpec `json:"references,omitempty"`
	Hires      *HiresSpec      `json:"hires,omitempty"`
	Seed       *int64          `json:"seed,omitempty"`
	Steps      *int            `json:"steps,omitempty"`
	CFG        *float64        `json:"cfg,omitempty"`
	// Sampler / Scheduler override the KSampler defaults. Useful per-call
	// because different prompts benefit from different solvers (e.g.
	// dpmpp_3m_sde for fine detail vs euler_ancestral for variety) without
	// needing a separate template per combo. Templates opt in by declaring
	// `sampler` / `scheduler` params pointing at the KSampler node.
	Sampler   *string  `json:"sampler,omitempty"`
	Scheduler *string  `json:"scheduler,omitempty"`
	// Denoise is the KSampler `denoise` knob. Always 1.0 for pure txt2img;
	// useful when the same template gets reused for img2img / hires-fix
	// passes that want a partial denoise.
	Denoise *float64 `json:"denoise,omitempty"`
	// N is the OpenAI-style batch count, accepted here in addition to the
	// top-level `metadata.n` so callers can pass it alongside other dynamic
	// knobs (loras, seed, steps, cfg) instead of mixing it with envelope
	// fields. Either location wins; if both are set the larger value applies
	// so a caller can't accidentally downgrade their request.
	N *int `json:"n,omitempty"`

	// Init / mask image URLs for the img2img / upscale / adetailer /
	// inpaint sub-modes. Both fetched and base64-injected via the
	// reference-chain pre-upload pipeline.
	InitImageURL *string `json:"init_image_url,omitempty"`
	MaskURL      *string `json:"mask_url,omitempty"`

	// Upscaler. Filename is the on-disk model_name. ScaleBy is pre-computed
	// by the caller as desired_multiplier / native_scale so the adapter
	// doesn't need a catalog lookup. Multiplier is the caller's final-
	// requested multiplier, kept for observability.
	Upscaler           *string  `json:"upscaler,omitempty"`
	UpscalerScaleBy    *float64 `json:"upscaler_scale_by,omitempty"`
	UpscalerMultiplier *float64 `json:"upscaler_multiplier,omitempty"`
	HiresSteps         *int     `json:"hires_steps,omitempty"`

	// Embeddings. Adapter rewrites the prompt to inject
	// `(embedding:<filename>:<weight>)` tokens before patching
	// CLIPTextEncode. Filename MUST include the file extension — ComfyUI
	// tokenizer errors on `(embedding:Name:1.2)` but accepts
	// `(embedding:Name.safetensors:1.2)`.
	Embeddings []EmbeddingSpec `json:"embeddings,omitempty"`

	// Single-unit ControlNet. Adapter rehosts ImageURL via the reference-
	// chain pattern and patches the ControlNetLoader + ControlNetApply
	// nodes that templates carry with default strength 0.
	ControlNet *ControlNetSpec `json:"control_net,omitempty"`

	// Layer Diffusion. Adapter rewires SaveImage from the stock VAEDecode
	// output to LayeredDiffusionDecodeRGBA when Weight > 0.
	LayerDiffusion *LayerDiffusionSpec `json:"layer_diffusion,omitempty"`

	// ADetailer subform. Nested object — adapter flattens fields to the
	// template's `adetailer_*` keys. The inner Loras run through the
	// applyLoraChain helper against the FaceDetailer's model input.
	Adetailer *AdetailerSpec `json:"adetailer,omitempty"`

	// CLIPSetLastLayer.stop_at_clip_layer convention. Form value is
	// positive (1=no skip, 2=skip last); adapter negates.
	ClipSkip *int `json:"clip_skip,omitempty"`

	// Eta Noise Seed Delta. Maps to Inspire KSampler's variation_seed
	// offset.
	ENSD *uint32 `json:"ensd,omitempty"`
}

// EmbeddingSpec — one textual-inversion entry in submit.extra.embeddings.
type EmbeddingSpec struct {
	Name     string  `json:"name,omitempty"`
	Filename string  `json:"filename,omitempty"`
	Weight   float64 `json:"weight,omitempty"`
}

// ControlNetSpec — single-unit ControlNet for the studio. The Kind
// resolves to a checkpoint filename via the adapter's kind→filename map.
type ControlNetSpec struct {
	Kind     string  `json:"kind,omitempty"`
	ImageURL string  `json:"imageUrl,omitempty"`
	Weight   float64 `json:"weight,omitempty"`
}

// LayerDiffusionSpec — transparent-output toggle. Weight > 0 swaps the
// stock VAEDecode output for LayeredDiffusionDecodeRGBA on SaveImage.
type LayerDiffusionSpec struct {
	Weight float64 `json:"weight,omitempty"`
}

// AdetailerSpec — ADetailer subform. All fields optional; adapter applies
// defaults from the template's FaceDetailer node.
type AdetailerSpec struct {
	YoloModel          *string    `json:"yoloModel,omitempty"`
	Prompt             *string    `json:"prompt,omitempty"`
	NegativePrompt     *string    `json:"negativePrompt,omitempty"`
	Steps              *int       `json:"steps,omitempty"`
	Confidence         *float64   `json:"confidence,omitempty"`
	MaskBlur           *int       `json:"maskBlur,omitempty"`
	Denoise            *float64   `json:"denoise,omitempty"`
	InpaintOnlyMasked  *bool      `json:"inpaintOnlyMasked,omitempty"`
	Loras              []LoraSpec `json:"loras,omitempty"`
}

// ReferenceSpec is one entry in submit.extra.references. URL is the
// HTTPS-fetchable image; Name and Weight are advisory (Name appears in the
// prompt template if the operator uses it, Weight feeds the ReferenceLatent
// weight input when the template wires it up).
type ReferenceSpec struct {
	URL    string  `json:"url,omitempty"`
	Name   string  `json:"name,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

type LoraSpec struct {
	URL              string  `json:"url,omitempty"`
	Name             string  `json:"name,omitempty"`
	CivitaiVersionID int64   `json:"civitai_version_id,omitempty"`
	Weight           float64 `json:"weight,omitempty"`
}

type HiresSpec struct {
	Denoise   *float64 `json:"denoise,omitempty"`
	UpscaleBy *float64 `json:"upscale_by,omitempty"`
}

// Strategy is the per-provider envelope. Workflow JSON, polling lifecycle and
// billing are shared in adaptor.go; only request shape and status URL differ.
//
// SubmitExtras lives in the strategies sub-package to avoid an import cycle
// (strategies imports nothing from this package; this package imports
// strategies). See strategies/types.go.
type Strategy interface {
	// Name returns the provider id ("runpod", "replicate", ...).
	Name() string
	// BuildSubmitRequest packages a patched workflow plus optional extras
	// (such as pre-uploaded reference images) for the provider.
	// patchedWorkflow is the API-format ComfyUI prompt object.
	BuildSubmitRequest(baseURL string, patchedWorkflow []byte, extras *strategies.SubmitExtras) (url string, body []byte, headers map[string]string, err error)
	// ParseSubmitResponse extracts the upstream task id.
	ParseSubmitResponse(body []byte) (upstreamTaskID string, err error)
	// StatusURL returns the GET URL for polling this task id.
	StatusURL(baseURL, upstreamTaskID string) string
	// ParseStatusResponse maps a provider status payload to a relaycommon.TaskInfo.
	ParseStatusResponse(body []byte) (*relaycommon.TaskInfo, error)
}
