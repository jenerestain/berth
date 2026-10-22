# FAQ

**What is an engine?** A program that runs a model on your GPU: llama.cpp, llama-swap, Ollama,
vLLM, ComfyUI, a voice server. Berth doesn't run models; it decides which engine has the card.

**Why not just llama-swap?** Use both. llama-swap swaps the models in its own config. Berth
covers the whole card, including ComfyUI, games and the stray process holding memory that
neither tool started. See [Berth and llama-swap](https://github.com/T-Crypt/berth#berth-and-llama-swap).

**Can a 700B model take turns on a 24 GB card?** No. Berth gives each engine the whole card in
turn, or a sized share with `--needs-mib`; it doesn't make a model fit that doesn't.

**How reliable is it?** Alpha. It runs every day on the author's RTX 4090 workstation, and v0.3
added `contrib/chaos`, the swap bench and the [tested hardware](benchmarks.md#tested-hardware) table so you can
check it on yours.

**AMD? Windows?** Linux AMD RDNA3/RDNA4 discrete and Phoenix/780M APU support is experimental.
APU leases are exclusive, track UMA VRAM/GTT separately, and require host-RAM headroom;
APU parking and other APU generations are not supported yet.
Windows with NVIDIA is experimental too, and `contrib/windows/check.ps1` checks it on your
machine; see the [support matrix](https://github.com/T-Crypt/berth#support-matrix).

**Can Berth handle ComfyUI on the host and vLLM in Docker?** Yes. Start each one under its own
lease: `berth run --as comfyui -- python ...` for ComfyUI, and
`berth run --as vllm --container vllm -- docker run --rm --name vllm --gpus all ... vllm/vllm-openai`
for vLLM. The container's GPU processes are parented by containerd-shim, so the process tree
cannot reach them; the lease names the container instead, Berth matches the processes by
container ID, and the drain runs `docker stop -t 30 vllm` (podman's when podman resolved it)
instead of signalling the wrapper. The handoff waits for the driver to show the container's
memory gone, as with a host process. A container Berth did not start is still a stowaway:
reported with its container ID, report only, never signalled. Details in
[contrib/INTEGRATION.md](https://github.com/T-Crypt/berth/blob/main/contrib/INTEGRATION.md).
