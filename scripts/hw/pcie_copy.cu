// Copyright 2026 Scott Friedman
// SPDX-License-Identifier: Apache-2.0
//
// pcie_copy: a fixed-size pinned cudaMemcpy loop, host-to-device for N
// seconds, then device-to-host for N seconds, so tach --pcie can be checked
// against a known copy rate (issue #9: units and direction of DCGM fields
// 1009/1010).
//
// Build:  nvcc -O2 -o pcie_copy pcie_copy.cu
// Usage:  pcie_copy [size_mib] [seconds_per_phase] [device]
//         defaults: 1024 MiB, 20 s, device 0
//
// Output, one line per phase (GB/s = 1e9 bytes/s; start/end are Unix epoch
// seconds, wall clock, for aligning against the tach CSV timestamps):
//   phase=h2d bytes=... seconds=... gbps=... copies=... start=... end=...

#include <cuda_runtime.h>

#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>

#define CHECK(call)                                                          \
    do {                                                                     \
        cudaError_t err_ = (call);                                           \
        if (err_ != cudaSuccess) {                                           \
            std::fprintf(stderr, "pcie_copy: %s:%d: %s: %s\n", __FILE__,     \
                         __LINE__, #call, cudaGetErrorString(err_));         \
            std::exit(1);                                                    \
        }                                                                    \
    } while (0)

static double wall_now() {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec * 1e-9;
}

static double mono_now() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec * 1e-9;
}

static long parse_arg(const char *s, const char *what) {
    char *end = nullptr;
    errno = 0;
    long v = std::strtol(s, &end, 10);
    if (errno != 0 || end == s || *end != '\0' || v <= 0) {
        std::fprintf(stderr, "pcie_copy: bad %s: %s\n", what, s);
        std::exit(2);
    }
    return v;
}

// run_phase copies `bytes` repeatedly in direction `kind` until `seconds` of
// wall time have passed, timing the whole phase with CUDA events.
static void run_phase(const char *name, void *dst, const void *src,
                      size_t bytes, double seconds, cudaMemcpyKind kind,
                      cudaStream_t stream) {
    cudaEvent_t ev_start, ev_stop;
    CHECK(cudaEventCreate(&ev_start));
    CHECK(cudaEventCreate(&ev_stop));

    // One untimed copy so first-touch and setup costs stay out of the rate.
    CHECK(cudaMemcpyAsync(dst, src, bytes, kind, stream));
    CHECK(cudaStreamSynchronize(stream));

    long copies = 0;
    double wall_start = wall_now();
    double t0 = mono_now();
    CHECK(cudaEventRecord(ev_start, stream));
    do {
        CHECK(cudaMemcpyAsync(dst, src, bytes, kind, stream));
        CHECK(cudaStreamSynchronize(stream));
        copies++;
    } while (mono_now() - t0 < seconds);
    CHECK(cudaEventRecord(ev_stop, stream));
    CHECK(cudaEventSynchronize(ev_stop));
    double wall_end = wall_now();

    float ms = 0.0f;
    CHECK(cudaEventElapsedTime(&ms, ev_start, ev_stop));
    double secs = (double)ms / 1e3;
    double total = (double)bytes * (double)copies;
    std::printf("phase=%s bytes=%.0f seconds=%.6f gbps=%.3f copies=%ld "
                "start=%.6f end=%.6f\n",
                name, total, secs, total / secs / 1e9, copies, wall_start,
                wall_end);
    std::fflush(stdout);

    CHECK(cudaEventDestroy(ev_start));
    CHECK(cudaEventDestroy(ev_stop));
}

int main(int argc, char **argv) {
    long size_mib = 1024;
    long seconds = 20;
    long device = 0;
    if (argc > 4 || (argc > 1 && (std::strcmp(argv[1], "-h") == 0 ||
                                  std::strcmp(argv[1], "--help") == 0))) {
        std::fprintf(stderr,
                     "usage: pcie_copy [size_mib] [seconds_per_phase] [device]\n");
        return 2;
    }
    if (argc > 1) size_mib = parse_arg(argv[1], "size_mib");
    if (argc > 2) seconds = parse_arg(argv[2], "seconds_per_phase");
    if (argc > 3) {
        device = std::strtol(argv[3], nullptr, 10);
    }
    size_t bytes = (size_t)size_mib << 20;

    CHECK(cudaSetDevice((int)device));
    cudaDeviceProp prop;
    CHECK(cudaGetDeviceProperties(&prop, (int)device));
    std::printf("device=%ld name=\"%s\" pci=%04x:%02x:%02x size_mib=%ld "
                "seconds_per_phase=%ld\n",
                device, prop.name, prop.pciDomainID, prop.pciBusID,
                prop.pciDeviceID, size_mib, seconds);
    std::fflush(stdout);

    void *host = nullptr;
    void *dev = nullptr;
    CHECK(cudaMallocHost(&host, bytes));
    CHECK(cudaMalloc(&dev, bytes));
    std::memset(host, 0xA5, bytes);
    CHECK(cudaMemset(dev, 0x5A, bytes));

    cudaStream_t stream;
    CHECK(cudaStreamCreateWithFlags(&stream, cudaStreamNonBlocking));
    CHECK(cudaDeviceSynchronize());

    run_phase("h2d", dev, host, bytes, (double)seconds,
              cudaMemcpyHostToDevice, stream);
    run_phase("d2h", host, dev, bytes, (double)seconds,
              cudaMemcpyDeviceToHost, stream);

    CHECK(cudaStreamDestroy(stream));
    CHECK(cudaFree(dev));
    CHECK(cudaFreeHost(host));
    return 0;
}
