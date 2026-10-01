#!/usr/bin/env python3
"""Inference benchmark for session B: LLM-style token generation (decode).

Each generated token pushes a small batch through large fp16 weights, so the
GPU mostly streams weights from memory: decode is memory-bandwidth-bound, like
real LLM serving at small batch sizes. (Session A2 on an A10G: memory bandwidth
was unaffected by power limits and clock locks.) A synthetic stand-in, not a
real model. Prints one JSON line with tokens/s.
"""
import argparse
import json
import time

import torch


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--hidden", type=int, default=8192)
    p.add_argument("--layers", type=int, default=8, help="each layer: hidden x 4*hidden and back (fp16)")
    p.add_argument("--batch", type=int, default=8, help="sequences decoded together")
    p.add_argument("--warmup", type=float, default=10)
    p.add_argument("--seconds", type=float, default=60)
    a = p.parse_args()

    cuda = torch.cuda.is_available()
    dev = torch.device("cuda" if cuda else "cpu")
    dtype = torch.float16 if cuda else torch.float32
    torch.manual_seed(0)
    weights = [(torch.randn(a.hidden, 4 * a.hidden, device=dev, dtype=dtype) * 0.01,
                torch.randn(4 * a.hidden, a.hidden, device=dev, dtype=dtype) * 0.01) for _ in range(a.layers)]
    h = torch.randn(a.batch, a.hidden, device=dev, dtype=dtype)

    @torch.no_grad()
    def token():
        x = h
        for w1, w2 in weights:
            x = torch.nn.functional.gelu(x @ w1) @ w2
        return x

    def run(seconds):
        n, start = 0, time.perf_counter()
        while True:
            for _ in range(10):
                token()
            n += 10
            if cuda:
                torch.cuda.synchronize()
            if time.perf_counter() - start >= seconds:
                return n, time.perf_counter() - start

    run(a.warmup)
    n, elapsed = run(a.seconds)
    weight_bytes = sum(w1.numel() + w2.numel() for w1, w2 in weights) * weights[0][0].element_size()
    print(json.dumps({
        "workload": "infer_decode", "device": torch.cuda.get_device_name(0) if cuda else "cpu",
        "tokens_per_s": round(n * a.batch / elapsed, 1), "steps_per_s": round(n / elapsed, 2),
        "weights_gb": round(weight_bytes / 1e9, 2),
        "effective_gb_per_s": round(n * weight_bytes / elapsed / 1e9, 1),
        "hidden": a.hidden, "layers": a.layers, "batch": a.batch, "seconds": round(elapsed, 1),
        "torch": torch.__version__,
    }), flush=True)


if __name__ == "__main__":
    main()
