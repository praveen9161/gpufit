## Benchmarks (median of repeats)

| setting | workload | measured | spread | vs baseline | GPUFit compute | deficit flagged |
|---|---|---|---|---|---|---|
| baseline | gemm | 63.557 TFLOPS | 0.1% | 1.00 | 1.00 | 0/28 |
| baseline | train | 112.853 steps/s | 0.1% | 1.00 | 1.00 | 0/27 |
| power250W | gemm | 59.535 TFLOPS | 0.2% | 0.94 | 0.83 | 0/27 |
| power250W | train | 112.822 steps/s | 0.0% | 1.00 | 0.83 | 0/18 |
| power200W | gemm | 53.609 TFLOPS | 0.0% | 0.84 | 0.67 | 0/30 |
| power200W | train | 111.598 steps/s | 0.0% | 0.99 | 0.67 | 0/18 |
| power150W | gemm | 43.103 TFLOPS | 0.3% | 0.68 | 0.50 | 0/33 |
| power150W | train | 105.691 steps/s | 0.0% | 0.94 | 0.50 | 0/18 |
| power100W | gemm | 21.998 TFLOPS | 0.4% | 0.35 | 0.33 | 0/48 |
| power100W | train | 80.642 steps/s | 0.2% | 0.71 | 0.33 | 0/18 |
| clock90pct | gemm | 61.926 TFLOPS | 0.0% | 0.97 | 0.89 | 27/27 |
| clock90pct | train | 110.251 steps/s | 0.0% | 0.98 | 0.55 | 18/18 |
| clock70pct | gemm | 48.196 TFLOPS | 0.0% | 0.76 | 0.57 | 30/30 |
| clock70pct | train | 98.539 steps/s | 0.1% | 0.87 | 0.41 | 18/18 |
| clock50pct | gemm | 34.347 TFLOPS | 0.0% | 0.54 | 0.42 | 36/36 |
| clock50pct | train | 83.406 steps/s | 0.1% | 0.74 | 0.34 | 18/18 |

## Probe parts vs baseline (median of repeats)

| setting | gemm (power-bound) | train (clock-sensitive) | copy (bandwidth) |
|---|---|---|---|
| baseline | 1.00 | 1.00 | 1.00 |
| power250W | 0.94 | 1.00 | 1.00 |
| power200W | 0.84 | 0.99 | 1.00 |
| power150W | 0.68 | 0.94 | 1.00 |
| power100W | 0.35 | 0.71 | 1.00 |
| clock90pct | 0.97 | 0.98 | 1.00 |
| clock70pct | 0.76 | 0.87 | 1.00 |
| clock50pct | 0.54 | 0.74 | 1.00 |
