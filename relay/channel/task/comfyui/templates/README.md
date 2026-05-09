# ComfyUI workflow templates

This directory holds reference workflow JSON snippets operators can paste into the **workflow_templates** field on a ComfyUI-type channel.

## Authoring a template

1. Build the workflow visually in ComfyUI desktop.
2. Settings -> Dev Mode -> enable "Save (API Format)".
3. Workflow menu -> Save (API Format) -> save the JSON.
4. Wrap it in the per-channel envelope (see `example-channel-config.json`):
   - `provider` selects the strategy (currently only `runpod`).
   - `app` is the provider-specific endpoint id (RunPod serverless endpoint id, fal app slug, etc.). The strategy uses it to build the submit URL.
   - `templates.<model_name>.workflow` is the API-format graph.
   - `templates.<model_name>.params` binds logical request fields (`prompt`, `negative_prompt`, `seed`, `steps`, `cfg`, `width`, `height`, `hires_denoise`, `hires_upscale`) to specific `node`/`input` pairs.
   - `templates.<model_name>.lora_chain` (optional) describes how the adapter injects one or more LoRAs into a chained LoraLoader sub-graph. `node` is the LoraLoader template node id, `input` is the lora identifier input, `url_input` is the URL input if your loader supports URLs (e.g. `LoraLoaderFromURL`), `weight_input` is the model strength input, `clip_weight_input` is the clip strength input.
   - `templates.<model_name>.references` (optional, Flux 2 reference-image composition) describes how the adapter injects one or more reference images into a chained `LoadImage -> ImageScaleToTotalPixels -> VAEEncode -> ReferenceLatent` sub-graph. The template defines one placeholder quadruple; the adapter clones it per requested reference and chains `ReferenceLatent.conditioning` head-to-tail. Required fields: `loader_node` (LoadImage id), `scale_node`, `encode_node`, `ref_node`. Optional: `loader_input` (defaults to `image`), `ref_conditioning_input` (defaults to `conditioning`), `ref_latent_input` (defaults to `latent`), `weight_input`, `max_references` (cap, 0 = unlimited). With zero references the placeholder group is dropped and downstream consumers (e.g. BasicGuider) rewire to the original RefNode's upstream conditioning. See the `flux2-dev-compose` example for the exact node layout.

## Workflow notes per model family

- **SDXL (KSampler-based)**: dimensions live on a single `EmptyLatentImage` node, so `width` / `height` params each map to one node/input pair.
- **Flux 2**: uses the new graph (`SamplerCustomAdvanced` + `Flux2Scheduler` + `EmptyFlux2LatentImage` + `FluxGuidance` + `CLIPLoader type: flux2`). No negative prompt (CFG is baked into `FluxGuidance`, not classifier-free). Width/height live in BOTH `EmptyFlux2LatentImage` and `Flux2Scheduler`; the adapter currently writes a param to a single node, so resolutions are hard-coded in both nodes and only `prompt`, `seed`, `steps`, `cfg` (= guidance) are exposed.
- **Flux 2 with references** (compose / image-edit): adds `LoadImage -> ImageScaleToTotalPixels -> VAEEncode -> ReferenceLatent` placeholder quadruple between `FluxGuidance` and `BasicGuider`. The placeholder's `ReferenceLatent.conditioning` reads from `FluxGuidance`; `BasicGuider.conditioning` reads from the placeholder `ReferenceLatent` (NOT directly from `FluxGuidance`). The adapter clones the quadruple per requested reference and threads the `ReferenceLatent.conditioning` chain head-to-tail. With zero references the entire quadruple is dropped and `BasicGuider.conditioning` rewires to `FluxGuidance` so the model degrades to text-only. Layout matches the official Comfy-Org example PNG (`comfyanonymous.github.io/ComfyUI_examples/flux2/flux2_example.png`); the note on that PNG says: "Unbypass the ReferenceLatent nodes to give ref images. Chain more of them to give more images."

## Notes

- Always seed-randomize on submit (set `auto_random: true` on the seed param). ComfyUI dedupes identical workflow JSON and won't re-render otherwise.
- Inject parameter values by parsing the JSON object, mutating, then re-marshalling. Never string-substitute into the workflow text.
- Test with a real provider before committing. Incorrect node IDs surface as `node_errors` in the provider response.