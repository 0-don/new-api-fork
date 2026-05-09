package strategies

// SubmitExtras is a side-channel for non-workflow fields that need to land
// alongside the workflow under the provider's `input` envelope. Used today
// for `images` (worker-comfyui's `input.images: [{name, image}]` for
// pre-uploading reference files into ComfyUI's input directory before
// workflow execution). Strategies are free to ignore keys they don't
// recognize.
//
// Lives in this sub-package (not in the parent comfyui package) because the
// Strategy interface in comfyui/types.go references it. Keeping it here
// avoids an import cycle: comfyui imports strategies, never the other way.
type SubmitExtras struct {
	// Images is the list of pre-uploaded image files referenced by name in
	// the patched workflow's LoadImage nodes. Each entry is
	// { "name": "<filename>", "image": "<base64 or data URI>" }.
	Images []map[string]any `json:"images,omitempty"`
}
