# Governed detector: a real model the fleet governs

A small floor-hazard image classifier in four versions, served from an OCI registry inside
the demo cluster, signed with cosign, gated by a `ModelPolicy` on metrics measured on a
held-out frame set, and rolled out by a `ModelRollout` to simulated robots that fetch,
verify, self-check and run it.

**Fidelity: L2.** The control plane, the registry, the signatures and the model are real.
The robots are the in-tree simulation adapter, and the frames are synthesised. No
camera, no site and no person appears in any frame.

## What is here

| File | What it does |
|---|---|
| `frames.py` | Draws 32x32 floor frames in four classes (`clear`, `cable`, `ledge`, `obstacle`) from a seed. The data set is this code, under Apache-2.0. |
| `train.py` | Trains the four versions from scratch with numpy and exports them to ONNX, each with a known-answer self-check file. No pretrained weights. |
| `publish.sh` | Pushes each version to a registry as an OCI artifact with `oras` and signs it with `cosign`. |
| `registry.yaml` | A CNCF Distribution registry for the demo cluster. |
| `modelpolicy.yaml` | The quality gate. |
| [`../../adapters/simulation/model_runtime.py`](../../adapters/simulation/model_runtime.py) | The adapter side: fetch, verify, load, self-check, run. Optional. |

The evaluation job is not in this repository. That job runs a candidate on the held-out set and
POSTs the metrics it measured to the policy's webhook, and the gate reads nothing else. To drive
the gate, evaluate each candidate on the held-out set (`frames.py --seed 2002`, below) with your
own job and POST the results in the payload format
[RFC-0001](../../rfcs/dist/RFC-0001-core-spec.md#modelpolicy-training-pipeline-integration-webhook-payload)
specifies. The rollout half runs without it: a `ModelRollout` that carries the `modelUri`,
`modelChecksum` and `modelSignatureRef` that `publish.sh` prints pushes that version to the robots.

## The four versions

| Version | Recipe | What it is for |
|---|---|---|
| 1.0.0 | 4/8 filters, 250 frames per class, 10 epochs | the baseline the fleet starts on |
| 2.0.0 | as 1.0.0, but 70 % of the `cable` training labels written as `clear` | a worse candidate; the gate blocks it on the cable recall it lost |
| 3.0.0 | 8/16 filters, 1000 frames per class, 25 epochs, flip augmentation | a better candidate; the gate admits it |
| 4.0.0 | 3.0.0 exported with a NaN output bias | loads, fails its self-check on the robot, and is rolled back |

The network: per-frame standardisation, two 3x3 convolution + ReLU + 2x2 max-pool stages,
one dense layer, softmax. Opset 17.

## Build and publish

Two Python environments, because the exporter and the adapter pin different protobuf
runtimes:

```bash
# build: numpy + onnx (+ onnxruntime to evaluate)
python -m venv /tmp/gd-build && /tmp/gd-build/bin/pip install numpy onnx onnxruntime
cd examples/governed-detector
/tmp/gd-build/bin/python train.py --out /tmp/gd/build
/tmp/gd-build/bin/python frames.py --seed 2002 --per-class 150 --out /tmp/gd/eval.npz   # held-out set
/tmp/gd-build/bin/python frames.py --seed 4004 --per-class 3 --out /tmp/gd/robot-frames.npz

# a cosign key pair, kept outside the repository
(cd /tmp/gd && cosign generate-key-pair)

kubectl apply -f registry.yaml
kubectl -n model-registry port-forward svc/model-registry 5051:5000 &
./publish.sh /tmp/gd/build localhost:5051 /tmp/gd/cosign.key > /tmp/gd/artifacts.tsv
```

`publish.sh` prints, per version, the `modelUri`, the `modelChecksum` and the
`modelSignatureRef` a `ModelRollout` or a policy payload carries. The checksum is the
manifest digest, which pins every layer. The creation time is fixed, so the same build
pushed to any registry has the same digest.

## What the adapter checks

Started with `--model-trust-root` (the public key `SwarmadaConfig.spec.signing.trustRoots`
pins) and `--model-frames`, the simulation adapter treats every `model_update` as a real
artifact:

1. the manifest's SHA-256 must equal `modelChecksum`;
2. `modelSignatureRef` must name a cosign signature that verifies against the trust root,
   claims that manifest digest and names the same repository;
3. every layer must hash to the digest the manifest declares.

Any failure refuses the push before it is acknowledged. After the acknowledgement the
model is loaded and must pass its self-check (finite, well-formed probabilities that agree
with the answers recorded at export); only then is it switched in and run on the fixed
frames, and the result logged as a `MODEL_RUN` line. A self-check failure leaves the
previous model running and reports the install failed, which is what triggers the
rollout's automatic rollback.

The extra is optional. Install it into the interpreter that runs the adapter:

```bash
pip install -r adapters/simulation/requirements-model-runtime.txt
```

Without `--model-trust-root` the adapter needs none of it and behaves as before.

## Licences

| Component | Licence |
|---|---|
| The model weights, the frames, the code here | Apache-2.0 (produced by this directory) |
| onnxruntime | MIT |
| onnx (build only) | Apache-2.0 |
| numpy | BSD-3-Clause |
| cryptography | Apache-2.0 OR BSD-3-Clause |
| CNCF Distribution (`registry:2`) | Apache-2.0 |
| cosign, oras (tools) | Apache-2.0 |
