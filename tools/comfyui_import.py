#!/usr/bin/env python3
"""Offline translator for a strict SDXL ComfyUI API-format text-to-image graph.

Never imports ComfyUI/custom nodes, reads weights, downloads files, or contacts a service.
"""
import argparse
import json
import os
import re
from pathlib import Path

PAIRS = {("euler", "normal"): ("euler", "euler"),
         ("euler_ancestral", "normal"): ("euler-ancestral", "euler"),
         ("dpmpp_2m", "karras"): ("dpmpp-2m", "karras"),
         ("ddim", "normal"): ("ddim", "ddim"),
         ("uni_pc", "normal"): ("unipc", "unipc")}
FIELDS = {
    "CheckpointLoaderSimple": {"ckpt_name"}, "CLIPTextEncode": {"text", "clip"},
    "EmptyLatentImage": {"width", "height", "batch_size"},
    "KSampler": {"model", "positive", "negative", "latent_image", "seed", "steps", "cfg", "sampler_name", "scheduler", "denoise"},
    "VAEDecode": {"samples", "vae"}, "SaveImage": {"images", "filename_prefix"},
}


def translate(graph, model_map, recipe_id):
    if not isinstance(graph, dict) or not graph or len(graph) > 32:
        raise ValueError("Expected an API-format graph with 1–32 nodes, not a UI workflow export")
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", recipe_id):
        raise ValueError("recipe_id must contain 1–80 letters, digits, underscores or hyphens")
    for key, node in graph.items():
        if not isinstance(node, dict) or set(node) - {"class_type", "inputs", "_meta"}:
            raise ValueError(f"Node {key}: invalid node shape")
        kind = node.get("class_type")
        if kind not in FIELDS:
            raise ValueError(f"Node {key}: unsupported class {kind!r}; custom nodes never execute")
        inputs = node.get("inputs")
        if not isinstance(inputs, dict) or set(inputs) != FIELDS[kind]:
            raise ValueError(f"Node {key}: expected exactly the supported {kind} inputs")
    used = set()

    def one(kind):
        matches = [k for k, n in graph.items() if n["class_type"] == kind]
        if len(matches) != 1:
            raise ValueError(f"Expected exactly one {kind}")
        used.add(matches[0])
        return matches[0], graph[matches[0]]["inputs"]

    def edge(value, node_id, slot):
        if value != [node_id, slot] or type(value[1]) is not int:
            raise ValueError(f"Expected link to node {node_id} output {slot}")

    checkpoint_id, checkpoint = one("CheckpointLoaderSimple")
    latent_id, latent = one("EmptyLatentImage")
    sampler_id, sampler = one("KSampler")
    decoder_id, decoder = one("VAEDecode")
    _, save = one("SaveImage")
    edge(sampler["model"], checkpoint_id, 0)
    edge(sampler["latent_image"], latent_id, 0)
    edge(decoder["samples"], sampler_id, 0)
    edge(decoder["vae"], checkpoint_id, 2)
    edge(save["images"], decoder_id, 0)
    texts = {}
    for role in ("positive", "negative"):
        link = sampler[role]
        if not isinstance(link, list) or len(link) != 2 or link[0] not in graph:
            raise ValueError(f"Invalid {role} conditioning link")
        node = graph[link[0]]
        if node["class_type"] != "CLIPTextEncode":
            raise ValueError(f"{role} conditioning must be CLIPTextEncode")
        edge(link, link[0], 0)
        edge(node["inputs"]["clip"], checkpoint_id, 1)
        if not isinstance(node["inputs"]["text"], str):
            raise ValueError(f"{role} conditioning must be literal text")
        texts[role] = node["inputs"]["text"]
        used.add(link[0])
    if used != set(graph):
        raise ValueError("Disconnected or additional branches are unsupported")
    model = model_map.get(checkpoint["ckpt_name"])
    if not isinstance(model, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", model):
        raise ValueError("Checkpoint requires an explicit mapping to a registered SDXL model ID")
    if sampler["denoise"] != 1 or isinstance(sampler["denoise"], bool):
        raise ValueError("Only full-denoise text-to-image is supported")
    if (sampler["sampler_name"], sampler["scheduler"]) not in PAIRS:
        raise ValueError("Unsupported sampler/scheduler pair; no substitution is performed")
    def integer(value, low, high, name, multiple=1):
        if type(value) is not int or not low <= value <= high or value % multiple:
            raise ValueError(f"{name} must be an integer in {low}–{high}, multiple of {multiple}")
        return value
    params = {
        "width": integer(latent["width"], 64, 2048, "width", 8),
        "height": integer(latent["height"], 64, 2048, "height", 8),
        "count": integer(latent["batch_size"], 1, 4, "batch size"),
        "seed": integer(sampler["seed"], 0, 4294967295, "seed"),
        "steps": integer(sampler["steps"], 1, 150, "steps"),
    }
    if type(sampler["cfg"]) not in (int, float) or not 0 <= sampler["cfg"] <= 30:
        raise ValueError("CFG must be a finite number in 0–30")
    params["guidance"] = sampler["cfg"]
    params["sampler"], params["scheduler"] = PAIRS[sampler["sampler_name"], sampler["scheduler"]]
    if not texts["positive"].strip():
        raise ValueError("A nonempty positive prompt is required")
    recipe = {"schema_version": 2, "recipe_id": recipe_id, "revision": 1,
              "inputs": {"prompt": {"type": "string", "required": True}, "negative_prompt": {"type": "string", "required": False}},
              "operations": [{"op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
                              "inputs": {"prompt": {"from": "input:prompt"}, "negative_prompt": {"from": "input:negative_prompt"}},
                              "defaults": params}], "publish": ["generate/image"]}
    request = {"recipe_id": recipe_id, "model_id": model, "inputs": {"prompt": texts["positive"], "negative_prompt": texts["negative"]}, "parameters": {}}
    return {"recipe": recipe, "request": request, "notes": ["Only the declared SDXL API graph was translated; no graph code executed.", "SaveImage filename_prefix is replaced by Runtime output IDs; pixels and generation settings are not implicitly changed.", "Batch seed progression is Runtime's documented seed + output index; cross-engine pixel identity is not promised."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("graph", type=Path)
    parser.add_argument("--model-map", required=True, type=Path)
    parser.add_argument("--recipe-id", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        if args.graph.stat().st_size > 4 * 1024 * 1024 or args.model_map.stat().st_size > 1024 * 1024:
            raise ValueError("Input document exceeds the offline import size limit")
        result = translate(json.loads(args.graph.read_text()), json.loads(args.model_map.read_text()), args.recipe_id)
        args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        for name, value in result.items():
            fd = os.open(args.output / f"{name}.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w") as stream:
                json.dump(value, stream, indent=2, allow_nan=False)
                stream.write("\n")
        print("Wrote recipe.json, request.json and notes.json; no Runtime state was changed.")
    except (ValueError, OSError, TypeError, KeyError) as error:
        parser.exit(2, f"Import refused: {error}\n")


if __name__ == "__main__":
    main()
