#!/usr/bin/env python3
"""Fixed GPU workloads for GPUFit measurements. Prints ONE JSON line.

  gemm   half-precision matrix multiply -> TFLOPS (raw compute, tensor cores)
  train  small CNN training loop on synthetic data -> steps/s (realistic mix)
  copy   device-to-device memory copy -> GB/s (memory bandwidth)
  probe  short run of all three (GPUFit active-probe prototype, see
         docs/design/active-probe.md). Session A on a T4: gemm is POWER-bound
         (ignored a 50% clock lock), the CNN is CLOCK-sensitive (-14% at a 50%
         lock), so a probe needs both parts.

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


def copy(dev, dtype, size, warmup, seconds):
    """size = MiB per buffer. Counts read + write bytes."""
    n = size * 1024 * 1024 // 4
    a = torch.empty(n, device=dev, dtype=torch.float32)
    b = torch.empty_like(a)
    iters, elapsed = timed(dev, lambda: b.copy_(a), warmup, seconds)
    return 2 * a.numel() * 4 * iters / elapsed / 1e9, "GB/s", iters, elapsed


WORKLOADS = {"gemm": gemm, "train": train, "copy": copy}
DEFAULT_SIZE = {  # (cuda, cpu)
    "gemm": (8192, 512), "train": (128, 8), "copy": (512, 16),
}


def main():
    p = argparse.ArgumentParser()
    p.add_argument("workload", choices=[*WORKLOADS, "probe"])
    p.add_argument("--seconds", type=float, default=20)
    p.add_argument("--warmup", type=float, default=10)
    p.add_argument("--size", type=int, help="matrix size (gemm) or batch size (train)")
    p.add_argument("--device", default="auto")
    a = p.parse_args()

    dev = torch.device("cuda" if a.device == "auto" and torch.cuda.is_available() else
                       "cpu" if a.device == "auto" else a.device)
    dtype = torch.float16 if dev.type == "cuda" else torch.float32
    def size_for(w):
        return a.size or DEFAULT_SIZE[w][0 if dev.type == "cuda" else 1]

    if a.workload == "probe":
        # Each part: short warm-up, short measurement. --seconds is per part.
        parts = {}
        for w in ("gemm", "train", "copy"):
            value, unit, _, _ = WORKLOADS[w](dev, dtype, size_for(w), a.warmup, a.seconds)
            parts[w] = {"value": round(value, 3), "unit": unit}
        print(json.dumps({"workload": "probe", "parts": parts, "torch": torch.__version__,
                          "device": torch.cuda.get_device_name(0) if dev.type == "cuda" else dev.type}))
        return

    size = size_for(a.workload)
    value, unit, iters, elapsed = WORKLOADS[a.workload](dev, dtype, size, a.warmup, a.seconds)
    print(json.dumps({
        "workload": a.workload, "value": round(value, 3), "unit": unit,
        "device": torch.cuda.get_device_name(0) if dev.type == "cuda" else dev.type,
        "dtype": str(dtype).replace("torch.", ""), "size": size,
        "iters": iters, "seconds": round(elapsed, 2), "torch": torch.__version__,
    }))


if __name__ == "__main__":
    main()
