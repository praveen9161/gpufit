#!/usr/bin/env python3
"""Fixed GPU workloads for GPUFit measurements. Prints ONE JSON line.

  gemm   half-precision matrix multiply -> TFLOPS (raw compute, tensor cores)
  train  small CNN training loop on synthetic data -> steps/s (realistic mix)

Each run warms up first (clocks and temperature settle), then measures for a
fixed time. Same inputs every run, so results are comparable across power and
clock settings. Runs on CPU too (tiny sizes), which is how it's tested locally.
"""
import argparse
import json
import time

import torch


def sync(dev):
    if dev.type == "cuda":
        torch.cuda.synchronize()


def timed(dev, step, warmup, seconds):
    """Run step() for `warmup` seconds, then count iterations for `seconds`."""
    end = time.perf_counter() + warmup
    while time.perf_counter() < end:
        step()
    sync(dev)
    iters, start = 0, time.perf_counter()
    while True:
        for _ in range(10):  # check the clock every 10 steps, not every step
            step()
        iters += 10
        sync(dev)
        elapsed = time.perf_counter() - start
        if elapsed >= seconds:
            return iters, elapsed


def gemm(dev, dtype, size, warmup, seconds):
    a = torch.randn(size, size, device=dev, dtype=dtype)
    b = torch.randn(size, size, device=dev, dtype=dtype)
    iters, elapsed = timed(dev, lambda: a @ b, warmup, seconds)
    return 2 * size**3 * iters / elapsed / 1e12, "TFLOPS", iters, elapsed


def train(dev, dtype, size, warmup, seconds):
    torch.manual_seed(0)
    model = torch.nn.Sequential(
        torch.nn.Conv2d(3, 64, 3, padding=1), torch.nn.ReLU(),
        torch.nn.Conv2d(64, 128, 3, padding=1, stride=2), torch.nn.ReLU(),
        torch.nn.Conv2d(128, 256, 3, padding=1, stride=2), torch.nn.ReLU(),
        torch.nn.AdaptiveAvgPool2d(1), torch.nn.Flatten(), torch.nn.Linear(256, 10),
    ).to(dev)
    opt = torch.optim.SGD(model.parameters(), lr=0.01)
    x = torch.randn(size, 3, 64, 64, device=dev)
    y = torch.randint(0, 10, (size,), device=dev)
    use_amp = dev.type == "cuda"
    scaler = torch.amp.GradScaler("cuda", enabled=use_amp)

    def step():
        opt.zero_grad(set_to_none=True)
        with torch.autocast(dev.type, dtype=dtype, enabled=use_amp):
            loss = torch.nn.functional.cross_entropy(model(x), y)
        scaler.scale(loss).backward()
        scaler.step(opt)
        scaler.update()

    iters, elapsed = timed(dev, step, warmup, seconds)
    return iters / elapsed, "steps/s", iters, elapsed


def main():
    p = argparse.ArgumentParser()
    p.add_argument("workload", choices=["gemm", "train"])
    p.add_argument("--seconds", type=float, default=20)
    p.add_argument("--warmup", type=float, default=10)
    p.add_argument("--size", type=int, help="matrix size (gemm) or batch size (train)")
    p.add_argument("--device", default="auto")
    a = p.parse_args()

    dev = torch.device("cuda" if a.device == "auto" and torch.cuda.is_available() else
                       "cpu" if a.device == "auto" else a.device)
    dtype = torch.float16 if dev.type == "cuda" else torch.float32
    size = a.size or {"gemm": 8192 if dev.type == "cuda" else 512,
                      "train": 128 if dev.type == "cuda" else 8}[a.workload]

    value, unit, iters, elapsed = {"gemm": gemm, "train": train}[a.workload](dev, dtype, size, a.warmup, a.seconds)
    print(json.dumps({
        "workload": a.workload, "value": round(value, 3), "unit": unit,
        "device": torch.cuda.get_device_name(0) if dev.type == "cuda" else dev.type,
        "dtype": str(dtype).replace("torch.", ""), "size": size,
        "iters": iters, "seconds": round(elapsed, 2), "torch": torch.__version__,
    }))


if __name__ == "__main__":
    main()
