#!/usr/bin/env python3
"""Data-parallel training benchmark for session B. Run under torchrun:

  torchrun --nnodes=2 --nproc-per-node=1 --node-rank=R --master-addr=A train_ddp.py

A GEMM-heavy stand-in for transformer training (stacked MLP blocks, fp16
autocast). Gradients are synced every --accum micro-steps (DDP no_sync) and
compressed to fp16, so GPU work dominates network time between the two VMs.

Every rank runs the SAME number of steps (a time-based loop could leave ranks
waiting on each other forever), so the job runs at the pace of its slowest
GPU: exactly the straggler effect session B measures. Rank 0 prints one JSON line.
"""
import argparse
import json
import os
import time

import torch
import torch.distributed as dist
from torch.distributed.algorithms.ddp_comm_hooks import default_hooks
from torch.nn.parallel import DistributedDataParallel as DDP


def model(hidden, layers):
    blocks = []
    for _ in range(layers):
        blocks += [torch.nn.Linear(hidden, 4 * hidden), torch.nn.GELU(), torch.nn.Linear(4 * hidden, hidden)]
    return torch.nn.Sequential(*blocks)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--hidden", type=int, default=2048)
    p.add_argument("--layers", type=int, default=4)
    p.add_argument("--micro-batch", type=int, default=8192, help="tokens per micro-step")
    p.add_argument("--accum", type=int, default=16, help="micro-steps per gradient sync")
    p.add_argument("--warmup-steps", type=int, default=3)
    p.add_argument("--steps", type=int, default=30, help="measured optimizer steps")
    a = p.parse_args()

    cuda = torch.cuda.is_available()
    dist.init_process_group("nccl" if cuda else "gloo")
    rank, world = dist.get_rank(), dist.get_world_size()
    dev = torch.device("cuda", int(os.environ.get("LOCAL_RANK", 0))) if cuda else torch.device("cpu")
    if cuda:
        torch.cuda.set_device(dev)

    torch.manual_seed(0)
    net = model(a.hidden, a.layers).to(dev)
    ddp = DDP(net, device_ids=[dev.index] if cuda else None)
    if cuda:
        ddp.register_comm_hook(dist.group.WORLD, default_hooks.fp16_compress_hook)
    opt = torch.optim.SGD(ddp.parameters(), lr=1e-4, momentum=0.9)
    x = torch.randn(a.micro_batch, a.hidden, device=dev)
    y = torch.randn(a.micro_batch, a.hidden, device=dev)

    def step():
        opt.zero_grad(set_to_none=True)
        for i in range(a.accum):
            last = i == a.accum - 1
            ctx = ddp.no_sync() if not last else torch.enable_grad()
            with ctx, torch.autocast(dev.type, dtype=torch.float16, enabled=cuda):
                loss = torch.nn.functional.mse_loss(ddp(x), y)
            loss.backward()
        opt.step()

    for _ in range(a.warmup_steps):
        step()
    if cuda:
        torch.cuda.synchronize()
    dist.barrier()
    start = time.perf_counter()
    for _ in range(a.steps):
        step()
    if cuda:
        torch.cuda.synchronize()
    dist.barrier()
    elapsed = time.perf_counter() - start

    if rank == 0:
        tokens = a.steps * a.accum * a.micro_batch * world
        params = sum(t.numel() for t in net.parameters())
        print(json.dumps({
            "workload": "train_ddp", "world_size": world,
            "device": torch.cuda.get_device_name(dev) if cuda else "cpu",
            "tokens_per_s": round(tokens / elapsed, 1),
            "tokens_per_s_per_gpu": round(tokens / elapsed / world, 1),
            "step_s": round(elapsed / a.steps, 3), "steps": a.steps, "seconds": round(elapsed, 1),
            "params_m": round(params / 1e6, 1), "hidden": a.hidden, "layers": a.layers,
            "micro_batch": a.micro_batch, "accum": a.accum, "torch": torch.__version__,
        }), flush=True)
    dist.destroy_process_group()


if __name__ == "__main__":
    main()
