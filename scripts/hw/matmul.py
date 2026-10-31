#!/usr/bin/env python3
# Copyright 2026 Scott Friedman
# SPDX-License-Identifier: Apache-2.0
"""Steady square-matmul load for tach validation (issues #10, #11, #13).

Runs C = A @ B in a loop for --seconds and prints achieved TFLOPS
(2*n^3 floating-point operations per matmul) with Unix-epoch start/end
timestamps for aligning against the tach CSV.

  python3 matmul.py --dtype fp32|bf16|fp64 --seconds 60 --size 8192

For fp32, TF32 is switched off explicitly so the run measures FP32 pipes,
not tensor cores.
"""

import argparse
import sys
import time

DTYPES = ("fp32", "bf16", "fp64")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dtype", choices=DTYPES, default="bf16")
    ap.add_argument("--seconds", type=float, default=60.0)
    ap.add_argument("--size", type=int, default=8192, help="matrix dimension n")
    ap.add_argument("--device", default="cuda", help="torch device (default cuda)")
    ap.add_argument("--warmup", type=int, default=3, help="untimed matmuls before the loop")
    args = ap.parse_args()

    try:
        import torch
    except ImportError:
        print(
            "matmul.py: PyTorch is not installed. Install it with:\n"
            "  python3 -m pip install torch\n"
            "(in a venv on systems with an externally managed Python:\n"
            "  python3 -m venv ~/tach-venv && ~/tach-venv/bin/pip install torch)",
            file=sys.stderr,
        )
        sys.exit(2)

    if not torch.cuda.is_available():
        print("matmul.py: torch.cuda.is_available() is False", file=sys.stderr)
        sys.exit(1)

    dtype = {"fp32": torch.float32, "bf16": torch.bfloat16, "fp64": torch.float64}[args.dtype]
    if args.dtype == "fp32":
        torch.backends.cuda.matmul.allow_tf32 = False
        torch.backends.cudnn.allow_tf32 = False
    print(
        "dtype=%s size=%d device=%s gpu=\"%s\" torch=%s cuda=%s "
        "allow_tf32.matmul=%s allow_tf32.cudnn=%s"
        % (
            args.dtype,
            args.size,
            args.device,
            torch.cuda.get_device_name(),
            torch.__version__,
            torch.version.cuda,
            torch.backends.cuda.matmul.allow_tf32,
            torch.backends.cudnn.allow_tf32,
        ),
        flush=True,
    )

    n = args.size
    a = torch.randn(n, n, device=args.device, dtype=dtype)
    b = torch.randn(n, n, device=args.device, dtype=dtype)
    c = torch.empty(n, n, device=args.device, dtype=dtype)

    for _ in range(args.warmup):
        torch.matmul(a, b, out=c)
    torch.cuda.synchronize()

    flops_per = 2.0 * n ** 3
    iters = 0
    wall_start = time.time()
    t0 = time.perf_counter()
    while True:
        torch.matmul(a, b, out=c)
        torch.cuda.synchronize()
        iters += 1
        elapsed = time.perf_counter() - t0
        if elapsed >= args.seconds:
            break
    wall_end = time.time()

    tflops = flops_per * iters / elapsed / 1e12
    print(
        "dtype=%s size=%d iters=%d seconds=%.3f tflops=%.3f start=%.6f end=%.6f"
        % (args.dtype, n, iters, elapsed, tflops, wall_start, wall_end),
        flush=True,
    )


if __name__ == "__main__":
    main()
