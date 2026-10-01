# The binaries are cross-compiled on the host (see deploy/agent/build.sh). Base:
# distroless "base" (glibc, no shell, non-root), because on real GPUs the agent
# runs the host's nvidia-smi, which the NVIDIA runtime mounts into the
# container and which needs glibc.
FROM gcr.io/distroless/base-debian12:nonroot
ARG TARGETARCH
COPY bin/gpufit-agent-linux-${TARGETARCH} /gpufit-agent
COPY bin/gpufit-controller-linux-${TARGETARCH} /gpufit-controller
# Default: the agent. The controller Deployment sets command: [/gpufit-controller].
ENTRYPOINT ["/gpufit-agent"]
CMD ["run"]
