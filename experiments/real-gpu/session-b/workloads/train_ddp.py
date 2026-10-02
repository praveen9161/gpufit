#!/usr/bin/env python3
"""Data-parallel training benchmark for session B. Run under torchrun:

  torchrun --nnodes=2 --nproc-per-node=1 --node-rank=R --master-addr=A train_ddp.py

A GEMM-heavy stand-in for transformer training (stacked MLP blocks, fp16
autocast). Gradients are synced every --accum micro-steps (DDP no_sync) and
compressed to fp16, so GPU work dominates network time between the two VMs.

Every rank runs the SAME number of steps (a time-based loop could leave ranks
waiting on each other forever), so the job runs at the pace of its slowest
GPU: exactly the straggler effect session B measures. Rank 0 prints one JSON line.

Job mode (--total-steps N, for the rescue experiments): train N optimizer steps
in total, checkpoint every --ckpt-every steps to --ckpt-url (rank 0, HTTP PUT),
and on start resume from the last checkpoint there. A job moved to other GPUs
loses at most --ckpt-every steps. Rank 0 prints progress lines and a final
JSON line with "workload": "train_job".
"""
import argparse
import io
import json
import os
import time
import urllib.error
import urllib.request

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
    p.add_argument("--total-steps", type=int, default=0, help="job mode: train this many steps in total, resumable")
    p.add_argument("--ckpt-url", default="", help="job mode: checkpoint URL, e.g. http://ckpt-store:8000/train-job.pt")
    p.add_argument("--ckpt-every", type=int, default=10, help="job mode: checkpoint every N steps")
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

    if a.total_steps:
        run_job(a, step, net, opt, rank, world, cuda, dev)
        dist.destroy_process_group()
        return

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


def load_checkpoint(url, net, opt):
    """Returns the step to resume from (0 if there's no checkpoint yet)."""
    if not url:
        return 0
    try:
        with urllib.request.urlopen(url, timeout=60) as r:
            state = torch.load(io.BytesIO(r.read()), map_location="cpu", weights_only=False)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return 0
        raise
    net.load_state_dict(state["model"])
    opt.load_state_dict(state["opt"])
    return state["step"]


def save_checkpoint(url, net, opt, step):
    buf = io.BytesIO()
    torch.save({"model": net.state_dict(), "opt": opt.state_dict(), "step": step}, buf)
    req = urllib.request.Request(url, data=buf.getvalue(), method="PUT")
    urllib.request.urlopen(req, timeout=120).close()


def run_job(a, step, net, opt, rank, world, cuda, dev):
    started = time.time()
    resumed_from = load_checkpoint(a.ckpt_url, net, opt)
    dist.barrier()
    if rank == 0:
        print(json.dumps({"event": "start", "resumed_from": resumed_from, "total_steps": a.total_steps,
                          "world_size": world, "time": started, "load_s": round(time.time() - started, 1)}), flush=True)
    t0, done = time.perf_counter(), resumed_from
    while done < a.total_steps:
        step()
        done += 1
        if done % a.ckpt_every == 0 or done == a.total_steps:
            if cuda:
                torch.cuda.synchronize()
            ts = time.perf_counter()
            if rank == 0 and a.ckpt_url:
                save_checkpoint(a.ckpt_url, net, opt, done)
            dist.barrier()
            if rank == 0:  # save_s: time all ranks waited for the checkpoint (its overhead)
                print(json.dumps({"event": "checkpoint", "step": done, "elapsed_s": round(time.perf_counter() - t0, 1),
                                  "save_s": round(time.perf_counter() - ts, 2), "time": time.time()}), flush=True)
    elapsed = time.perf_counter() - t0
    if rank == 0:
        steps = done - resumed_from
        tokens = steps * a.accum * a.micro_batch * world
        print(json.dumps({
            "workload": "train_job", "world_size": world,
            "device": torch.cuda.get_device_name(dev) if cuda else "cpu",
            "total_steps": a.total_steps, "resumed_from": resumed_from, "steps_this_run": steps,
            "seconds_this_run": round(elapsed, 1), "tokens_per_s": round(tokens / elapsed, 1) if elapsed else 0,
            "started": started, "finished": time.time(), "torch": torch.__version__,
        }), flush=True)


if __name__ == "__main__":
    main()
